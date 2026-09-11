package main

import (
	"context"
	"log"
	"net/http"
	"time"

	kgrpc "github.com/go-kratos/kratos/v2/transport/grpc"
	"github.com/google/uuid"
	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/platform/cache"
	platformclient "github.com/panda-dev/panda-v2/backend/platform/client"
	"github.com/panda-dev/panda-v2/backend/platform/config"
	"github.com/panda-dev/panda-v2/backend/platform/database"
	"github.com/panda-dev/panda-v2/backend/platform/database/migrate"
	"github.com/panda-dev/panda-v2/backend/platform/messaging"
	"github.com/panda-dev/panda-v2/backend/platform/registry"
	"github.com/panda-dev/panda-v2/backend/platform/server"
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	casbinpkg "github.com/panda-dev/panda-v2/backend/services/user-service/internal/casbin"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/handler"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/rpc"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
	userv1 "github.com/panda-dev/panda-v2/contracts/proto/user/v1"
	"github.com/panda-dev/panda-v2/migrations"
)

// merchantDialTimeout bounds establishing the shared connection at startup.
const merchantDialTimeout = 5 * time.Second

func main() {
	cfg, err := config.Load("user-service")
	if err != nil {
		log.Fatalf("user-service: load config: %v", err)
	}

	// 提前建连接池，这样可以把 *pgxpool.Pool 注入 repository。
	// USER_DATABASE_URL 是拆库后的身份库，未配置时回落 DATABASE_URL（单库栈仍可跑）。
	ctx := context.Background()
	dbPool, err := database.New(ctx, cfg.ServiceDatabaseURL)
	if err != nil {
		log.Fatalf("user-service: init database: %v", err)
	}
	pgxAdapter, ok := dbPool.(*database.PGXPool)
	if !ok {
		log.Fatalf("user-service: USER_DATABASE_URL (or DATABASE_URL) is required")
	}
	pgxPool := pgxAdapter.Pool()

	// 迁移必须在任何读表动作之前：casbin 加载策略、repository 查表都假定 schema 已就绪。
	// 生产由发布流程在带外执行，这里默认关闭，只由 DB_MIGRATE_ON_START 打开。
	if cfg.MigrateOnStart {
		if err := migrate.Apply(ctx, pgxPool, migrations.Identity); err != nil {
			log.Fatalf("user-service: migrate: %v", err)
		}
	}

	jwtSvc, err := auth.NewService(
		[]byte(cfg.JWTSecret),
		cfg.JWTIssuer,
		15*time.Minute,
		7*24*time.Hour,
	)
	if err != nil {
		log.Fatalf("user-service: init jwt: %v", err)
	}

	// 依赖注入：repository → service → handler
	//
	// recorder 让每个变更操作在业务事务里顺带追加一条审计事件。身份库是
	// admin_operation_logs 的归属方，所以事件由本服务的 outbox 发出、又由本服务
	// 消费落库——消息不跨服务，但路径完整（事务内追加 → relay → MQ → 消费者），
	// 商户服务那条链路用的就是同一条。
	recorder := audit.NewRecorder()
	adminRepo := repository.NewAdminUserRepository(pgxPool, recorder)
	merchantRepo := repository.NewMerchantUserRepository(pgxPool, recorder)
	roleRepo := repository.NewAdminRoleRepository(pgxPool, recorder)
	permRepo := repository.NewAdminPermissionRepository(pgxPool, recorder)
	bindingRepo := repository.NewAdminBindingRepository(pgxPool, recorder)
	menuRepo := repository.NewMenuRepository(pgxPool, recorder)
	operationLogSvc := service.NewOperationLogService(repository.NewAdminOperationLogRepository(pgxPool))

	enforcer, err := casbinpkg.New(pgxPool)
	if err != nil {
		log.Fatalf("user-service: init casbin: %v", err)
	}

	// Redis 只做两件事：广播策略变更、给网关限流提供共享窗口。这里建一次、
	// 交给 runtime 管生命周期（OwnCache），再注入 broadcaster。
	// REDIS_ADDR 为空时退化为 cache.Noop：Reload 仍在本实例生效，只是不广播。
	redisClient, err := cache.New(ctx, cache.Options{
		Addr: cfg.RedisAddress, Password: cfg.RedisPassword, DB: cfg.RedisDB,
	})
	if err != nil {
		log.Fatalf("user-service: init cache: %v", err)
	}
	// 实例标识只用于「跳过自己发出的广播」，所以用随机值而不是注册中心的实例 ID：
	// 这里唯一的要求是进程间不重复，而随机值不依赖地址配置——GRPC_ADDR 为空时
	// kratos 会自己挑端口，两个副本可能算出同一个注册 ID 而互相跳过通知。
	policy := casbinpkg.NewBroadcaster(enforcer, redisClient, uuid.NewString())

	// 商户主体与资源归属统一走 merchant-service，本地不再保留对应的 repository。
	// 连接拨一次共享给所有调用；registry 为空时 New 返回 Noop，Dial 会退回静态地址
	ownershipTimeout := time.Duration(cfg.MerchantOwnershipTimeoutMS) * time.Millisecond
	reg := registry.New(cfg.RegistryEndpoint)
	conn, err := platformclient.Dial(ctx, "merchant-service", cfg.MerchantGRPCAddress, merchantDialTimeout, reg)
	if err != nil {
		log.Fatalf("user-service: dial merchant-service: %v", err)
	}
	defer conn.Close()
	remote, err := client.NewMerchantGRPCClient(conn, cfg.MerchantInternalToken, ownershipTimeout)
	if err != nil {
		log.Fatalf("user-service: init merchant client: %v", err)
	}
	var merchantAccess service.MerchantAccessPort = remote
	var merchantResources service.MerchantResourceAccess = remote
	adminAuthSvc := service.NewAdminAuthService(adminRepo, bindingRepo, jwtSvc)
	merchantAuthSvc := service.NewMerchantAuthService(merchantRepo, merchantAccess, jwtSvc)
	adminUserSvc := service.NewAdminUserService(adminRepo)
	roleSvc := service.NewAdminRoleService(roleRepo, policy)
	permSvc := service.NewAdminPermissionService(permRepo, policy)
	bindingSvc := service.NewAdminBindingService(bindingRepo, policy)
	menuSvc := service.NewAdminMenuService(menuRepo, roleRepo, bindingRepo)
	merchantAccountSvc := service.NewMerchantAccountService(merchantAccess, merchantRepo, merchantResources)

	adminAuthH := handler.NewAdminAuthHandler(adminAuthSvc)
	merchantAuthH := handler.NewMerchantAuthHandler(merchantAuthSvc)
	adminUserH := handler.NewAdminUserHandler(adminUserSvc)
	roleH := handler.NewAdminRoleHandler(roleSvc)
	permH := handler.NewAdminPermissionHandler(permSvc)
	bindingH := handler.NewAdminBindingHandler(bindingSvc)
	menuH := handler.NewAdminMenuHandler(menuSvc)
	merchantH := handler.NewAdminMerchantHandler(merchantAccountSvc)

	authMW := auth.Middleware(jwtSvc)
	// permMW 组合 JWT 认证 + Casbin 鉴权，参数为权限码
	permMW := func(code string) func(http.Handler) http.Handler {
		casbinMW := handler.RequirePermission(enforcer, code)
		return func(next http.Handler) http.Handler {
			return authMW(casbinMW(next))
		}
	}

	if err := server.RunWithOptions(cfg, runtime.Options{
		Database:    dbPool,
		OwnDatabase: true,
		Registry:    reg,
		OwnRegistry: true,
		// 自己建、交给 runtime 管：订阅循环（Worker）需要同一个客户端。
		Cache:    redisClient,
		OwnCache: true,
		// 策略变更订阅循环随服务启停；关掉 Redis 时它只是空转等待 ctx 结束。
		Workers: []runtime.Runner{policy},
		// Outbox 与 ConsumerInbox 都由 runtime 接线：给了 outbox 就起 relay，
		// 给了 inbox 就给消费者套上幂等。RABBITMQ_URL 为空时两者都退化为 Noop，
		// 服务照常启动，事件只是堆在 outbox 里等 MQ 回来。
		Outbox:        messaging.NewPostgreSQL(pgxPool),
		ConsumerInbox: messaging.NewPostgreSQL(pgxPool),
		ConsumerHandler: func(ctx context.Context, event messaging.Envelope) error {
			return operationLogSvc.Handle(ctx, event)
		},
		GRPCRoutes: func(s *kgrpc.Server) {
			// 两个服务共用 MERCHANT_INTERNAL_TOKEN：merchant-service 用服务 token 调
			// HasUsers/ResetAccountScope，网关转发管理员 access token 调 GetAdminAccess
			userv1.RegisterUserServiceServer(s, rpc.NewUserServiceServer(merchantRepo))
			userv1.RegisterAdminAccessServiceServer(s, rpc.NewAdminAccessServiceServer(adminAuthSvc))
		},
		GRPCServerOptions: []kgrpc.ServerOption{auth.GRPCServerOption(jwtSvc, cfg.MerchantInternalToken)},
		HTTPRoutes: func(s *runtime.HTTPRouter) {
			// 认证（不需要 Casbin，只需要 JWT）
			s.HandleFunc("/v1/admin/auth/login", methodOnly(http.MethodPost, adminAuthH.Login))
			s.HandleFunc("/v1/admin/auth/logout", methodOnly(http.MethodPost, adminAuthH.Logout))
			s.HandleFunc("/v1/admin/auth/refresh", methodOnly(http.MethodPost, adminAuthH.Refresh))
			s.HandleFunc("/v1/admin/users/me", withMiddleware(authMW, adminAuthH.Me))
			s.HandleFunc("/v1/merchant/auth/login", methodOnly(http.MethodPost, merchantAuthH.Login))
			s.HandleFunc("/v1/merchant/users/me", withMiddleware(authMW, merchantAuthH.Me))

			// 管理员用户管理
			s.HandleFunc("/v1/admin/users", withMiddleware(authMW, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					withMiddleware(permMW("admin:users:view"), adminUserH.List)(w, r)
				case http.MethodPost:
					withMiddleware(permMW("admin:users:manage"), adminUserH.Create)(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			s.HandleFunc("/v1/admin/users/{id}", withMiddleware(authMW, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					withMiddleware(permMW("admin:users:view"), adminUserH.Get)(w, r)
				} else {
					http.NotFound(w, r)
				}
			}))
			s.HandleFunc("/v1/admin/users/{id}/status",
				withMiddleware(authMW, withMiddleware(permMW("admin:users:manage"),
					methodOnly(http.MethodPatch, adminUserH.UpdateStatus))))

			// 角色管理
			s.HandleFunc("/v1/admin/roles", withMiddleware(authMW, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					withMiddleware(permMW("admin:roles:view"), roleH.List)(w, r)
				case http.MethodPost:
					withMiddleware(permMW("admin:roles:manage"), roleH.Create)(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			s.HandleFunc("/v1/admin/roles/{id}", withMiddleware(authMW, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					withMiddleware(permMW("admin:roles:view"), roleH.Get)(w, r)
				case http.MethodPut:
					withMiddleware(permMW("admin:roles:manage"), roleH.Update)(w, r)
				case http.MethodDelete:
					withMiddleware(permMW("admin:roles:delete"), roleH.Delete)(w, r)
				default:
					http.NotFound(w, r)
				}
			}))

			// 权限管理
			s.HandleFunc("/v1/admin/permissions", withMiddleware(authMW, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					withMiddleware(permMW("admin:permissions:view"), permH.List)(w, r)
				case http.MethodPost:
					withMiddleware(permMW("admin:permissions:manage"), permH.Create)(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			s.HandleFunc("/v1/admin/permissions/{id}", withMiddleware(authMW, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					withMiddleware(permMW("admin:permissions:view"), permH.Get)(w, r)
				case http.MethodPut:
					withMiddleware(permMW("admin:permissions:manage"), permH.Update)(w, r)
				case http.MethodDelete:
					withMiddleware(permMW("admin:permissions:delete"), permH.Delete)(w, r)
				default:
					http.NotFound(w, r)
				}
			}))

			// 角色-权限绑定
			s.HandleFunc("/v1/admin/roles/{roleId}/permissions", withMiddleware(authMW, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					withMiddleware(permMW("admin:bindings:view"), permH.ListByRole)(w, r)
				case http.MethodPost:
					withMiddleware(permMW("admin:bindings:manage"), bindingH.AssignPermissions)(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			s.HandleFunc("/v1/admin/roles/{roleId}/permissions/{permissionId}",
				withMiddleware(authMW, withMiddleware(permMW("admin:bindings:manage"),
					methodOnly(http.MethodDelete, bindingH.RemovePermission))))

			// 用户-角色绑定
			s.HandleFunc("/v1/admin/users/{userId}/roles", withMiddleware(authMW, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					withMiddleware(permMW("admin:bindings:view"), bindingH.ListRoles)(w, r)
				case http.MethodPost:
					withMiddleware(permMW("admin:bindings:manage"), bindingH.AssignRoles)(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			s.HandleFunc("/v1/admin/users/{userId}/roles/{roleId}",
				withMiddleware(authMW, withMiddleware(permMW("admin:bindings:manage"),
					methodOnly(http.MethodDelete, bindingH.RemoveRole))))

			// 菜单管理
			s.HandleFunc("/v1/admin/menus/me",
				withMiddleware(authMW, methodOnly(http.MethodGet, menuH.MeTree)))
			s.HandleFunc("/v1/admin/menus", withMiddleware(authMW, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					withMiddleware(permMW("admin:menus:view"), menuH.List)(w, r)
				case http.MethodPost:
					withMiddleware(permMW("admin:menus:manage"), menuH.Create)(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			s.HandleFunc("/v1/admin/menus/{id}", withMiddleware(authMW, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodPut:
					withMiddleware(permMW("admin:menus:manage"), menuH.Update)(w, r)
				case http.MethodDelete:
					withMiddleware(permMW("admin:menus:delete"), menuH.Delete)(w, r)
				default:
					http.NotFound(w, r)
				}
			}))

			// 角色-菜单绑定
			s.HandleFunc("/v1/admin/roles/{roleId}/menus", withMiddleware(authMW, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					withMiddleware(permMW("admin:bindings:view"), menuH.RoleMenus)(w, r)
				case http.MethodPost:
					withMiddleware(permMW("admin:bindings:manage"), menuH.AssignRoleMenus)(w, r)
				default:
					http.NotFound(w, r)
				}
			}))

			// 商户账号管理
			s.HandleFunc("/v1/admin/merchants/{id}/users", withMiddleware(authMW, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					withMiddleware(permMW("admin:merchants:view"), merchantH.ListUsers)(w, r)
				case http.MethodPost:
					withMiddleware(permMW("admin:merchants:manage"), merchantH.CreateUser)(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			s.HandleFunc("/v1/admin/merchant-users/{id}/status",
				withMiddleware(authMW, withMiddleware(permMW("admin:merchants:manage"),
					methodOnly(http.MethodPatch, merchantH.UpdateUserStatus))))
			s.HandleFunc("/v1/admin/merchant-users/{id}/scope",
				withMiddleware(authMW, withMiddleware(permMW("admin:merchants:manage"),
					methodOnly(http.MethodPatch, merchantH.UpdateUserScope))))
			s.HandleFunc("/v1/admin/merchant-users/{id}",
				withMiddleware(authMW, withMiddleware(permMW("admin:merchants:delete"),
					methodOnly(http.MethodDelete, merchantH.DeleteUser))))
		},
	}); err != nil {
		log.Fatalf("user-service: %v", err)
	}
}

// methodOnly 拒绝非指定方法的请求
func methodOnly(method string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}
}

// withMiddleware 将中间件应用到 HandlerFunc
func withMiddleware(mw func(http.Handler) http.Handler, h http.HandlerFunc) http.HandlerFunc {
	return mw(h).ServeHTTP
}

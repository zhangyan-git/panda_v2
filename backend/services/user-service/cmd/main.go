package main

import (
	"context"
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
	"log"
	"log/slog"
	"net/http"
	"time"
)

// merchantDialTimeout bounds establishing the shared connection at startup.
const merchantDialTimeout = 5 * time.Second

// wechatCallTimeout 是一次微信接口调用的预算。登录请求压着用户在等，
// 超过这个时间还没回来就该按「微信不可用」报错让他重试，而不是继续挂着。
const wechatCallTimeout = 5 * time.Second

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

	// access token 24 小时：管理端要求「登录一次管一天」，不再让操作到一半被踢回
	// 登录页。代价是这枚 token 泄露后 24 小时内可直接使用，且它携带的权限声明在
	// 过期前不会更新（改权限要等 token 换新）。三处 auth.NewService 的取值必须一致。
	jwtSvc, err := auth.NewService(
		[]byte(cfg.JWTSecret),
		cfg.JWTIssuer,
		24*time.Hour,
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
	// 一个仓库实例服两个入口：消费者写、后台页面读。两者共用连接池，也共用
	// 「这张表长什么样」这一份认知，分成两个实例只会多一份重复的构造。
	operationLogRepo := repository.NewAdminOperationLogRepository(pgxPool)
	operationLogSvc := service.NewOperationLogService(operationLogRepo)
	adminOperationLogSvc := service.NewAdminOperationLogService(operationLogRepo)

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

	// 小程序（C 端）链路。C 端用户与 B 端账号同库但完全独立：前者在 users，
	// 后者在 admin_users / merchant_users，互相不认识，也都不外键到对方。
	userRepo := repository.NewUserRepository(pgxPool)
	userSessionRepo := repository.NewUserSessionRepository(pgxPool)
	userSMSCodeRepo := repository.NewSMSCodeRepository(pgxPool)

	// 微信凭据缺失时仍然构造客户端：登不进去的只应该是微信这两条登录路径，
	// 而不是整个服务。Configured() 只为启动时打印一条警告，不参与启动决策。
	wechatClient := client.NewWechatMiniappClient(client.WechatMiniappConfig{
		AppID:   cfg.WechatMiniappAppID,
		Secret:  cfg.WechatMiniappSecret,
		BaseURL: cfg.WechatMiniappAPIBase,
		Timeout: wechatCallTimeout,
	})
	if !wechatClient.Configured() {
		slog.Warn("微信小程序凭据未配置，微信登录将返回 503",
			"app_id", "WECHAT_MINIAPP_APP_ID", "secret", "WECHAT_MINIAPP_APP_SECRET")
	}

	// 短信通道在 V2 里还没有实现（计划归 notification-service），所以默认那一档
	// 是「明确报错」而不是「假装发成功」。开发期要看验证码就显式打开日志发送器，
	// 但它会把验证码原文写进日志，等于把一个能登录任意手机号的凭据留在日志系统里，
	// 因此只允许在 dev 环境打开：配错了宁可不启动，也不要悄悄降级。
	smsSender := service.SmsSender(client.UnavailableSmsSender{})
	if cfg.SmsDevLogCodes {
		if cfg.Environment != "dev" {
			log.Fatalf("user-service: SMS_DEV_LOG_CODES writes raw verification codes to the log and is only accepted when PANDA_ENV=dev (PANDA_ENV=%q)", cfg.Environment)
		}
		slog.Warn("SMS_DEV_LOG_CODES 已开启：短信验证码将写入日志而不是真的发送")
		smsSender = client.LogSmsSender{}
	}

	miniappAuthSvc := service.NewMiniappAuthService(
		userRepo, userSessionRepo, userSMSCodeRepo, wechatClient, smsSender, jwtSvc)
	miniappUserSvc := service.NewMiniappUserService(userRepo, userSMSCodeRepo)
	// 后台那侧单独一个仓库：它要写审计（管理员对别人做的事），而 userRepo 的
	// 写入全是用户对自己的操作，没有 Actor 可填。
	adminMiniappUserSvc := service.NewAdminMiniappUserService(
		userRepo, userSessionRepo, repository.NewAdminMiniappUserRepository(pgxPool, recorder))

	adminAuthH := handler.NewAdminAuthHandler(adminAuthSvc)
	merchantAuthH := handler.NewMerchantAuthHandler(merchantAuthSvc)
	adminUserH := handler.NewAdminUserHandler(adminUserSvc)
	roleH := handler.NewAdminRoleHandler(roleSvc)
	permH := handler.NewAdminPermissionHandler(permSvc)
	bindingH := handler.NewAdminBindingHandler(bindingSvc)
	menuH := handler.NewAdminMenuHandler(menuSvc)
	merchantH := handler.NewAdminMerchantHandler(merchantAccountSvc)
	miniappAuthH := handler.NewMiniappAuthHandler(miniappAuthSvc)
	miniappUserH := handler.NewMiniappUserHandler(miniappUserSvc)
	adminMiniappUserH := handler.NewAdminMiniappUserHandler(adminMiniappUserSvc)
	adminOperationLogH := handler.NewAdminOperationLogHandler(adminOperationLogSvc)

	authMW := auth.Middleware(jwtSvc)
	// permMW 组合 JWT 认证 + 鉴权，参数为权限码。
	// 鉴权里超管判定读的是 bindingRepo 的实时角色绑定、账号是否可用读的是 adminRepo，
	// 都不是 token 里的快照：后者在过期前不会变，用它就等于「降级超管」和「停用账号」
	// 要等最长 24 小时才生效。
	permMW := func(code string) func(http.Handler) http.Handler {
		casbinMW := handler.RequirePermission(enforcer, bindingRepo, adminRepo, code)
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

			// 小程序（C 端）
			//
			// 登录这一组不需要 authMW：验证码和 refresh token 就是凭据本身，
			// 拿不出 access token 才来调它们。它们的防护是网关限流 + 验证码本身的
			// 重发间隔与失败次数上限。
			s.HandleFunc("/v1/miniapp/auth/login", methodOnly(http.MethodPost, miniappAuthH.Login))
			s.HandleFunc("/v1/miniapp/auth/sms", methodOnly(http.MethodPost, miniappAuthH.SendSMS))
			s.HandleFunc("/v1/miniapp/auth/refresh", methodOnly(http.MethodPost, miniappAuthH.Refresh))
			// 退出登录同样不需要 access token：令牌过期时最需要退出。
			s.HandleFunc("/v1/miniapp/auth/logout", methodOnly(http.MethodPost, miniappAuthH.Logout))

			// 资料这一组必须过 authMW —— 不是因为它做了鉴权（它只解析令牌、
			// 不看 realm），而是因为身份是 authMW 放进 request context 的，
			// 少了它 requireConsumer 永远取不到身份，接口会对所有人回 401。
			// realm 的判定在 requireConsumer 里，那是 C 端和 B 端的分界线。
			s.HandleFunc("/v1/miniapp/users/me", withMiddleware(authMW, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					miniappUserH.Me(w, r)
				case http.MethodPatch:
					miniappUserH.UpdateMe(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			s.HandleFunc("/v1/miniapp/users/me/phone",
				withMiddleware(authMW, methodOnly(http.MethodPost, miniappUserH.BindPhone)))

			// 小程序用户管理（后台）。路径刻意不复用 /v1/admin/users：那条是平台
			// 管理员（admin_users），两批人从权限码到数据表都不一样。
			s.HandleFunc("/v1/admin/miniapp-users", withMiddleware(
				permMW("admin:miniapp-users:view"),
				methodOnly(http.MethodGet, adminMiniappUserH.List)))
			s.HandleFunc("/v1/admin/miniapp-users/{id}", withMiddleware(
				permMW("admin:miniapp-users:view"),
				methodOnly(http.MethodGet, adminMiniappUserH.Get)))
			// 禁用会在同一事务里撤销该用户全部会话，见 repository/admin_miniapp_user.go。
			s.HandleFunc("/v1/admin/miniapp-users/{id}/status", withMiddleware(
				permMW("admin:miniapp-users:manage"),
				methodOnly(http.MethodPatch, adminMiniappUserH.UpdateStatus)))

			// 操作日志（只读）。没有 manage 权限码：这张表是审计证据，后台能做的
			// 只有查，删除与清空不提供入口，所以视图码就是全部。
			// /facets 是更具体的路径，Go 1.22 的 mux 会优先匹配它，不会被上面
			// 那条精确路径抢走。
			s.HandleFunc("/v1/admin/operation-logs", withMiddleware(
				permMW("admin:operation-logs:view"),
				methodOnly(http.MethodGet, adminOperationLogH.List)))
			s.HandleFunc("/v1/admin/operation-logs/facets", withMiddleware(
				permMW("admin:operation-logs:view"),
				methodOnly(http.MethodGet, adminOperationLogH.Facets)))

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

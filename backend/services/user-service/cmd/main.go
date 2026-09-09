package main

import (
	"context"
	"log"
	"net/http"
	"time"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/platform/config"
	"github.com/panda-dev/panda-v2/backend/platform/database"
	"github.com/panda-dev/panda-v2/backend/platform/server"
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	casbinpkg "github.com/panda-dev/panda-v2/backend/services/user-service/internal/casbin"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/handler"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
)

func main() {
	cfg, err := config.Load("user-service")
	if err != nil {
		log.Fatalf("user-service: load config: %v", err)
	}

	// 提前建连接池，这样可以把 *pgxpool.Pool 注入 repository
	ctx := context.Background()
	dbPool, err := database.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("user-service: init database: %v", err)
	}
	pgxAdapter, ok := dbPool.(*database.PGXPool)
	if !ok {
		log.Fatalf("user-service: DATABASE_URL is required")
	}
	pgxPool := pgxAdapter.Pool()

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
	adminRepo := repository.NewAdminUserRepository(pgxPool)
	merchantRepo := repository.NewMerchantUserRepository(pgxPool)
	merchantsRepo := repository.NewMerchantRepository(pgxPool)
	roleRepo := repository.NewAdminRoleRepository(pgxPool)
	permRepo := repository.NewAdminPermissionRepository(pgxPool)
	bindingRepo := repository.NewAdminBindingRepository(pgxPool)
	menuRepo := repository.NewMenuRepository(pgxPool)
	brandsRepo := repository.NewBrandRepository(pgxPool)
	storesRepo := repository.NewStoreRepository(pgxPool)
	brandAuditRepo := repository.NewBrandAuditRepository(pgxPool)
	storeAuditRepo := repository.NewStoreAuditRepository(pgxPool)

	enforcer, err := casbinpkg.New(pgxPool)
	if err != nil {
		log.Fatalf("user-service: init casbin: %v", err)
	}

	merchantAccess := service.NewRepositoryMerchantAccess(merchantsRepo)
	adminAuthSvc := service.NewAdminAuthService(adminRepo, bindingRepo, jwtSvc)
	merchantAuthSvc := service.NewMerchantAuthService(merchantRepo, merchantAccess, jwtSvc)
	adminUserSvc := service.NewAdminUserService(adminRepo)
	roleSvc := service.NewAdminRoleService(roleRepo)
	permSvc := service.NewAdminPermissionService(permRepo)
	bindingSvc := service.NewAdminBindingService(bindingRepo, enforcer)
	menuSvc := service.NewAdminMenuService(menuRepo, roleRepo, bindingRepo)
	merchantSvc := service.NewAdminMerchantService(merchantsRepo, merchantRepo, brandsRepo, storesRepo)
	brandSvc := service.NewAdminBrandService(brandsRepo, merchantsRepo, brandAuditRepo, merchantRepo)
	storeSvc := service.NewAdminStoreService(storesRepo, brandsRepo, merchantsRepo, storeAuditRepo, merchantRepo)

	adminAuthH := handler.NewAdminAuthHandler(adminAuthSvc)
	merchantAuthH := handler.NewMerchantAuthHandler(merchantAuthSvc)
	adminUserH := handler.NewAdminUserHandler(adminUserSvc)
	roleH := handler.NewAdminRoleHandler(roleSvc)
	permH := handler.NewAdminPermissionHandler(permSvc)
	bindingH := handler.NewAdminBindingHandler(bindingSvc)
	menuH := handler.NewAdminMenuHandler(menuSvc)
	merchantH := handler.NewAdminMerchantHandler(merchantSvc)
	brandH := handler.NewAdminBrandHandler(brandSvc)
	storeH := handler.NewAdminStoreHandler(storeSvc)

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
		HTTPRoutes: func(s *khttp.Server) {
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
					withMiddleware(permMW("admin:users:read"), adminUserH.List)(w, r)
				case http.MethodPost:
					withMiddleware(permMW("admin:users:write"), adminUserH.Create)(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			s.HandleFunc("/v1/admin/users/{id}", withMiddleware(authMW, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					withMiddleware(permMW("admin:users:read"), adminUserH.Get)(w, r)
				} else {
					http.NotFound(w, r)
				}
			}))
			s.HandleFunc("/v1/admin/users/{id}/status",
				withMiddleware(authMW, withMiddleware(permMW("admin:users:write"),
					methodOnly(http.MethodPatch, adminUserH.UpdateStatus))))

			// 角色管理
			s.HandleFunc("/v1/admin/roles", withMiddleware(authMW, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					withMiddleware(permMW("admin:roles:read"), roleH.List)(w, r)
				case http.MethodPost:
					withMiddleware(permMW("admin:roles:write"), roleH.Create)(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			s.HandleFunc("/v1/admin/roles/{id}", withMiddleware(authMW, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					withMiddleware(permMW("admin:roles:read"), roleH.Get)(w, r)
				case http.MethodPut:
					withMiddleware(permMW("admin:roles:write"), roleH.Update)(w, r)
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
					withMiddleware(permMW("admin:permissions:read"), permH.List)(w, r)
				case http.MethodPost:
					withMiddleware(permMW("admin:permissions:write"), permH.Create)(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			s.HandleFunc("/v1/admin/permissions/{id}", withMiddleware(authMW, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					withMiddleware(permMW("admin:permissions:read"), permH.Get)(w, r)
				case http.MethodPut:
					withMiddleware(permMW("admin:permissions:write"), permH.Update)(w, r)
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
					withMiddleware(permMW("admin:bindings:read"), permH.ListByRole)(w, r)
				case http.MethodPost:
					withMiddleware(permMW("admin:bindings:write"), bindingH.AssignPermissions)(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			s.HandleFunc("/v1/admin/roles/{roleId}/permissions/{permissionId}",
				withMiddleware(authMW, withMiddleware(permMW("admin:bindings:write"),
					methodOnly(http.MethodDelete, bindingH.RemovePermission))))

			// 用户-角色绑定
			s.HandleFunc("/v1/admin/users/{userId}/roles", withMiddleware(authMW, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					withMiddleware(permMW("admin:bindings:read"), bindingH.ListRoles)(w, r)
				case http.MethodPost:
					withMiddleware(permMW("admin:bindings:write"), bindingH.AssignRoles)(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			s.HandleFunc("/v1/admin/users/{userId}/roles/{roleId}",
				withMiddleware(authMW, withMiddleware(permMW("admin:bindings:write"),
					methodOnly(http.MethodDelete, bindingH.RemoveRole))))

			// 菜单管理
			s.HandleFunc("/v1/admin/menus/me",
				withMiddleware(authMW, methodOnly(http.MethodGet, menuH.MeTree)))
			s.HandleFunc("/v1/admin/menus", withMiddleware(authMW, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					withMiddleware(permMW("admin:menus:read"), menuH.List)(w, r)
				case http.MethodPost:
					withMiddleware(permMW("admin:menus:write"), menuH.Create)(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			s.HandleFunc("/v1/admin/menus/{id}", withMiddleware(authMW, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodPut:
					withMiddleware(permMW("admin:menus:write"), menuH.Update)(w, r)
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
					withMiddleware(permMW("admin:bindings:read"), menuH.RoleMenus)(w, r)
				case http.MethodPost:
					withMiddleware(permMW("admin:bindings:write"), menuH.AssignRoleMenus)(w, r)
				default:
					http.NotFound(w, r)
				}
			}))

			// 商户管理
			s.HandleFunc("/v1/admin/merchants", withMiddleware(authMW, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					withMiddleware(permMW("admin:merchants:read"), merchantH.List)(w, r)
				case http.MethodPost:
					withMiddleware(permMW("admin:merchants:write"), merchantH.Create)(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			s.HandleFunc("/v1/admin/merchants/{id}", withMiddleware(authMW, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					withMiddleware(permMW("admin:merchants:read"), merchantH.Get)(w, r)
				case http.MethodPut:
					withMiddleware(permMW("admin:merchants:write"), merchantH.Update)(w, r)
				case http.MethodDelete:
					withMiddleware(permMW("admin:merchants:delete"), merchantH.Delete)(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			s.HandleFunc("/v1/admin/merchants/{id}/status",
				withMiddleware(authMW, withMiddleware(permMW("admin:merchants:write"),
					methodOnly(http.MethodPatch, merchantH.UpdateStatus))))
			s.HandleFunc("/v1/admin/merchants/{id}/users", withMiddleware(authMW, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					withMiddleware(permMW("admin:merchants:read"), merchantH.ListUsers)(w, r)
				case http.MethodPost:
					withMiddleware(permMW("admin:merchants:write"), merchantH.CreateUser)(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			s.HandleFunc("/v1/admin/merchant-users/{id}/status",
				withMiddleware(authMW, withMiddleware(permMW("admin:merchants:write"),
					methodOnly(http.MethodPatch, merchantH.UpdateUserStatus))))
			s.HandleFunc("/v1/admin/merchant-users/{id}/scope",
				withMiddleware(authMW, withMiddleware(permMW("admin:merchants:write"),
					methodOnly(http.MethodPatch, merchantH.UpdateUserScope))))
			s.HandleFunc("/v1/admin/merchant-users/{id}",
				withMiddleware(authMW, withMiddleware(permMW("admin:merchants:delete"),
					methodOnly(http.MethodDelete, merchantH.DeleteUser))))

			// 品牌管理
			s.HandleFunc("/v1/admin/brands", withMiddleware(authMW, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					withMiddleware(permMW("admin:brands:read"), brandH.List)(w, r)
				case http.MethodPost:
					withMiddleware(permMW("admin:brands:write"), brandH.Create)(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			s.HandleFunc("/v1/admin/brands/{id}", withMiddleware(authMW, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					withMiddleware(permMW("admin:brands:read"), brandH.Get)(w, r)
				case http.MethodPut:
					withMiddleware(permMW("admin:brands:write"), brandH.Update)(w, r)
				case http.MethodDelete:
					withMiddleware(permMW("admin:brands:delete"), brandH.Delete)(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			s.HandleFunc("/v1/admin/brands/{id}/status",
				withMiddleware(authMW, withMiddleware(permMW("admin:brands:write"),
					methodOnly(http.MethodPatch, brandH.UpdateStatus))))
			s.HandleFunc("/v1/admin/brands/{id}/audit",
				withMiddleware(authMW, withMiddleware(permMW("admin:brands:write"),
					methodOnly(http.MethodPatch, brandH.Audit))))

			// 门店管理
			s.HandleFunc("/v1/admin/stores", withMiddleware(authMW, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					withMiddleware(permMW("admin:stores:read"), storeH.List)(w, r)
				case http.MethodPost:
					withMiddleware(permMW("admin:stores:write"), storeH.Create)(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			s.HandleFunc("/v1/admin/stores/{id}", withMiddleware(authMW, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					withMiddleware(permMW("admin:stores:read"), storeH.Get)(w, r)
				case http.MethodPut:
					withMiddleware(permMW("admin:stores:write"), storeH.Update)(w, r)
				case http.MethodDelete:
					withMiddleware(permMW("admin:stores:delete"), storeH.Delete)(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			s.HandleFunc("/v1/admin/stores/{id}/status",
				withMiddleware(authMW, withMiddleware(permMW("admin:stores:write"),
					methodOnly(http.MethodPatch, storeH.UpdateStatus))))
			s.HandleFunc("/v1/admin/stores/{id}/audit",
				withMiddleware(authMW, withMiddleware(permMW("admin:stores:write"),
					methodOnly(http.MethodPatch, storeH.Audit))))
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

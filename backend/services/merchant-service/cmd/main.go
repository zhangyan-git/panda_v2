package main

import (
	"context"
	kgrpc "github.com/go-kratos/kratos/v2/transport/grpc"
	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	platformclient "github.com/panda-dev/panda-v2/backend/platform/client"
	"github.com/panda-dev/panda-v2/backend/platform/config"
	"github.com/panda-dev/panda-v2/backend/platform/database"
	"github.com/panda-dev/panda-v2/backend/platform/database/migrate"
	"github.com/panda-dev/panda-v2/backend/platform/messaging"
	"github.com/panda-dev/panda-v2/backend/platform/registry"
	"github.com/panda-dev/panda-v2/backend/platform/server"
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/platform/upload"
	userclient "github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/handler"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/rpc"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/service"
	merchantv1 "github.com/panda-dev/panda-v2/contracts/proto/merchant/v1"
	userv1 "github.com/panda-dev/panda-v2/contracts/proto/user/v1"
	"github.com/panda-dev/panda-v2/migrations"
	"log"
	"time"
)

const (
	// userServiceDialTimeout bounds establishing the shared connection at startup.
	userServiceDialTimeout = 5 * time.Second
	// authorizationTimeout bounds one live authorization lookup. It is not a cache
	// TTL: every admin request re-checks its grants.
	authorizationTimeout = 5 * time.Second
)

func main() {
	cfg, err := config.Load("merchant-service")
	if err != nil {
		log.Fatalf("merchant-service: load config: %v", err)
	}

	// MERCHANT_DATABASE_URL is the merchant database after the split, falling
	// back to DATABASE_URL on a single-database stack.
	db, err := database.New(context.Background(), cfg.ServiceDatabaseURL)
	if err != nil {
		log.Fatalf("merchant-service: init database: %v", err)
	}
	defer db.Close()

	// One registry and one dialed connection serve every user-service call. The
	// registry falls back to the static address until discovery is real.
	reg := registry.New(cfg.RegistryEndpoint)
	conn, err := platformclient.Dial(context.Background(), "user-service", cfg.UserGRPCAddress, userServiceDialTimeout, reg)
	if err != nil {
		log.Fatalf("merchant-service: dial user-service: %v", err)
	}
	defer conn.Close()
	users := userclient.NewUserServiceClient(conn, cfg.MerchantInternalToken)

	pool, ok := db.(*database.PGXPool)
	if !ok {
		log.Fatal("merchant-service: database does not support admin API")
	}
	// Migrate before anything reads the schema; repositories and the gRPC server
	// all assume the tables already exist. Releases migrate out of band, so this
	// is off unless DB_MIGRATE_ON_START says otherwise.
	if cfg.MigrateOnStart {
		if err := migrate.Apply(context.Background(), pool.Pool(), migrations.Merchant); err != nil {
			log.Fatalf("merchant-service: migrate: %v", err)
		}
	}
	// 商户库发、身份库收：审计事件由本服务的 outbox 追加、经 relay 投到 RabbitMQ，
	// user-service 的消费者把它写进身份库的 admin_operation_logs（审计由身份侧拥有）。
	// 本服务不消费，所以只注入 Outbox，不注入 ConsumerInbox。
	recorder := audit.NewRecorder()
	merchantRepo := repository.NewMerchantRepository(pool.Pool(), recorder)
	brandRepo := repository.NewBrandRepository(pool.Pool(), recorder)
	storeRepo := repository.NewStoreRepository(pool.Pool(), recorder)
	adminMerchantService := service.NewAdminMerchantService(merchantRepo, users)
	adminMerchant := handler.NewAdminMerchantHandler(adminMerchantService)
	adminBrand := handler.NewAdminBrandHandler(service.NewAdminBrandService(brandRepo, merchantRepo, repository.NewBrandAuditRepository(pool.Pool()), users))
	adminStore := handler.NewAdminStoreHandler(service.NewAdminStoreService(storeRepo, brandRepo, merchantRepo, repository.NewStoreAuditRepository(pool.Pool()), users))
	access := service.NewMerchantAccessService(merchantRepo, brandRepo, storeRepo)

	// access token 24 小时：管理端要求「登录一次管一天」，不再让操作到一半被踢回
	// 登录页。代价是这枚 token 泄露后 24 小时内可直接使用，且它携带的权限声明在
	// 过期前不会更新（改权限要等 token 换新）。三处 auth.NewService 的取值必须一致。
	jwtService, err := auth.NewService([]byte(cfg.JWTSecret), cfg.JWTIssuer, 24*time.Hour, 7*24*time.Hour)
	if err != nil {
		log.Fatalf("merchant-service: init jwt: %v", err)
	}

	authorizer, err := handler.NewAdminAuthorizer(userv1.NewAdminAccessServiceClient(conn), authorizationTimeout)
	if err != nil {
		log.Fatalf("merchant-service: init live authorization: %v", err)
	}

	// 缺 OSS 配置是一个自洽的状态，不是启动失败：上传接口照常注册，只是每个
	// 请求都回 503 并点名缺哪个变量，其余接口完全不受影响。所以这里记一条日志、
	// 把原因交给处理器，而不是 log.Fatalf。
	uploader, uploadErr := upload.New(uploadConfig(cfg))
	if uploadErr != nil {
		log.Printf("merchant-service: image upload is disabled: %v", uploadErr)
	}
	adminUpload := handler.NewAdminUploadHandler(uploader, uploadErr)

	if err := server.RunWithOptions(cfg, runtime.Options{
		Database:    db,
		OwnDatabase: false,
		Registry:    reg,
		OwnRegistry: true,
		// 只给 outbox：供给它，runtime 才会起 relay 把审计事件投出去。
		Outbox: messaging.NewPostgreSQL(pool.Pool()),
		HTTPRoutes: func(s *runtime.HTTPRouter) {
			handler.Register(s, adminMerchant, adminBrand, adminStore, adminUpload, jwtService, authorizer)
		},
		GRPCRoutes: func(s *kgrpc.Server) {
			merchantv1.RegisterMerchantServiceServer(s, rpc.NewMerchantService(adminMerchantService, access))
		},
		// Both directions share MERCHANT_INTERNAL_TOKEN as the service token.
		GRPCServerOptions: []kgrpc.ServerOption{auth.GRPCServerOption(jwtService, cfg.MerchantInternalToken)},
	}); err != nil {
		log.Fatalf("merchant-service: %v", err)
	}
}

// uploadConfig 把 platform/config 的散字段收成上传包的配置。
//
// 这一层组装只能放在这里：platform/config 不能 import platform/upload——那会让
// OSS SDK 经 platform/server 进入每一个服务的 go.sum，而不只是真正上传的那一个。
func uploadConfig(cfg config.Config) upload.Config {
	return upload.Config{
		AccessKey:   cfg.OSSAccessKey,
		SecretKey:   cfg.OSSSecretKey,
		Endpoint:    cfg.OSSEndpoint,
		Bucket:      cfg.OSSBucket,
		CNAME:       cfg.OSSCNAME,
		Prefix:      cfg.UploadPath,
		MaxFileSize: cfg.UploadMaxFileSize,
		UseMD5:      cfg.UploadUseMD5,
	}
}

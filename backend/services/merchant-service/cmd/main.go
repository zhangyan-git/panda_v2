package main

import (
	"context"
	"log"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/panda-dev/panda-v2/backend/platform/config"
	"github.com/panda-dev/panda-v2/backend/platform/database"
	"github.com/panda-dev/panda-v2/backend/platform/server"
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/handler"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/service"
)

func main() {
	cfg, err := config.Load("merchant-service")
	if err != nil {
		log.Fatalf("merchant-service: load config: %v", err)
	}

	db, err := database.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("merchant-service: init database: %v", err)
	}
	defer db.Close()

	var repo repository.Repository
	var legacy *handler.LegacyHandler
	var adminMerchant *handler.AdminMerchantHandler
	var adminBrand *handler.AdminBrandHandler
	var adminStore *handler.AdminStoreHandler
	if pgx, ok := db.(*database.PGXPool); ok {
		pool := pgx.Pool()
		repo = repository.NewLegacyPostgres(pool)
		merchantRepo := repository.NewMerchantRepository(pool)
		brandRepo := repository.NewBrandRepository(pool)
		storeRepo := repository.NewStoreRepository(pool)
		legacy = handler.NewLegacy(service.New(repo))
		adminMerchant = handler.NewAdminMerchantHandler(service.NewAdminMerchantService(merchantRepo, merchantRepo))
		adminBrand = handler.NewAdminBrandHandler(service.NewAdminBrandService(brandRepo, merchantRepo, repository.NewBrandAuditRepository(pool), repository.NewUnavailableScope()))
		adminStore = handler.NewAdminStoreHandler(service.NewAdminStoreService(storeRepo, brandRepo, merchantRepo, repository.NewStoreAuditRepository(pool), repository.NewUnavailableScope()))
	} else {
		repo = repository.NewUnavailable()
		legacy = handler.NewLegacy(service.New(repo))
	}
	if adminMerchant == nil {
		log.Fatal("merchant-service: database does not support admin API")
	}
	if err := server.RunWithOptions(cfg, runtime.Options{
		Database:    db,
		OwnDatabase: false,
		HTTPRoutes: func(s *khttp.Server) {
			handler.Register(s, adminMerchant, adminBrand, adminStore, legacy)
		},
	}); err != nil {
		log.Fatalf("merchant-service: %v", err)
	}
}

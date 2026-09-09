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
	if pgx, ok := db.(*database.PGXPool); ok {
		repo = repository.NewPostgres(pgx.Pool())
	} else {
		repo = repository.NewUnavailable()
	}
	h := handler.New(service.New(repo))
	if err := server.RunWithOptions(cfg, runtime.Options{
		Database:    db,
		OwnDatabase: false,
		HTTPRoutes:  func(s *khttp.Server) { h.Register(s) },
	}); err != nil {
		log.Fatalf("merchant-service: %v", err)
	}
}

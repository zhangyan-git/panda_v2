package app

import (
	"context"
	"errors"
	"time"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/platform/config"
	"github.com/panda-dev/panda-v2/backend/platform/database"
	"github.com/panda-dev/panda-v2/backend/platform/server"
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/domain"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/handler"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/repository"
)

func Run(service string) error {
	cfg, err := config.Load(service)
	if err != nil {
		return err
	}
	db, err := database.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	var repo domain.Repository = repository.NewMemory()
	var tokenStore handler.Store = handler.NewMemoryStore()
	if pg, ok := db.(*database.PGXPool); ok && pg.Pool() != nil {
		repo = repository.NewPostgreSQL(pg.Pool())
		tokenStore = repository.NewPostgreSQLRefreshTokenStore(pg.Pool())
	}
	return runWithRepository(cfg, db, repo, tokenStore)
}

func RunWithRepository(cfg config.Config, repo domain.Repository) error {
	return runWithRepository(cfg, nil, repo, handler.NewMemoryStore())
}

func runWithRepository(cfg config.Config, db database.Pool, repo domain.Repository, tokenStore handler.Store) error {
	if repo == nil {
		return errors.New("account repository is required")
	}
	if cfg.JWTSecret == "" || cfg.JWTIssuer == "" {
		return errors.New("JWT_SECRET and JWT_ISSUER are required")
	}
	if err := InitializeDevelopmentAccounts(context.Background(), cfg, accountWriter(repo)); err != nil {
		return err
	}
	authorizer, err := auth.NewService([]byte(cfg.JWTSecret), cfg.JWTIssuer, 15*time.Minute, 7*24*time.Hour)
	if err != nil {
		return err
	}
	h := handler.New(domain.NewService(repo), authorizer, tokenStore)
	return server.RunWithOptions(cfg, runtime.Options{Database: db, HTTPRoutes: func(s *khttp.Server) {
		s.HandleFunc("/v1/auth/login", h.Login)
		s.HandleFunc("/v1/auth/refresh", h.Refresh)
		s.HandleFunc("/v1/auth/revoke", h.Revoke)
	}})
}

func accountWriter(repo domain.Repository) domain.AccountWriter {
	writer, _ := repo.(domain.AccountWriter)
	return writer
}

func NewMemoryRepository(accounts ...domain.Account) *repository.Memory {
	return repository.NewMemory(accounts...)
}

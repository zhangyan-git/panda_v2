package app

import (
	"context"
	"errors"
	"strings"
	"time"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/platform/config"
	"github.com/panda-dev/panda-v2/backend/platform/database"
	"github.com/panda-dev/panda-v2/backend/platform/server"
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/controller"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/routes"
	accountservice "github.com/panda-dev/panda-v2/backend/services/account-service/internal/service"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/token"
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

	if _, ok := db.(database.Noop); ok && !isLocalEnvironment(cfg.Environment) {
		return errors.New("DATABASE_URL is required outside dev and test environments")
	}

	var repo model.Repository = repository.NewMemory()
	var tokenStore token.Store = token.NewMemoryStore()
	if pg, ok := db.(*database.PGXPool); ok && pg.Pool() != nil {
		repo = repository.NewPostgreSQL(pg.Pool())
		tokenStore = repository.NewPostgreSQLRefreshTokenStore(pg.Pool())
	}
	return runWithRepository(cfg, db, repo, tokenStore)
}

func RunWithRepository(cfg config.Config, repo model.Repository) error {
	return runWithRepository(cfg, nil, repo, token.NewMemoryStore())
}

func runWithRepository(cfg config.Config, db database.Pool, repo model.Repository, tokenStore token.Store) error {
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
	h := controller.New(accountservice.NewService(repo), authorizer, tokenStore)
	return server.RunWithOptions(cfg, runtime.Options{Database: db, HTTPRoutes: func(s *khttp.Server) {
		routes.Register(s, h)
	}})
}

func isLocalEnvironment(environment string) bool {
	switch strings.ToLower(strings.TrimSpace(environment)) {
	case "dev", "test":
		return true
	default:
		return false
	}
}

func accountWriter(repo model.Repository) model.AccountWriter {
	writer, _ := repo.(model.AccountWriter)
	return writer
}

func NewMemoryRepository(accounts ...model.Account) *repository.Memory {
	return repository.NewMemory(accounts...)
}

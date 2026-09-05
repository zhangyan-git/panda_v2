package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/platform/config"
	"github.com/panda-dev/panda-v2/backend/platform/database"
	"github.com/panda-dev/panda-v2/backend/platform/server"
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/controller"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/routes"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/service"
)

var ErrNoProvider = errors.New("coffee machine worker has no synchronization provider configured")

// RunWorker is the worker process entrypoint. No concrete manufacturer
// provider or worker configuration boundary exists yet, so it fails explicitly
// instead of starting the HTTP API or pretending to run synchronization.
func RunWorker(string) error {
	return ErrNoProvider
}

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
	repo, err := repositoryFor(cfg, db)
	if err != nil {
		return err
	}
	return runWithRepository(cfg, db, repo)
}

func repositoryFor(cfg config.Config, db database.Pool) (model.Repository, error) {
	if pg, ok := db.(*database.PGXPool); ok && pg.Pool() != nil {
		return repository.NewPostgreSQL(pg.Pool()), nil
	}
	if _, ok := db.(database.Noop); ok {
		if strings.EqualFold(cfg.Environment, "dev") || strings.EqualFold(cfg.Environment, "test") {
			return repository.NewMemory(), nil
		}
		return nil, errors.New("DATABASE_URL is required outside dev and test environments")
	}
	return nil, errors.New("coffee machine repository could not be configured")
}

func RunWithRepository(cfg config.Config, r model.Repository) error {
	return runWithRepository(cfg, nil, r)
}

func runWithRepository(cfg config.Config, db database.Pool, r model.Repository) error {
	if r == nil {
		return errors.New("coffee machine repository is required")
	}
	if cfg.JWTSecret == "" || cfg.JWTIssuer == "" {
		return errors.New("JWT_SECRET and JWT_ISSUER are required")
	}
	authorizer, err := auth.NewService([]byte(cfg.JWTSecret), cfg.JWTIssuer, 15*time.Minute, 7*24*time.Hour)
	if err != nil {
		return err
	}
	h := controller.New(service.NewService(r))
	return server.RunWithOptions(cfg, runtime.Options{HTTPRoutes: func(s *khttp.Server) {
		m := http.NewServeMux()
		routes.RegisterRoutes(m, h, authorizer)
		s.HandlePrefix("/v1/", m)
	}, Database: db})
}

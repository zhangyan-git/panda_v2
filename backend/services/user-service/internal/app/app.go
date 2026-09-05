package app

import (
	"errors"
	"net/http"
	"time"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/platform/config"
	"github.com/panda-dev/panda-v2/backend/platform/database"
	"github.com/panda-dev/panda-v2/backend/platform/server"
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/controller"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
)

func Run(service string) error { return server.Run(service) }

func RunWithRepository(cfg config.Config, db database.Pool) error {
	pg, ok := db.(*database.PGXPool)
	if !ok || pg.Pool() == nil {
		return errors.New("user-service requires a PostgreSQL database")
	}
	repo := repository.NewPostgreSQL(pg.Pool())
	if cfg.JWTSecret == "" || cfg.JWTIssuer == "" {
		return errors.New("JWT_SECRET and JWT_ISSUER are required")
	}
	authorizer, err := auth.NewService([]byte(cfg.JWTSecret), cfg.JWTIssuer, 15*time.Minute, 7*24*time.Hour)
	if err != nil {
		return err
	}
	userHandler := controller.New(service.NewService(repo))
	profile := auth.Bearer(authorizer)(http.HandlerFunc(userHandler.Profile))
	return server.RunWithOptions(cfg, runtime.Options{Database: db, HTTPRoutes: func(s *khttp.Server) {
		s.HandleFunc("/users", userHandler.Register)
		s.HandlePrefix("/v1/users/", profile)
	}})
}

package app

import (
	"errors"
	"net/http"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/panda-dev/panda-v2/backend/platform/config"
	"github.com/panda-dev/panda-v2/backend/platform/database"
	"github.com/panda-dev/panda-v2/backend/platform/server"
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/domain"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/handler"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
)

func Run(service string) error {
	return server.Run(service)
}

func RunWithRepository(cfg config.Config, db database.Pool) error {
	pg, ok := db.(*database.PGXPool)
	if !ok || pg.Pool() == nil {
		return errors.New("user-service requires a PostgreSQL database")
	}
	repo := repository.NewPostgreSQL(pg.Pool())
	userHandler := handler.New(domain.NewService(repo))
	return server.RunWithOptions(cfg, runtime.Options{Database: db, HTTPRoutes: func(s *khttp.Server) {
		s.HandleFunc("/users", userHandler.Register)
		s.HandlePrefix("/users/", http.HandlerFunc(userHandler.GetByID))
	}})
}

package routes

import (
	"net/http"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/controller"
)

func Register(s *khttp.Server, h *controller.Handler, authorizer *auth.Service) {
	s.HandleFunc("/users", h.Register)
	profile := auth.Bearer(authorizer)(http.HandlerFunc(h.Profile))
	s.HandlePrefix("/v1/users/", profile)
}

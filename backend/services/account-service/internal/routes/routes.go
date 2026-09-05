package routes

import (
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"net/http"

	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/controller"
)

func writeError(w http.ResponseWriter, status int, message string) {
	api.Error(w, status, api.CodeForStatus(status), message)
}

func Register(mux interface {
	HandleFunc(string, http.HandlerFunc)
	Handle(string, http.Handler)
}, h *controller.Handler) {
	mux.HandleFunc("/v1/auth/login", h.Login)
	mux.HandleFunc("/v1/auth/refresh", h.Refresh)
	mux.HandleFunc("/v1/auth/revoke", h.Revoke)
	mux.Handle("/v1/auth/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/login" && r.URL.Path != "/v1/auth/refresh" && r.URL.Path != "/v1/auth/revoke" {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		switch r.URL.Path {
		case "/v1/auth/login":
			h.Login(w, r)
		case "/v1/auth/refresh":
			h.Refresh(w, r)
		case "/v1/auth/revoke":
			h.Revoke(w, r)
		}
	}))
}

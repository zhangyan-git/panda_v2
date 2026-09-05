package routes

import (
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"net/http"
	"strings"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/controller"
)

func RegisterRoutes(m *http.ServeMux, h *controller.Handler, authorizer *auth.Service) {
	protected := auth.Bearer(authorizer)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFromRequest(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !isAdmin(identity) {
			writeError(w, http.StatusForbidden, "forbidden")
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/coffee-machine/manufacturers"):
			h.Collection(w, r, "manufacturers")
		case strings.HasPrefix(r.URL.Path, "/v1/coffee-machine/devices"):
			if strings.Contains(strings.TrimSuffix(r.URL.Path, "/"), "/drinks") {
				h.Relations(w, r)
			} else {
				h.Collection(w, r, "devices")
			}
		case strings.HasPrefix(r.URL.Path, "/v1/coffee-machine/drinks"):
			h.Collection(w, r, "drinks")
		default:
			api.Error(w, http.StatusNotFound, api.CodeNotFound, "not found")
		}
	}))
	m.Handle("/v1/coffee-machine", protected)
	m.Handle("/v1/coffee-machine/", protected)
}

func isAdmin(identity auth.Identity) bool {
	for _, role := range identity.Roles {
		if strings.TrimSpace(role) == "admin" {
			return true
		}
	}
	return false
}

func writeError(w http.ResponseWriter, status int, message string) {
	api.Error(w, status, api.CodeForStatus(status), message)
}

package routes

import (
	"net/http"
	"strings"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/controller"
)

func RegisterRoutes(mux *http.ServeMux, h *controller.Handler, authorizer *auth.Service) {
	admin := auth.Bearer(authorizer)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/admin/stores/") && strings.Contains(r.URL.Path, "/audit/") {
			h.AdminAudit(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/admin/stores") {
			h.AdminStores(w, r)
			return
		}
		h.AdminMerchants(w, r)
	}))
	mux.Handle("/v1/admin/", admin)
	merchant := auth.Bearer(authorizer)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/merchant/profile" {
			h.Profile(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/submit") {
			h.Submit(w, r)
			return
		}
		h.Stores(w, r)
	}))
	mux.Handle("/v1/merchant/", merchant)
}

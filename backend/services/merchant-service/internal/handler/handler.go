package handler

import (
	"encoding/json"
	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/service"
	"net/http"
)

type LegacyHandler struct{ service *service.Service }

func NewLegacy(s *service.Service) *LegacyHandler { return &LegacyHandler{service: s} }
func (h *LegacyHandler) List(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	m, e := h.service.ListMerchants(r.Context())
	if e != nil {
		http.Error(w, "internal server error", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(m)
}
func Register(s *khttp.Server, m *AdminMerchantHandler, b *AdminBrandHandler, st *AdminStoreHandler, l *LegacyHandler) {
	s.HandleFunc("/v1/admin/merchants", m.List)
	s.HandleFunc("/v1/admin/merchants/{id}", m.Get)
	s.HandleFunc("/v1/admin/merchants/{id}/status", m.UpdateStatus)
	s.HandleFunc("/v1/admin/brands", b.List)
	s.HandleFunc("/v1/admin/brands/{id}", b.Get)
	s.HandleFunc("/v1/admin/brands/{id}/status", b.UpdateStatus)
	s.HandleFunc("/v1/admin/brands/{id}/audit", b.Audit)
	s.HandleFunc("/v1/admin/stores", st.List)
	s.HandleFunc("/v1/admin/stores/{id}", st.Get)
	s.HandleFunc("/v1/admin/stores/{id}/status", st.UpdateStatus)
	s.HandleFunc("/v1/admin/stores/{id}/audit", st.Audit)
	s.HandleFunc("/v1/merchant-service/merchants", l.List)
}

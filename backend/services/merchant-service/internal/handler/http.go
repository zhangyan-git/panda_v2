package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/domain"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/repository"
)

const maxBodyBytes = 1 << 20

type Handler struct{ service *domain.Service }

func New(service *domain.Service) *Handler { return &Handler{service: service} }

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return errors.New("multiple JSON values")
	}
	return nil
}

func identity(r *http.Request) (auth.Identity, bool) { return auth.IdentityFromRequest(r) }
func isAdmin(i auth.Identity) bool {
	for _, role := range i.Roles {
		if role == "admin" {
			return true
		}
	}
	return false
}
func requireAdmin(w http.ResponseWriter, r *http.Request) (auth.Identity, bool) {
	i, ok := identity(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return i, false
	}
	if !isAdmin(i) {
		writeError(w, http.StatusForbidden, "forbidden")
		return i, false
	}
	return i, true
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
func statusError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, repository.ErrNotFound), errors.Is(err, domain.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, domain.ErrForbidden):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, domain.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, domain.ErrInvalidMerchant),
		errors.Is(err, domain.ErrInvalidStore),
		errors.Is(err, domain.ErrInvalidAccount),
		errors.Is(err, domain.ErrInvalidPermission),
		errors.Is(err, domain.ErrInvalidAudit),
		errors.Is(err, domain.ErrInvalidStatus),
		errors.Is(err, domain.ErrStoreMerchant):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}

// MerchantRequest and StoreRequest provide the public JSON naming contract.
type MerchantRequest struct {
	ID              string        `json:"id"`
	Name            string        `json:"name"`
	ContactName     string        `json:"contact_name"`
	ContactPhone    string        `json:"contact_phone"`
	BusinessLicense string        `json:"business_license"`
	Address         string        `json:"address"`
	Status          domain.Status `json:"status"`
}
type StoreRequest struct {
	ID            string        `json:"id"`
	MerchantID    string        `json:"merchant_id"`
	BrandID       string        `json:"brand_id"`
	Name          string        `json:"name"`
	Logo          string        `json:"logo"`
	Phone         string        `json:"phone"`
	Province      string        `json:"province"`
	City          string        `json:"city"`
	District      string        `json:"district"`
	Address       string        `json:"address"`
	BusinessHours string        `json:"business_hours"`
	Longitude     float64       `json:"longitude"`
	Latitude      float64       `json:"latitude"`
	Status        domain.Status `json:"status"`
	Visible       bool          `json:"visible"`
}

func (v MerchantRequest) merchant() domain.Merchant {
	return domain.Merchant{ID: v.ID, Name: v.Name, ContactName: v.ContactName, ContactPhone: v.ContactPhone, BusinessLicense: v.BusinessLicense, Address: v.Address, Status: v.Status}
}
func storeRequest(v StoreRequest) domain.Store {
	return domain.Store{ID: v.ID, MerchantID: v.MerchantID, BrandID: v.BrandID, Name: v.Name, Logo: v.Logo, Phone: v.Phone, Province: v.Province, City: v.City, District: v.District, Address: v.Address, BusinessHours: v.BusinessHours, Longitude: v.Longitude, Latitude: v.Latitude, Status: v.Status, Visible: v.Visible}
}

func (h *Handler) AdminMerchants(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		if strings.TrimSuffix(r.URL.Path, "/") != "/v1/admin/merchants" {
			id := strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/"), "/v1/admin/merchants/")
			v, e := h.service.GetMerchant(r.Context(), id)
			if e != nil {
				statusError(w, e)
				return
			}
			writeJSON(w, 200, v)
			return
		}
		v, e := h.service.ListMerchants(r.Context())
		if e != nil {
			statusError(w, e)
			return
		}
		writeJSON(w, 200, v)
	case http.MethodPost:
		var q MerchantRequest
		if decodeJSON(w, r, &q) != nil {
			return
		}
		v, e := h.service.CreateMerchant(r.Context(), q.merchant())
		if e != nil {
			statusError(w, e)
			return
		}
		writeJSON(w, 201, v)
	case http.MethodPut, http.MethodPatch:
		id := strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/"), "/v1/admin/merchants/")
		var q MerchantRequest
		if decodeJSON(w, r, &q) != nil {
			return
		}
		v, e := h.service.UpdateMerchant(r.Context(), id, q.merchant())
		if e != nil {
			statusError(w, e)
			return
		}
		writeJSON(w, 200, v)
	default:
		writeError(w, 405, "method not allowed")
	}
}

func (h *Handler) AdminStores(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	path := strings.TrimSuffix(r.URL.Path, "/")
	base := "/v1/admin/stores"
	if r.Method == http.MethodGet && path == base {
		m := r.URL.Query().Get("merchant_id")
		if m != "" {
			v, e := h.service.ListStoresByMerchant(r.Context(), m)
			if e != nil {
				statusError(w, e)
				return
			}
			writeJSON(w, 200, v)
			return
		}
		writeError(w, 400, "merchant_id is required")
		return
	}
	id := strings.TrimPrefix(path, base+"/")
	if id == path || id == "" {
		if r.Method == http.MethodPost {
			var q StoreRequest
			if decodeJSON(w, r, &q) != nil {
				return
			}
			v, e := h.service.CreateStore(r.Context(), storeRequest(q))
			if e != nil {
				statusError(w, e)
				return
			}
			writeJSON(w, 201, v)
			return
		}
		writeError(w, 405, "method not allowed")
		return
	}
	switch r.Method {
	case http.MethodGet:
		v, e := h.service.GetStore(r.Context(), id)
		if e != nil {
			statusError(w, e)
			return
		}
		writeJSON(w, 200, v)
	case http.MethodPut, http.MethodPatch:
		var q StoreRequest
		if decodeJSON(w, r, &q) != nil {
			return
		}
		v, e := h.service.UpdateStore(r.Context(), id, storeRequest(q))
		if e != nil {
			statusError(w, e)
			return
		}
		writeJSON(w, 200, v)
	case http.MethodDelete:
		e := h.service.DeleteStore(r.Context(), id)
		if e != nil {
			statusError(w, e)
			return
		}
		writeJSON(w, 200, map[string]bool{"deleted": true})
	default:
		writeError(w, 405, "method not allowed")
	}
}

func (h *Handler) AdminAudit(w http.ResponseWriter, r *http.Request) {
	i, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	p := strings.TrimSuffix(r.URL.Path, "/")
	const prefix = "/v1/admin/stores/"
	if !strings.HasPrefix(p, prefix) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	parts := strings.Split(strings.TrimPrefix(p, prefix), "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] != "audit" || (parts[2] != "approve" && parts[2] != "reject") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var q struct {
		Remark string `json:"remark"`
	}
	if decodeJSON(w, r, &q) != nil {
		return
	}
	var out domain.StoreAuditRecord
	var err error
	if parts[2] == "approve" {
		out, err = h.service.Approve(r.Context(), parts[0], i.Subject, q.Remark)
	} else {
		out, err = h.service.Reject(r.Context(), parts[0], i.Subject, q.Remark)
	}
	if err != nil {
		statusError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) Profile(w http.ResponseWriter, r *http.Request) {
	i, ok := identity(r)
	if !ok {
		writeError(w, 401, "unauthorized")
		return
	}
	a, e := h.service.GetMerchantAccountByAccountID(r.Context(), i.Subject)
	if e != nil {
		statusError(w, e)
		return
	}
	m, e := h.service.GetMerchant(r.Context(), a.MerchantID)
	if e != nil {
		statusError(w, e)
		return
	}
	writeJSON(w, 200, m)
}
func (h *Handler) Stores(w http.ResponseWriter, r *http.Request) {
	i, ok := identity(r)
	if !ok {
		writeError(w, 401, "unauthorized")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	v, e := h.service.ScopedStores(r.Context(), i.Subject)
	if e != nil {
		statusError(w, e)
		return
	}
	writeJSON(w, 200, v)
}
func (h *Handler) Submit(w http.ResponseWriter, r *http.Request) {
	i, ok := identity(r)
	if !ok {
		writeError(w, 401, "unauthorized")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	id := strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/"), "/v1/merchant/stores/")
	v, e := h.service.SubmitForReview(r.Context(), id, i.Subject)
	if e != nil {
		statusError(w, e)
		return
	}
	writeJSON(w, 201, v)
}

func RegisterRoutes(mux *http.ServeMux, h *Handler, authorizer *auth.Service) {
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

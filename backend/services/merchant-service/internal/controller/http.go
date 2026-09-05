package controller

import (
	"encoding/json"
	"errors"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"io"
	"net/http"
	"strings"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/service"
)

const maxBodyBytes = 1 << 20

type Handler struct{ service *service.Service }

func New(service *service.Service) *Handler { return &Handler{service: service} }

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
	api.Success(w, status, value)
}
func writeError(w http.ResponseWriter, status int, message string) {
	api.Error(w, status, api.CodeForStatus(status), message)
}
func statusError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, repository.ErrNotFound), errors.Is(err, model.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, model.ErrForbidden):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, model.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, model.ErrInvalidMerchant),
		errors.Is(err, model.ErrInvalidStore),
		errors.Is(err, model.ErrInvalidAccount),
		errors.Is(err, model.ErrInvalidPermission),
		errors.Is(err, model.ErrInvalidAudit),
		errors.Is(err, model.ErrInvalidStatus),
		errors.Is(err, model.ErrStoreMerchant):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
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
		var q dto.MerchantRequest
		if decodeJSON(w, r, &q) != nil {
			return
		}
		v, e := h.service.CreateMerchant(r.Context(), q.Merchant())
		if e != nil {
			statusError(w, e)
			return
		}
		writeJSON(w, 201, v)
	case http.MethodPut, http.MethodPatch:
		id := strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/"), "/v1/admin/merchants/")
		var q dto.MerchantRequest
		if decodeJSON(w, r, &q) != nil {
			return
		}
		v, e := h.service.UpdateMerchant(r.Context(), id, q.Merchant())
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
			var q dto.StoreRequest
			if decodeJSON(w, r, &q) != nil {
				return
			}
			v, e := h.service.CreateStore(r.Context(), dto.Store(q))
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
		var q dto.StoreRequest
		if decodeJSON(w, r, &q) != nil {
			return
		}
		v, e := h.service.UpdateStore(r.Context(), id, dto.Store(q))
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
	var out model.StoreAuditRecord
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

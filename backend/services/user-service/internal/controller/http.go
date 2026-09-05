package controller

import (
	"encoding/json"
	"errors"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
)

const maxRegisterBodyBytes = 1 << 20

type Handler struct{ service *service.Service }

func New(service *service.Service) *Handler { return &Handler{service: service} }

func decodeJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRegisterBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return false
	}
	return true
}

func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request dto.RegisterRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	user, err := h.service.Register(r.Context(), request.Name)
	if err != nil {
		if errors.Is(err, model.ErrInvalidName) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusCreated, user)
}

func parseID(path string) (int64, error) {
	path = strings.TrimPrefix(path, "/v1")
	id, err := strconv.ParseInt(strings.TrimPrefix(path, "/users/"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid user id")
	}
	return id, nil
}

func (h *Handler) GetByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, err := parseID(r.URL.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	user, err := h.service.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, user)
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, err := parseID(r.URL.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var request dto.PatchRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	user, err := h.service.Update(r.Context(), id, request.UserUpdate())
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, user)
}

func (h *Handler) Profile(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.URL.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	identity, ok := auth.IdentityFromRequest(r)
	if !ok || identity.UserID != strconv.FormatInt(id, 10) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if r.Method == http.MethodGet {
		h.GetByID(w, r)
		return
	}
	if r.Method == http.MethodPatch {
		h.Update(w, r)
		return
	}
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	api.Success(w, status, value)
}
func writeError(w http.ResponseWriter, status int, message string) {
	api.Error(w, status, api.CodeForStatus(status), message)
}

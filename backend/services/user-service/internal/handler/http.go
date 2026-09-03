package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/domain"
)

const maxRegisterBodyBytes = 1 << 20

type Handler struct{ service *domain.Service }

func New(service *domain.Service) *Handler { return &Handler{service: service} }

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
	var request struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	user, err := h.service.Register(r.Context(), request.Name)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidName) {
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
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, user)
}

type patchRequest struct {
	Nickname   *string   `json:"nickname"`
	AvatarURL  *string   `json:"avatar_url"`
	Email      *string   `json:"email"`
	Gender     *string   `json:"gender"`
	Birthday   *string   `json:"birthday"`
	Occupation *string   `json:"occupation"`
	Hobbies    *[]string `json:"hobbies"`
	RegionCode *string   `json:"region_code"`
	RegionName *string   `json:"region_name"`
}

func (p *patchRequest) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if len(fields) == 0 {
		return errors.New("patch must contain at least one field")
	}
	for name, raw := range fields {
		if string(raw) == "null" {
			return errors.New("null patch field")
		}
		var target any
		switch name {
		case "nickname":
			target = &p.Nickname
		case "avatar_url":
			target = &p.AvatarURL
		case "email":
			target = &p.Email
		case "gender":
			target = &p.Gender
		case "birthday":
			target = &p.Birthday
		case "occupation":
			target = &p.Occupation
		case "hobbies":
			target = &p.Hobbies
		case "region_code":
			target = &p.RegionCode
		case "region_name":
			target = &p.RegionName
		default:
			return errors.New("unknown patch field")
		}
		if err := json.Unmarshal(raw, target); err != nil {
			return err
		}
	}
	return nil
}

func (p patchRequest) update() domain.UserUpdate {
	return domain.UserUpdate{Nickname: p.Nickname, AvatarURL: p.AvatarURL, Email: p.Email, Gender: p.Gender, Birthday: p.Birthday, Occupation: p.Occupation, Hobbies: p.Hobbies, RegionCode: p.RegionCode, RegionName: p.RegionName}
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
	var request patchRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	user, err := h.service.Update(r.Context(), id, request.update())
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
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
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

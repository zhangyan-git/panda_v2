package controller

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
)

func TestStatusErrorMapsConflict(t *testing.T) {
	recorder := httptest.NewRecorder()
	statusError(recorder, model.ErrConflict)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusConflict)
	}
}

func TestStatusErrorHidesUnknownDetails(t *testing.T) {
	recorder := httptest.NewRecorder()
	statusError(recorder, errors.New("database password leaked"))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	if strings.Contains(recorder.Body.String(), "database password leaked") {
		t.Fatalf("response leaked internal error: %s", recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "internal server error") {
		t.Fatalf("response = %s, want generic message", recorder.Body.String())
	}
}

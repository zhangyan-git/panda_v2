package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResponseSuccessAndError(t *testing.T) {
	for _, test := range []struct {
		name        string
		write       func(http.ResponseWriter)
		httpStatus  int
		wantSuccess bool
		wantCode    string
		dataPresent bool
	}{
		{
			"success",
			func(w http.ResponseWriter) { Success(w, map[string]bool{"ok": true}) },
			http.StatusOK, true, "", true,
		},
		{
			"error",
			func(w http.ResponseWriter) { Error(w, http.StatusBadRequest, CodeInvalidRequest, "bad") },
			http.StatusBadRequest, false, CodeInvalidRequest, false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			test.write(w)
			if w.Code != test.httpStatus {
				t.Fatalf("http status: got %d, want %d", w.Code, test.httpStatus)
			}
			var got Response
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Success != test.wantSuccess {
				t.Errorf("success: got %v, want %v", got.Success, test.wantSuccess)
			}
			if test.wantCode != "" && got.ErrorCode != test.wantCode {
				t.Errorf("errorCode: got %q, want %q", got.ErrorCode, test.wantCode)
			}
			if test.dataPresent && got.Data == nil {
				t.Error("expected data to be present")
			}
			if !test.dataPresent && got.Data != nil {
				t.Errorf("expected no data, got %v", got.Data)
			}
		})
	}
}

func TestWriteNoContentHasEmptyBody(t *testing.T) {
	w := httptest.NewRecorder()
	WriteNoContent(w)
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestSuccessPage(t *testing.T) {
	w := httptest.NewRecorder()
	SuccessPage(w, []string{"a", "b"}, 2)
	if w.Code != http.StatusOK {
		t.Fatalf("http status: got %d", w.Code)
	}
	var got Response
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Success {
		t.Error("expected success=true")
	}
	if got.Data == nil {
		t.Error("expected data to contain page payload")
	}
}

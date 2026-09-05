package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEnvelopeSuccessAndError(t *testing.T) {
	for _, test := range []struct {
		name    string
		write   func(http.ResponseWriter)
		status  string
		code    int
		dataNil bool
	}{
		{"success", func(w http.ResponseWriter) { Success(w, http.StatusOK, map[string]bool{"ok": true}) }, "success", CodeOK, false},
		{"error", func(w http.ResponseWriter) { Error(w, http.StatusBadRequest, CodeInvalidRequest, "bad") }, "error", CodeInvalidRequest, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			test.write(w)
			if w.Code != map[bool]int{true: http.StatusBadRequest, false: http.StatusOK}[test.dataNil] {
				t.Fatalf("status=%d", w.Code)
			}
			var got Envelope
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Status != test.status || got.Code != test.code || (got.Data == nil) != test.dataNil {
				t.Fatalf("envelope=%+v", got)
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

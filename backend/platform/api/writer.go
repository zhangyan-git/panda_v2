package api

import "net/http"

func WriteOK(w http.ResponseWriter, status int, data any) { Success(w, status, data) }
func WriteError(w http.ResponseWriter, status int, code int, message string) {
	Error(w, status, code, message)
}
func WriteNoContent(w http.ResponseWriter) { Write(w, http.StatusNoContent, 0, "", nil) }

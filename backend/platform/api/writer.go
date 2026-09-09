package api

import "net/http"

func WriteOK(w http.ResponseWriter, data any)                             { Success(w, data) }
func WriteError(w http.ResponseWriter, status int, code string, msg string) { Error(w, status, code, msg) }
func WriteNoContent(w http.ResponseWriter)                                { NoContent(w) }

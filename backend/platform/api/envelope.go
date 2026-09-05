package api

import (
	"encoding/json"
	"net/http"
)

type Envelope struct {
	Status  string `json:"status"`
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

func Write(w http.ResponseWriter, status int, code int, message string, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if status != http.StatusNoContent {
		envelopeStatus := "error"
		if status >= 200 && status < 400 {
			envelopeStatus = "success"
		}
		_ = json.NewEncoder(w).Encode(Envelope{Status: envelopeStatus, Code: code, Message: message, Data: data})
	}
}

func Success(w http.ResponseWriter, status int, data any) {
	Write(w, status, CodeOK, successMessage(status), data)
}

func Error(w http.ResponseWriter, status int, code int, message string) {
	Write(w, status, code, message, nil)
}

func successMessage(status int) string {
	if status == http.StatusCreated {
		return "created"
	}
	return "success"
}

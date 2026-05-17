package server

import (
	"encoding/json"
	"net/http"
)

// errorResponse is the canonical error envelope. Mirrors the shape auth.go
// uses for 401s so clients see one consistent format across the gateway.
type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
	Details any    `json:"details,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorResponse{Error: code, Message: msg})
}

func writeErrorWithDetails(w http.ResponseWriter, status int, code, msg string, details any) {
	writeJSON(w, status, errorResponse{Error: code, Message: msg, Details: details})
}

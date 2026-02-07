package openai

import (
	"encoding/json"
	"net/http"
)

type errorResponse struct {
	Error errorObject `json:"error"`
}

type errorObject struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
}

func writeError(w http.ResponseWriter, status int, typ, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{
		Error: errorObject{
			Message: msg,
			Type:    typ,
			Code:    code,
		},
	})
}


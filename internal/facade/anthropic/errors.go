package anthropic

import (
	"encoding/json"
	"net/http"
)

type apiErrorResponse struct {
	Type  string      `json:"type"`
	Error apiErrorObj `json:"error"`
}

type apiErrorObj struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, typ string, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiErrorResponse{
		Type: "error",
		Error: apiErrorObj{
			Type:    typ,
			Message: msg,
		},
	})
}


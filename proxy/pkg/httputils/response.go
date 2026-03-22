package httputils

import (
	"encoding/json"
	"net/http"
	"proxy/internal/models"
)

func ErrorResponse(w http.ResponseWriter, code int, message string) {
	JSONResponse(w, code, models.ErrorResponse{
		Error: models.ErrorDetails{
			Code:    code,
			Message: message,
		},
	})
}

func JSONResponse(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	if data == nil {
		w.WriteHeader(status)
		return
	}

	payload, err := json.Marshal(data)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":500,"message":"internal server error"}}` + "\n"))
		return
	}

	w.WriteHeader(status)
	_, _ = w.Write(append(payload, '\n'))
}

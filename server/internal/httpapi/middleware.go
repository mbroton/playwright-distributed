package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

func authMiddleware(api huma.API, authenticator Authenticator, logger *slog.Logger) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		principal, err := authenticator.Authenticate(ctx.Context(), ctx.Header("Authorization"))
		if errors.Is(err, errUnauthorized) {
			ctx.SetHeader("WWW-Authenticate", "Bearer")
			if writeErr := huma.WriteErr(api, ctx, http.StatusUnauthorized, "unauthorized"); writeErr != nil {
				logger.Error("write authentication response", "error", writeErr)
			}
			return
		}
		if err != nil {
			logger.Error("authenticate request", "error", err)
			ctx.SetHeader("Retry-After", "1")
			if writeErr := huma.WriteErr(api, ctx, http.StatusServiceUnavailable, "authentication service unavailable"); writeErr != nil {
				logger.Error("write authentication service response", "error", writeErr)
			}
			return
		}
		next(huma.WithValue(ctx, principalContextKey{}, principal))
	}
}

func requestLogger(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		response := &loggingResponseWriter{
			ResponseWriter: w,
			status:         http.StatusOK,
		}
		next.ServeHTTP(response, r)
		logger.Info(
			"http request",
			"method", r.Method,
			"path", r.URL.Path,
			"remote_address", r.RemoteAddr,
			"status", response.status,
			"duration", time.Since(start),
		)
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *loggingResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *loggingResponseWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *loggingResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

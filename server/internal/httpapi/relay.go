package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"

	"server/internal/db/data"
	"server/internal/relay"
	"server/internal/scheduler"
)

type relayHandler struct {
	queries        *data.Queries
	authenticator  Authenticator
	scheduler      *scheduler.Scheduler
	manager        *relay.Manager
	defaultBrowser string
	dialTimeout    time.Duration
	logger         *slog.Logger
	upgrader       websocket.Upgrader
}

const relayTransitionTimeout = 5 * time.Second

func registerRelayRoutes(
	mux *http.ServeMux,
	queries *data.Queries,
	authenticator Authenticator,
	sessionScheduler *scheduler.Scheduler,
	manager *relay.Manager,
	defaultBrowser string,
	dialTimeout time.Duration,
	logger *slog.Logger,
) {
	handler := &relayHandler{
		queries:        queries,
		authenticator:  authenticator,
		scheduler:      sessionScheduler,
		manager:        manager,
		defaultBrowser: defaultBrowser,
		dialTimeout:    dialTimeout,
		logger:         logger,
	}
	mux.HandleFunc("GET /{$}", handler.implicit)
	mux.HandleFunc("GET /sessions/{id}", handler.explicit)
}

func (h *relayHandler) implicit(w http.ResponseWriter, request *http.Request) {
	browser := h.defaultBrowser
	if requested := request.URL.Query().Get("browser"); requested != "" {
		if !validBrowser(requested) {
			writeRelayError(
				w,
				http.StatusBadRequest,
				fmt.Sprintf("Unknown browser type: %s - allowed: chromium, firefox, webkit", requested),
			)
			return
		}
		browser = requested
	}
	if !h.preflight(w, request) {
		return
	}
	principal, ok := authenticateRelay(w, request, h.authenticator, h.logger)
	if !ok {
		return
	}
	if h.scheduler == nil {
		writeRelayError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	_, versionPrefix, _ := relay.UserAgentVersion(request.UserAgent())
	session, err := h.scheduler.Admit(request.Context(), scheduler.ClaimRequest{
		Browser:         browser,
		VersionPrefix:   versionPrefix,
		CreatedByKey:    principal.KeyID,
		ConnectMetadata: json.RawMessage(`{}`),
	})
	if err != nil {
		h.writeAdmissionError(w, err)
		return
	}
	h.attach(w, request, session)
}

func (h *relayHandler) explicit(w http.ResponseWriter, request *http.Request) {
	if !h.preflight(w, request) {
		return
	}
	if _, ok := authenticateRelay(w, request, h.authenticator, h.logger); !ok {
		return
	}
	sessionID, err := uuid.Parse(request.PathValue("id"))
	if err != nil {
		writeRelayError(w, http.StatusNotFound, "session not found")
		return
	}
	session, err := h.queries.GetSession(request.Context(), sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeRelayError(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		h.logger.Error("load relay session", "session_id", sessionID, "error", err)
		writeRelayError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	switch session.Status {
	case data.SessionStatusPending:
	case data.SessionStatusRunning:
		writeRelayError(w, http.StatusConflict, "session already has an attached client")
		return
	default:
		writeRelayError(w, http.StatusGone, "session is no longer available")
		return
	}
	if clientVersion, clientPrefix, ok := relay.UserAgentVersion(request.UserAgent()); ok {
		sessionPrefix, _ := relay.VersionPrefix(session.PlaywrightVersion)
		if clientPrefix != sessionPrefix {
			writeRelayError(
				w,
				http.StatusConflict,
				fmt.Sprintf(
					"client Playwright version %s is incompatible with session version %s",
					clientVersion,
					session.PlaywrightVersion,
				),
			)
			return
		}
	}
	h.attach(w, request, session)
}

func (h *relayHandler) preflight(w http.ResponseWriter, request *http.Request) bool {
	if !websocket.IsWebSocketUpgrade(request) {
		writeRelayError(w, http.StatusUpgradeRequired, "This endpoint is for WebSocket connections only.")
		return false
	}
	if !sameOrigin(request) {
		writeRelayError(w, http.StatusForbidden, "websocket origin is not allowed")
		return false
	}
	return true
}

func (h *relayHandler) attach(w http.ResponseWriter, request *http.Request, session data.Session) {
	reservation, err := h.manager.BeginAttach(session.ID)
	if errors.Is(err, relay.ErrDraining) {
		w.Header().Set("Retry-After", "1")
		writeRelayError(w, http.StatusServiceUnavailable, "server is shutting down")
		return
	}
	if err != nil {
		h.logger.Error("reserve relay attachment", "session_id", session.ID, "error", err)
		writeRelayError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	defer reservation.Release()

	if _, err := h.queries.StartSession(request.Context(), session.ID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.writeRejectedStart(w, request, session.ID)
			return
		}
		h.logger.Error("start relay session", "session_id", session.ID, "error", err)
		h.failSession(session.ID)
		writeRelayError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	worker, err := h.dialWorker(request, session)
	if err != nil {
		h.failSession(session.ID)
		h.logger.Warn("dial session worker", "session_id", session.ID, "error", err)
		writeRelayError(w, http.StatusBadGateway, "worker connection failed")
		return
	}

	client, err := h.upgrader.Upgrade(w, request, nil)
	if err != nil {
		closeErr := relay.ClosePeer(worker, websocket.CloseInternalServerErr, "client upgrade failed")
		if connectionErr := worker.Close(); connectionErr != nil {
			closeErr = errors.Join(closeErr, connectionErr)
		}
		if closeErr != nil &&
			!errors.Is(closeErr, websocket.ErrCloseSent) &&
			!errors.Is(closeErr, net.ErrClosed) {
			h.logger.Debug("close worker after client upgrade failure", "session_id", session.ID, "error", closeErr)
		}
		h.failSession(session.ID)
		h.logger.Warn("upgrade relay client", "session_id", session.ID, "error", err)
		return
	}
	if err := h.manager.Run(reservation, client, worker); err != nil &&
		!errors.Is(err, relay.ErrDraining) &&
		!errors.Is(err, relay.ErrDuplicateRelay) {
		h.logger.Error("run relay", "session_id", session.ID, "error", err)
	}
}

func (h *relayHandler) dialWorker(request *http.Request, session data.Session) (*websocket.Conn, error) {
	dialCtx, cancel := context.WithTimeout(request.Context(), h.dialTimeout)
	defer cancel()
	headers := make(http.Header)
	if userAgent := request.UserAgent(); userAgent != "" {
		headers.Set("User-Agent", userAgent)
	}
	// Playwright changes this header set across client versions. Prefix
	// forwarding is deliberate because the client is authenticated and the
	// worker is trusted infrastructure. Use an allow-list if workers later use
	// client-controlled headers for privileged actions.
	for name, values := range request.Header {
		if !strings.HasPrefix(strings.ToLower(name), "x-playwright-") {
			continue
		}
		for _, value := range values {
			headers.Add(name, value)
		}
	}
	headers.Set("x-pwd-session-id", session.ID.String())
	dialer := websocket.Dialer{HandshakeTimeout: h.dialTimeout}
	connection, response, err := dialer.DialContext(dialCtx, session.WorkerAddress, headers)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("dialing worker: %w", err)
	}
	return connection, nil
}

func (h *relayHandler) writeRejectedStart(w http.ResponseWriter, request *http.Request, sessionID uuid.UUID) {
	session, err := h.queries.GetSession(request.Context(), sessionID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			h.logger.Error("load rejected relay session", "session_id", sessionID, "error", err)
		}
		writeRelayError(w, http.StatusGone, "session is no longer available")
		return
	}
	if session.Status == data.SessionStatusRunning {
		writeRelayError(w, http.StatusConflict, "session already has an attached client")
		return
	}
	writeRelayError(w, http.StatusGone, "session is no longer available")
}

func (h *relayHandler) failSession(sessionID uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.Background(), relayTransitionTimeout)
	defer cancel()
	if _, err := h.queries.FailSession(ctx, sessionID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		h.logger.Error("fail relay session", "session_id", sessionID, "error", err)
	}
}

func (h *relayHandler) writeAdmissionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, scheduler.ErrQueueFull):
		writeRelayError(w, http.StatusTooManyRequests, "session capacity and queue are full")
	case errors.Is(err, scheduler.ErrWaitTimeout):
		w.Header().Set("Retry-After", "1")
		writeRelayError(w, http.StatusServiceUnavailable, "session queue wait timed out")
	case errors.Is(err, scheduler.ErrDraining):
		w.Header().Set("Retry-After", "1")
		writeRelayError(w, http.StatusServiceUnavailable, "server is shutting down")
	case errors.Is(err, context.Canceled):
	default:
		h.logger.Error("admit relay session", "error", err)
		writeRelayError(w, http.StatusInternalServerError, "internal server error")
	}
}

func authenticateRelay(
	w http.ResponseWriter,
	request *http.Request,
	authenticator Authenticator,
	logger *slog.Logger,
) (Principal, bool) {
	authorization := request.Header.Get("Authorization")
	if authorization == "" {
		if token := request.URL.Query().Get("token"); token != "" {
			authorization = "Bearer " + token
		}
	}
	principal, err := authenticator.Authenticate(request.Context(), authorization)
	if errors.Is(err, errUnauthorized) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeRelayError(w, http.StatusUnauthorized, "unauthorized")
		return Principal{}, false
	}
	if err != nil {
		logger.Error("authenticate relay request", "error", err)
		w.Header().Set("Retry-After", "1")
		writeRelayError(w, http.StatusServiceUnavailable, "authentication service unavailable")
		return Principal{}, false
	}
	return principal, true
}

func sameOrigin(request *http.Request) bool {
	origins := request.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) != 1 {
		return false
	}
	origin, err := url.Parse(origins[0])
	return err == nil && strings.EqualFold(origin.Host, request.Host)
}

func validBrowser(browser string) bool {
	return browser == "chromium" || browser == "firefox" || browser == "webkit"
}

func writeRelayError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Message string `json:"message"`
	}{Message: message})
}

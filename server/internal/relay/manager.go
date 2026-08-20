package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"

	"server/internal/db/data"
)

const (
	copyBufferSize       = 32 * 1024
	closeFrameTimeout    = 2 * time.Second
	terminalQueryTimeout = 5 * time.Second

	// ShutdownCleanupBudget covers the two sequential close-frame writes and
	// the terminal database transition after the shutdown grace period.
	ShutdownCleanupBudget = 2*closeFrameTimeout + terminalQueryTimeout
)

var (
	ErrDraining       = errors.New("relay manager is draining")
	ErrDuplicateRelay = errors.New("duplicate relay")
)

type registerResult uint8

const (
	registerAccepted registerResult = iota
	registerDraining
	registerDuplicate
)

type Options struct {
	WriteTimeout      time.Duration
	PingInterval      time.Duration
	PongTimeout       time.Duration
	HeartbeatInterval time.Duration
	ShutdownGrace     time.Duration
}

type Manager struct {
	queries *data.Queries
	logger  *slog.Logger
	options Options

	mu       sync.Mutex
	draining bool
	live     map[uuid.UUID]func(relayEvent)
	wg       sync.WaitGroup
}

type relayEvent struct {
	code   int
	reason string
	normal bool
	err    error
}

func NewManager(queries *data.Queries, logger *slog.Logger, options Options) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		queries: queries,
		logger:  logger,
		options: options,
		live:    make(map[uuid.UUID]func(relayEvent)),
	}
}

func (m *Manager) Accepting() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.draining
}

func (m *Manager) Run(sessionID uuid.UUID, client, worker *websocket.Conn) error {
	events := make(chan relayEvent, 4)
	stop := func(event relayEvent) {
		select {
		case events <- event:
		default:
		}
	}
	switch m.register(sessionID, stop) {
	case registerDraining:
		m.closeConnections(sessionID, client, worker, websocket.CloseGoingAway, "server shutting down")
		m.recordOutcome(sessionID, true)
		return ErrDraining
	case registerDuplicate:
		m.closeConnections(sessionID, client, worker, websocket.CloseInternalServerErr, "duplicate relay")
		// Implicit sessions have fresh UUIDs, and explicit sessions pass the
		// StartSession guard before Run. A duplicate is therefore defensive.
		m.logger.Error("duplicate relay", "session_id", sessionID)
		return ErrDuplicateRelay
	}
	defer m.unregister(sessionID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	configurePeer(client, m.options)
	configurePeer(worker, m.options)

	var goroutines sync.WaitGroup
	start := func(run func() relayEvent) {
		goroutines.Add(1)
		go func() {
			defer goroutines.Done()
			stop(run())
		}()
	}
	start(func() relayEvent { return copyMessages(client, worker, m.options) })
	start(func() relayEvent { return copyMessages(worker, client, m.options) })
	start(func() relayEvent { return pingPeers(ctx, client, worker, m.options) })
	start(func() relayEvent { return m.renewHeartbeat(ctx, sessionID) })

	event := <-events
	cancel()
	m.closeConnections(sessionID, client, worker, event.code, event.reason)
	goroutines.Wait()
	m.recordOutcome(sessionID, event.normal)
	if event.err != nil {
		m.logger.Debug("relay stopped", "session_id", sessionID, "error", event.err)
	}
	return nil
}

func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.draining = true
	m.mu.Unlock()

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(m.options.ShutdownGrace)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}

	event := relayEvent{
		code:   websocket.CloseGoingAway,
		reason: "server shutting down",
		normal: true,
	}
	m.mu.Lock()
	stops := make([]func(relayEvent), 0, len(m.live))
	for _, stop := range m.live {
		stops = append(stops, stop)
	}
	m.mu.Unlock()
	for _, stop := range stops {
		stop(event)
	}

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) register(sessionID uuid.UUID, stop func(relayEvent)) registerResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.draining {
		return registerDraining
	}
	if _, exists := m.live[sessionID]; exists {
		return registerDuplicate
	}
	m.live[sessionID] = stop
	m.wg.Add(1)
	return registerAccepted
}

func (m *Manager) unregister(sessionID uuid.UUID) {
	m.mu.Lock()
	delete(m.live, sessionID)
	m.mu.Unlock()
	m.wg.Done()
}

func (m *Manager) renewHeartbeat(ctx context.Context, sessionID uuid.UUID) relayEvent {
	ticker := time.NewTicker(m.options.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return relayEvent{err: ctx.Err()}
		case <-ticker.C:
			_, err := m.queries.RenewSessionHeartbeat(ctx, sessionID)
			switch {
			case err == nil:
			case errors.Is(err, pgx.ErrNoRows):
				return relayEvent{
					code:   websocket.CloseGoingAway,
					reason: "session terminated",
					normal: true,
				}
			default:
				m.logger.Warn("renew session heartbeat", "session_id", sessionID, "error", err)
			}
		}
	}
}

func (m *Manager) recordOutcome(sessionID uuid.UUID, normal bool) {
	ctx, cancel := context.WithTimeout(context.Background(), terminalQueryTimeout)
	defer cancel()
	var err error
	if normal {
		_, err = m.queries.CompleteSession(ctx, sessionID)
	} else {
		_, err = m.queries.FailSession(ctx, sessionID)
	}
	if err == nil || errors.Is(err, pgx.ErrNoRows) {
		return
	}
	m.logger.Error("record relay outcome", "session_id", sessionID, "normal", normal, "error", err)
}

func configurePeer(conn *websocket.Conn, options Options) {
	_ = conn.SetReadDeadline(time.Now().Add(options.PongTimeout))
	// The relay forwards the received close after it selects the winning
	// teardown event. Disable Gorilla's fixed one-second default close reply.
	conn.SetCloseHandler(func(int, string) error { return nil })
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(options.PongTimeout))
	})
	conn.SetPingHandler(func(payload string) error {
		return conn.WriteControl(
			websocket.PongMessage,
			[]byte(payload),
			time.Now().Add(options.WriteTimeout),
		)
	})
}

func copyMessages(src, dst *websocket.Conn, options Options) relayEvent {
	buffer := make([]byte, copyBufferSize)
	for {
		if err := src.SetReadDeadline(time.Now().Add(options.PongTimeout)); err != nil {
			return errorEvent(err)
		}
		messageType, reader, err := src.NextReader()
		if err != nil {
			return errorEvent(err)
		}
		if err := dst.SetWriteDeadline(time.Now().Add(options.WriteTimeout)); err != nil {
			return errorEvent(err)
		}
		writer, err := dst.NextWriter(messageType)
		if err != nil {
			return errorEvent(err)
		}
		_, copyErr := io.CopyBuffer(
			&deadlineWriter{conn: dst, writer: writer, timeout: options.WriteTimeout},
			&deadlineReader{conn: src, reader: reader, timeout: options.PongTimeout},
			buffer,
		)
		if copyErr != nil {
			return errorEvent(copyErr)
		}
		deadlineErr := dst.SetWriteDeadline(time.Now().Add(options.WriteTimeout))
		closeErr := writer.Close()
		if err := errors.Join(deadlineErr, closeErr); err != nil {
			return errorEvent(err)
		}
	}
}

func pingPeers(ctx context.Context, client, worker *websocket.Conn, options Options) relayEvent {
	ticker := time.NewTicker(options.PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return relayEvent{err: ctx.Err()}
		case <-ticker.C:
			deadline := time.Now().Add(options.WriteTimeout)
			clientErr := client.WriteControl(websocket.PingMessage, nil, deadline)
			workerErr := worker.WriteControl(websocket.PingMessage, nil, deadline)
			if err := errors.Join(clientErr, workerErr); err != nil {
				return errorEvent(err)
			}
		}
	}
}

func errorEvent(err error) relayEvent {
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		if closeErr.Code != websocket.CloseNoStatusReceived && !isValidReceivedCloseCode(closeErr.Code) {
			return relayEvent{
				code:   websocket.CloseInternalServerErr,
				reason: "peer connection lost",
				err:    err,
			}
		}
		return relayEvent{
			code:   closeErr.Code,
			reason: closeErr.Text,
			normal: isNormalCloseCode(closeErr.Code),
			err:    err,
		}
	}
	return relayEvent{
		code:   websocket.CloseInternalServerErr,
		reason: "relay error",
		err:    err,
	}
}

func isValidReceivedCloseCode(code int) bool {
	switch code {
	case websocket.CloseNormalClosure,
		websocket.CloseGoingAway,
		websocket.CloseProtocolError,
		websocket.CloseUnsupportedData,
		websocket.CloseInvalidFramePayloadData,
		websocket.ClosePolicyViolation,
		websocket.CloseMessageTooBig,
		websocket.CloseMandatoryExtension,
		websocket.CloseInternalServerErr,
		websocket.CloseServiceRestart,
		websocket.CloseTryAgainLater:
		return true
	default:
		return code >= 3000 && code <= 4999
	}
}

func isNormalCloseCode(code int) bool {
	return code == websocket.CloseNormalClosure ||
		code == websocket.CloseGoingAway ||
		code == websocket.CloseNoStatusReceived
}

func (m *Manager) closeConnections(
	sessionID uuid.UUID,
	client, worker *websocket.Conn,
	code int,
	reason string,
) {
	clientErr := closePeer(client, code, reason)
	workerErr := closePeer(worker, code, reason)
	_ = client.Close()
	_ = worker.Close()
	m.logCloseError(sessionID, "client", clientErr)
	m.logCloseError(sessionID, "worker", workerErr)
}

func (m *Manager) logCloseError(sessionID uuid.UUID, peer string, err error) {
	if err == nil || errors.Is(err, websocket.ErrCloseSent) || errors.Is(err, net.ErrClosed) {
		return
	}
	m.logger.Debug("send relay close", "session_id", sessionID, "peer", peer, "error", err)
}

func closePeer(conn *websocket.Conn, code int, reason string) error {
	if conn == nil {
		return nil
	}
	message := websocket.FormatCloseMessage(code, reason)
	return conn.WriteControl(websocket.CloseMessage, message, time.Now().Add(closeFrameTimeout))
}

type deadlineReader struct {
	conn    *websocket.Conn
	reader  io.Reader
	timeout time.Duration
}

func (r *deadlineReader) Read(buffer []byte) (int, error) {
	read, err := r.reader.Read(buffer)
	if read > 0 {
		if deadlineErr := r.conn.SetReadDeadline(time.Now().Add(r.timeout)); deadlineErr != nil {
			return read, errors.Join(err, deadlineErr)
		}
	}
	return read, err
}

type deadlineWriter struct {
	conn    *websocket.Conn
	writer  io.Writer
	timeout time.Duration
}

func (w *deadlineWriter) Write(buffer []byte) (int, error) {
	if err := w.conn.SetWriteDeadline(time.Now().Add(w.timeout)); err != nil {
		return 0, fmt.Errorf("setting relay write deadline: %w", err)
	}
	return w.writer.Write(buffer)
}

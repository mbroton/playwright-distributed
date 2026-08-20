package relay

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

func TestManager_RegisterDistinguishesDrainAndDuplicate(t *testing.T) {
	manager := NewManager(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{})
	sessionID := uuid.New()
	events := make(chan relayEvent, 1)
	stop := func(event relayEvent) { events <- event }
	reservation, err := manager.BeginAttach(sessionID)
	if err != nil {
		t.Fatalf("BeginAttach() returned an error: %v", err)
	}
	if got := manager.register(reservation, stop); got != registerAccepted {
		t.Fatalf("first register result = %d, want accepted", got)
	}
	t.Cleanup(func() { manager.unregister(sessionID) })
	duplicate, err := manager.BeginAttach(sessionID)
	if err != nil {
		t.Fatalf("duplicate BeginAttach() returned an error: %v", err)
	}
	defer duplicate.Release()
	if got := manager.register(duplicate, func(relayEvent) {}); got != registerDuplicate {
		t.Fatalf("duplicate register result = %d, want duplicate", got)
	}

	manager.live[sessionID](relayEvent{reason: "owner"})
	if event := <-events; event.reason != "owner" {
		t.Fatalf("registered owner event reason = %q, want owner", event.reason)
	}
	draining, err := manager.BeginAttach(uuid.New())
	if err != nil {
		t.Fatalf("draining BeginAttach() returned an error before drain: %v", err)
	}
	defer draining.Release()
	manager.draining = true
	if got := manager.register(draining, func(relayEvent) {}); got != registerDraining {
		t.Fatalf("draining register result = %d, want draining", got)
	}
}

func TestManager_ShutdownWaitsForAttachReservation(t *testing.T) {
	manager := NewManager(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
		ShutdownGrace: time.Millisecond,
	})
	reservation, err := manager.BeginAttach(uuid.New())
	if err != nil {
		t.Fatalf("BeginAttach() returned an error: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- manager.Shutdown(shutdownCtx) }()

	for {
		probe, beginErr := manager.BeginAttach(uuid.New())
		if errors.Is(beginErr, ErrDraining) {
			break
		}
		if beginErr != nil {
			t.Fatalf("BeginAttach() during shutdown returned an error: %v", beginErr)
		}
		probe.Release()
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown() returned before reservation release: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	reservation.Release()
	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown() returned an error: %v", err)
	}
}

func TestErrorEvent_SanitizesNonTransmittableCloseCodes(t *testing.T) {
	tests := []struct {
		name       string
		code       int
		wantCode   int
		wantReason string
		wantNormal bool
	}{
		{
			name:       "normal close",
			code:       websocket.CloseNormalClosure,
			wantCode:   websocket.CloseNormalClosure,
			wantReason: "peer reason",
			wantNormal: true,
		},
		{
			name:       "no status uses empty close payload",
			code:       websocket.CloseNoStatusReceived,
			wantCode:   websocket.CloseNoStatusReceived,
			wantReason: "peer reason",
			wantNormal: true,
		},
		{
			name:       "application close",
			code:       4008,
			wantCode:   4008,
			wantReason: "peer reason",
		},
		{
			name:       "abnormal close",
			code:       websocket.CloseAbnormalClosure,
			wantCode:   websocket.CloseInternalServerErr,
			wantReason: "peer connection lost",
		},
		{
			name:       "TLS close",
			code:       websocket.CloseTLSHandshake,
			wantCode:   websocket.CloseInternalServerErr,
			wantReason: "peer connection lost",
		},
		{
			name:       "unregistered close",
			code:       2000,
			wantCode:   websocket.CloseInternalServerErr,
			wantReason: "peer connection lost",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := errorEvent(&websocket.CloseError{Code: test.code, Text: "peer reason"})
			if event.code != test.wantCode || event.reason != test.wantReason || event.normal != test.wantNormal {
				t.Fatalf(
					"errorEvent() = code %d, reason %q, normal %t; want %d, %q, %t",
					event.code,
					event.reason,
					event.normal,
					test.wantCode,
					test.wantReason,
					test.wantNormal,
				)
			}
		})
	}
}

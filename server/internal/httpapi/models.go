package httpapi

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"server/internal/db/data"
)

type Worker struct {
	ID                uuid.UUID         `json:"id" format:"uuid"`
	Address           string            `json:"address"`
	Browser           string            `json:"browser"`
	PlaywrightVersion string            `json:"playwright_version"`
	MaxSlots          int32             `json:"max_slots"`
	Status            data.WorkerStatus `json:"status" enum:"available,draining,stalled,shutting_down"`
	LastHeartbeat     time.Time         `json:"last_heartbeat"`
	LifetimeSessions  int64             `json:"lifetime_sessions"`
	CreatedAt         time.Time         `json:"created_at"`
}

type Session struct {
	ID                uuid.UUID          `json:"id" format:"uuid"`
	WorkerID          uuid.UUID          `json:"worker_id" format:"uuid"`
	Browser           string             `json:"browser"`
	PlaywrightVersion string             `json:"playwright_version"`
	WorkerAddress     string             `json:"worker_address"`
	Mode              data.SessionMode   `json:"mode" enum:"default,dedicated"`
	Status            data.SessionStatus `json:"status" enum:"pending,running,completed,failed,expired"`
	CreatedByKey      *uuid.UUID         `json:"created_by_key,omitempty" format:"uuid"`
	CreatedAt         time.Time          `json:"created_at"`
	ExpiresAt         *time.Time         `json:"expires_at,omitempty"`
	LastHeartbeat     time.Time          `json:"last_heartbeat"`
	KeepAliveMs       *int32             `json:"keep_alive_ms,omitempty"`
	ConnectMetadata   map[string]any     `json:"connect_metadata"`
}

func workerFromData(worker data.Worker) Worker {
	return Worker{
		ID:                worker.ID,
		Address:           worker.Address,
		Browser:           worker.Browser,
		PlaywrightVersion: worker.PlaywrightVersion,
		MaxSlots:          worker.MaxSlots,
		Status:            worker.Status,
		LastHeartbeat:     worker.LastHeartbeat,
		LifetimeSessions:  worker.LifetimeSessions,
		CreatedAt:         worker.CreatedAt,
	}
}

func sessionFromData(session data.Session) (Session, error) {
	metadata := map[string]any{}
	if err := json.Unmarshal(session.ConnectMetadata, &metadata); err != nil {
		return Session{}, fmt.Errorf("decoding session connect metadata: %w", err)
	}

	var keepAliveMs *int32
	if session.KeepAliveMs.Valid {
		value := session.KeepAliveMs.Int32
		keepAliveMs = &value
	}

	return Session{
		ID:                session.ID,
		WorkerID:          session.WorkerID,
		Browser:           session.Browser,
		PlaywrightVersion: session.PlaywrightVersion,
		WorkerAddress:     session.WorkerAddress,
		Mode:              session.Mode,
		Status:            session.Status,
		CreatedByKey:      session.CreatedByKey,
		CreatedAt:         session.CreatedAt,
		ExpiresAt:         session.ExpiresAt,
		LastHeartbeat:     session.LastHeartbeat,
		KeepAliveMs:       keepAliveMs,
		ConnectMetadata:   metadata,
	}, nil
}

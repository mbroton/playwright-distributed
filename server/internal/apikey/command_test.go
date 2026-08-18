package apikey

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"server/internal/db"
	"server/internal/db/data"
)

const postgresImage = "postgres:18-alpine"

var errWriteFailed = errors.New("write failed")

type failingWriter struct {
	cancel context.CancelFunc
}

func (w failingWriter) Write([]byte) (int, error) {
	if w.cancel != nil {
		w.cancel()
	}
	return 0, errWriteFailed
}

func TestRun_CreateListRevoke(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)

	var createOutput bytes.Buffer
	if err := Run(t.Context(), []string{"create", "--name", "deployment"}, queries, &createOutput); err != nil {
		t.Fatalf("Run(create) returned an error: %v", err)
	}
	token := strings.TrimSpace(createOutput.String())
	if !strings.HasPrefix(token, tokenPrefix) || len(strings.Split(createOutput.String(), "\n")) != 2 {
		t.Fatalf("create output = %q, want one pwd_ token line", createOutput.String())
	}

	keys, err := queries.ListAPIKeys(t.Context())
	if err != nil {
		t.Fatalf("ListAPIKeys() returned an error: %v", err)
	}
	if len(keys) != 1 || keys[0].Name != "deployment" || keys[0].Prefix != token[:storedTokenPrefixLength] {
		t.Fatalf("stored keys = %+v, want created deployment key", keys)
	}

	var listOutput bytes.Buffer
	if err := Run(t.Context(), []string{"list"}, queries, &listOutput); err != nil {
		t.Fatalf("Run(list) returned an error: %v", err)
	}
	listed := listOutput.String()
	if !strings.Contains(listed, keys[0].ID.String()) || !strings.Contains(listed, "deployment") {
		t.Fatalf("list output does not contain key fields: %s", listed)
	}
	if strings.Contains(listed, token) || strings.Contains(strings.ToLower(listed), "hash") {
		t.Fatalf("list output contains secret material: %s", listed)
	}

	if err := Run(
		t.Context(),
		[]string{"revoke", "--id", keys[0].ID.String()},
		queries,
		&bytes.Buffer{},
	); err != nil {
		t.Fatalf("Run(revoke) returned an error: %v", err)
	}
	if _, err := queries.GetActiveAPIKeyByHash(t.Context(), hashTokenForTest(token)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetActiveAPIKeyByHash() after revoke error = %v, want %v", err, pgx.ErrNoRows)
	}
	var alreadyRevokedOutput bytes.Buffer
	if err := Run(
		t.Context(),
		[]string{"revoke", "--id", keys[0].ID.String()},
		queries,
		&alreadyRevokedOutput,
	); err != nil {
		t.Fatalf("Run(revoke already revoked) returned an error: %v", err)
	}
	wantAlreadyRevoked := fmt.Sprintf("api key %s was already revoked\n", keys[0].ID)
	if alreadyRevokedOutput.String() != wantAlreadyRevoked {
		t.Fatalf("already-revoked output = %q, want %q", alreadyRevokedOutput.String(), wantAlreadyRevoked)
	}

	unknownID := uuid.New()
	if err := Run(
		t.Context(),
		[]string{"revoke", "--id", unknownID.String()},
		queries,
		&bytes.Buffer{},
	); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("api key %s not found", unknownID)) {
		t.Fatalf("Run(revoke unknown) error = %v, want not-found error", err)
	}

	listOutput.Reset()
	if err := Run(t.Context(), []string{"list"}, queries, &listOutput); err != nil {
		t.Fatalf("Run(list after revoke) returned an error: %v", err)
	}
	if !strings.Contains(listOutput.String(), keys[0].ID.String()) || strings.HasSuffix(listOutput.String(), "-\n") {
		t.Fatalf("revoked key list output = %q, want key and revoke time", listOutput.String())
	}
}

func TestRun_HelpAndUnknownSubcommand(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantUsage string
	}{
		{name: "create help", args: []string{"create", "--help"}, wantUsage: "Usage of apikey create"},
		{name: "short help", args: []string{"-h"}, wantUsage: "Usage: server apikey"},
		{name: "long help", args: []string{"--help"}, wantUsage: "Usage: server apikey"},
		{name: "help command", args: []string{"help"}, wantUsage: "Usage: server apikey"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var help bytes.Buffer
			if err := Run(t.Context(), test.args, nil, &help); err != nil {
				t.Fatalf("Run(%v) returned an error: %v", test.args, err)
			}
			if !strings.Contains(help.String(), test.wantUsage) {
				t.Fatalf("help output = %q, want %q", help.String(), test.wantUsage)
			}
		})
	}

	err := Run(t.Context(), []string{"rotate"}, nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "create, list, revoke") {
		t.Fatalf("Run(unknown) error = %v, want valid subcommands", err)
	}
}

func TestCreate_DeletesKeyWhenPrintingFails(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)

	tests := []struct {
		name   string
		writer func(context.CancelFunc) failingWriter
	}{
		{
			name: "write failure",
			writer: func(context.CancelFunc) failingWriter {
				return failingWriter{}
			},
		},
		{
			name: "context canceled before write failure",
			writer: func(cancel context.CancelFunc) failingWriter {
				return failingWriter{cancel: cancel}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if _, err := Create(ctx, queries, "unusable", test.writer(cancel)); !errors.Is(err, errWriteFailed) {
				t.Fatalf("Create() error = %v, want %v", err, errWriteFailed)
			}
			count, err := queries.CountActiveAPIKeys(t.Context())
			if err != nil {
				t.Fatalf("CountActiveAPIKeys() returned an error: %v", err)
			}
			if count != 0 {
				t.Fatalf("active API key count = %d, want 0 after output failure", count)
			}
		})
	}
}

func TestRun_ReportsUnmigratedDatabase(t *testing.T) {
	pool := newTestPool(t)
	err := Run(t.Context(), []string{"list"}, data.New(pool), &bytes.Buffer{})
	want := `database is not migrated; run "server serve" once first`
	if err == nil || err.Error() != want {
		t.Fatalf("Run(list) error = %v, want %q", err, want)
	}
}

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)

	container, err := postgres.Run(
		t.Context(),
		postgresImage,
		postgres.WithDatabase("server_test"),
		postgres.WithUsername("server_test"),
		postgres.WithPassword("server_test"),
		postgres.BasicWaitStrategies(),
	)
	testcontainers.CleanupContainer(t, container)
	if err != nil {
		t.Fatalf("starting Postgres container: %v", err)
	}
	dsn, err := container.ConnectionString(t.Context(), "sslmode=disable")
	if err != nil {
		t.Fatalf("getting Postgres connection string: %v", err)
	}
	pool, err := db.Open(t.Context(), dsn)
	if err != nil {
		t.Fatalf("opening Postgres pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newMigratedTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newTestPool(t)
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("migrating test database: %v", err)
	}
	return pool
}

func hashTokenForTest(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

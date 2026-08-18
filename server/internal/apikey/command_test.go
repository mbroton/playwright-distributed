package apikey

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"server/internal/db"
	"server/internal/db/data"
)

const postgresImage = "postgres:18-alpine"

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
	if len(keys) != 1 || keys[0].Name != "deployment" || keys[0].Prefix != token[:8] {
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

	listOutput.Reset()
	if err := Run(t.Context(), []string{"list"}, queries, &listOutput); err != nil {
		t.Fatalf("Run(list after revoke) returned an error: %v", err)
	}
	if !strings.Contains(listOutput.String(), keys[0].ID.String()) || strings.HasSuffix(listOutput.String(), "-\n") {
		t.Fatalf("revoked key list output = %q, want key and revoke time", listOutput.String())
	}
}

func newMigratedTestPool(t *testing.T) *pgxpool.Pool {
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
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("migrating test database: %v", err)
	}
	return pool
}

func hashTokenForTest(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

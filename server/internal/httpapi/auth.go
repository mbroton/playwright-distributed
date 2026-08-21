package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"server/internal/db/data"
)

const authQueryTimeout = 2 * time.Second

var (
	errUnauthorized = errors.New("httpapi: unauthorized")
	errAuthBackend  = errors.New("httpapi: authentication backend unavailable")
)

type Principal struct {
	KeyID *uuid.UUID
}

type principalContextKey struct{}

func PrincipalFromContext(ctx context.Context) Principal {
	principal, _ := ctx.Value(principalContextKey{}).(Principal)
	return principal
}

type Authenticator interface {
	Authenticate(ctx context.Context, authorization string) (Principal, error)
}

type NoAuthAuthenticator struct{}

func (NoAuthAuthenticator) Authenticate(context.Context, string) (Principal, error) {
	return Principal{}, nil
}

type TokenAuthenticator struct {
	queries *data.Queries
	logger  *slog.Logger

	mu                      sync.Mutex
	authRequired            bool
	unauthenticatedWarnOnce sync.Once
}

func NewTokenAuthenticator(queries *data.Queries, logger *slog.Logger) *TokenAuthenticator {
	return &TokenAuthenticator{
		queries: queries,
		logger:  logger,
	}
}

func (a *TokenAuthenticator) Authenticate(ctx context.Context, authorization string) (Principal, error) {
	required, err := a.requiresAuthentication(ctx)
	if err != nil {
		return Principal{}, err
	}
	if !required {
		a.unauthenticatedWarnOnce.Do(func() {
			a.logger.Warn("control plane authentication disabled because no active api keys exist")
		})
		return Principal{}, nil
	}

	token, ok := bearerToken(authorization)
	if !ok {
		return Principal{}, errUnauthorized
	}

	digest := sha256.Sum256([]byte(token))
	key, err := a.queries.GetActiveAPIKeyByHash(ctx, hex.EncodeToString(digest[:]))
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, errUnauthorized
	}
	if err != nil {
		return Principal{}, fmt.Errorf("%w: looking up api key: %w", errAuthBackend, err)
	}
	if err := a.queries.TouchAPIKey(ctx, key.ID); err != nil {
		a.logger.Warn("touch api key", "key_id", key.ID, "error", err)
	}

	keyID := key.ID
	return Principal{KeyID: &keyID}, nil
}

func (a *TokenAuthenticator) requiresAuthentication(ctx context.Context) (bool, error) {
	a.mu.Lock()
	required := a.authRequired
	a.mu.Unlock()
	if required {
		return true, nil
	}

	// A request whose count predates a concurrent first-key commit can still pass.
	// That window is bounded by the request; after a positive count, the latch stays enabled.
	queryCtx, cancel := context.WithTimeout(ctx, authQueryTimeout)
	defer cancel()
	count, err := a.queries.CountActiveAPIKeys(queryCtx)
	if err != nil {
		return false, fmt.Errorf("%w: counting active api keys: %w", errAuthBackend, err)
	}
	if count == 0 {
		a.mu.Lock()
		required = a.authRequired
		a.mu.Unlock()
		return required, nil
	}

	a.mu.Lock()
	a.authRequired = true
	a.mu.Unlock()
	return true, nil
}

func bearerToken(authorization string) (string, bool) {
	scheme, token, ok := strings.Cut(authorization, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	if !strings.HasPrefix(token, "pwd_") || len(token) == len("pwd_") || strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}
	return token, true
}

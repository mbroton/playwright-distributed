package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"server/internal/db/data"
)

var errUnauthorized = errors.New("httpapi: unauthorized")

type Authenticator interface {
	Authenticate(ctx context.Context, authorization string) error
}

type NoAuthAuthenticator struct{}

func (NoAuthAuthenticator) Authenticate(context.Context, string) error {
	return nil
}

type TokenAuthenticator struct {
	queries *data.Queries
	ttl     time.Duration
	noAuth  NoAuthAuthenticator

	mu             sync.Mutex
	activeKeyCount int64
	cacheExpiresAt time.Time
}

func NewTokenAuthenticator(queries *data.Queries, ttl time.Duration) *TokenAuthenticator {
	return &TokenAuthenticator{
		queries: queries,
		ttl:     ttl,
	}
}

func (a *TokenAuthenticator) Authenticate(ctx context.Context, authorization string) error {
	count, err := a.countActiveKeys(ctx)
	if err != nil {
		return errUnauthorized
	}
	if count == 0 {
		return a.noAuth.Authenticate(ctx, authorization)
	}

	token, ok := bearerToken(authorization)
	if !ok {
		return errUnauthorized
	}

	digest := sha256.Sum256([]byte(token))
	key, err := a.queries.GetActiveAPIKeyByHash(ctx, hex.EncodeToString(digest[:]))
	if err != nil {
		return errUnauthorized
	}
	if _, err := a.queries.TouchAPIKey(ctx, key.ID); err != nil {
		return errUnauthorized
	}

	return nil
}

func (a *TokenAuthenticator) countActiveKeys(ctx context.Context) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now()
	if now.Before(a.cacheExpiresAt) {
		return a.activeKeyCount, nil
	}

	count, err := a.queries.CountActiveAPIKeys(ctx)
	if err != nil {
		return 0, err
	}
	a.activeKeyCount = count
	a.cacheExpiresAt = now.Add(a.ttl)
	return count, nil
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

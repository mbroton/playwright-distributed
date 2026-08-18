package apikey

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"server/internal/db/data"
)

const tokenPrefix = "pwd_"

func Run(ctx context.Context, args []string, queries *data.Queries, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("api key subcommand is required")
	}

	switch args[0] {
	case "create":
		flags := flag.NewFlagSet("apikey create", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		name := flags.String("name", "", "API key name")
		if err := flags.Parse(args[1:]); err != nil {
			return fmt.Errorf("parsing apikey create flags: %w", err)
		}
		if flags.NArg() != 0 {
			return errors.New("apikey create does not accept positional arguments")
		}
		_, err := Create(ctx, queries, *name, stdout)
		return err
	case "list":
		if len(args) != 1 {
			return errors.New("apikey list does not accept arguments")
		}
		return List(ctx, queries, stdout)
	case "revoke":
		flags := flag.NewFlagSet("apikey revoke", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		id := flags.String("id", "", "API key ID")
		if err := flags.Parse(args[1:]); err != nil {
			return fmt.Errorf("parsing apikey revoke flags: %w", err)
		}
		if flags.NArg() != 0 {
			return errors.New("apikey revoke does not accept positional arguments")
		}
		keyID, err := uuid.Parse(*id)
		if err != nil {
			return fmt.Errorf("parsing api key id: %w", err)
		}
		return Revoke(ctx, queries, keyID)
	default:
		return fmt.Errorf("unknown api key subcommand %q", args[0])
	}
}

func Create(
	ctx context.Context,
	queries *data.Queries,
	name string,
	stdout io.Writer,
) (data.APIKey, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return data.APIKey{}, errors.New("api key name is required")
	}

	token, err := generateToken()
	if err != nil {
		return data.APIKey{}, err
	}
	digest := sha256.Sum256([]byte(token))
	key, err := queries.InsertAPIKey(ctx, data.InsertAPIKeyParams{
		ID:     uuid.New(),
		Name:   name,
		Hash:   hex.EncodeToString(digest[:]),
		Prefix: token[:8],
	})
	if err != nil {
		return data.APIKey{}, fmt.Errorf("inserting api key: %w", err)
	}
	if _, err := fmt.Fprintln(stdout, token); err != nil {
		return data.APIKey{}, fmt.Errorf("printing api key token: %w", err)
	}
	return key, nil
}

func List(ctx context.Context, queries *data.Queries, stdout io.Writer) error {
	keys, err := queries.ListAPIKeys(ctx)
	if err != nil {
		return fmt.Errorf("listing api keys: %w", err)
	}

	table := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "ID\tNAME\tPREFIX\tCREATED_AT\tLAST_USED_AT\tREVOKED_AT"); err != nil {
		return fmt.Errorf("printing api key header: %w", err)
	}
	for _, key := range keys {
		if _, err := fmt.Fprintf(
			table,
			"%s\t%s\t%s\t%s\t%s\t%s\n",
			key.ID,
			key.Name,
			key.Prefix,
			formatTime(&key.CreatedAt),
			formatTime(key.LastUsedAt),
			formatTime(key.RevokedAt),
		); err != nil {
			return fmt.Errorf("printing api key: %w", err)
		}
	}
	if err := table.Flush(); err != nil {
		return fmt.Errorf("flushing api key list: %w", err)
	}
	return nil
}

func Revoke(ctx context.Context, queries *data.Queries, id uuid.UUID) error {
	if _, err := queries.RevokeAPIKey(ctx, id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("api key %s not found", id)
		}
		return fmt.Errorf("revoking api key: %w", err)
	}
	return nil
}

func generateToken() (string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generating api key token: %w", err)
	}
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(random), nil
}

func formatTime(value *time.Time) string {
	if value == nil {
		return "-"
	}
	return value.UTC().Format(time.RFC3339Nano)
}

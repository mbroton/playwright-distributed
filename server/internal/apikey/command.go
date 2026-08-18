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
	"github.com/jackc/pgx/v5/pgconn"

	"server/internal/db/data"
)

const (
	tokenPrefix             = "pwd_"
	storedTokenPrefixLength = len(tokenPrefix) + 4
	undefinedTableSQLState  = "42P01"
)

type commandKind uint8

const (
	createCommand commandKind = iota
	listCommand
	revokeCommand
)

type Command struct {
	kind commandKind
	name string
	id   uuid.UUID
}

func Parse(args []string, stdout io.Writer) (*Command, error) {
	if len(args) == 0 {
		return nil, errors.New("api key subcommand is required")
	}

	switch args[0] {
	case "-h", "--help", "help":
		if _, err := fmt.Fprintln(stdout, "Usage: server apikey <create|list|revoke> [options]"); err != nil {
			return nil, fmt.Errorf("printing api key help: %w", err)
		}
		return nil, nil
	case "create":
		flags := flag.NewFlagSet("apikey create", flag.ContinueOnError)
		flags.SetOutput(stdout)
		name := flags.String("name", "", "API key name")
		if err := flags.Parse(args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, nil
			}
			return nil, fmt.Errorf("parsing apikey create flags: %w", err)
		}
		if flags.NArg() != 0 {
			return nil, errors.New("apikey create does not accept positional arguments")
		}
		return &Command{kind: createCommand, name: *name}, nil
	case "list":
		flags := flag.NewFlagSet("apikey list", flag.ContinueOnError)
		flags.SetOutput(stdout)
		if err := flags.Parse(args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, nil
			}
			return nil, fmt.Errorf("parsing apikey list flags: %w", err)
		}
		if flags.NArg() != 0 {
			return nil, errors.New("apikey list does not accept arguments")
		}
		return &Command{kind: listCommand}, nil
	case "revoke":
		flags := flag.NewFlagSet("apikey revoke", flag.ContinueOnError)
		flags.SetOutput(stdout)
		id := flags.String("id", "", "API key ID")
		if err := flags.Parse(args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, nil
			}
			return nil, fmt.Errorf("parsing apikey revoke flags: %w", err)
		}
		if flags.NArg() != 0 {
			return nil, errors.New("apikey revoke does not accept positional arguments")
		}
		keyID, err := uuid.Parse(*id)
		if err != nil {
			return nil, fmt.Errorf("parsing api key id: %w", err)
		}
		return &Command{kind: revokeCommand, id: keyID}, nil
	default:
		return nil, fmt.Errorf("unknown api key subcommand %q (valid subcommands: create, list, revoke)", args[0])
	}
}

func Run(ctx context.Context, args []string, queries *data.Queries, stdout io.Writer) error {
	command, err := Parse(args, stdout)
	if err != nil || command == nil {
		return err
	}
	return command.Execute(ctx, queries, stdout)
}

func (command *Command) Execute(ctx context.Context, queries *data.Queries, stdout io.Writer) error {
	var err error
	switch command.kind {
	case createCommand:
		_, err = Create(ctx, queries, command.name, stdout)
	case listCommand:
		err = List(ctx, queries, stdout)
	case revokeCommand:
		err = Revoke(ctx, queries, command.id, stdout)
	default:
		err = errors.New("invalid api key command")
	}
	return friendlyDatabaseError(err)
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
		Prefix: token[:storedTokenPrefixLength],
	})
	if err != nil {
		return data.APIKey{}, fmt.Errorf("inserting api key: %w", err)
	}
	if _, err := fmt.Fprintln(stdout, token); err != nil {
		printErr := fmt.Errorf("printing api key token: %w", err)
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if deleteErr := queries.DeleteAPIKey(cleanupCtx, key.ID); deleteErr != nil {
			return data.APIKey{}, errors.Join(printErr, fmt.Errorf("deleting unusable api key: %w", deleteErr))
		}
		return data.APIKey{}, printErr
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

func Revoke(ctx context.Context, queries *data.Queries, id uuid.UUID, stdout io.Writer) error {
	if _, err := queries.RevokeAPIKey(ctx, id); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("revoking api key: %w", err)
		}
		key, lookupErr := queries.GetAPIKey(ctx, id)
		if errors.Is(lookupErr, pgx.ErrNoRows) {
			return fmt.Errorf("api key %s not found", id)
		}
		if lookupErr != nil {
			return fmt.Errorf("looking up api key after revoke: %w", lookupErr)
		}
		if key.RevokedAt == nil {
			return fmt.Errorf("api key %s could not be revoked", id)
		}
		if _, printErr := fmt.Fprintf(stdout, "api key %s was already revoked\n", id); printErr != nil {
			return fmt.Errorf("printing api key revoke status: %w", printErr)
		}
	}
	return nil
}

func friendlyDatabaseError(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == undefinedTableSQLState {
		return errors.New(`database is not migrated; run "server serve" once first`)
	}
	return err
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

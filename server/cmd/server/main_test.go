package main

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestRun_RejectsInvalidCommandsBeforeOpeningDatabase(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "unknown command",
			args:    []string{"unknown"},
			wantErr: `unknown command "unknown"`,
		},
		{
			name:    "serve with arguments",
			args:    []string{"serve", "extra"},
			wantErr: "serve does not accept arguments",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := run(
				t.Context(),
				test.args,
				&bytes.Buffer{},
				slog.New(slog.NewTextHandler(io.Discard, nil)),
			)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("run() error = %v, want error containing %q", err, test.wantErr)
			}
		})
	}
}

func TestRun_APIKeyHelpBeforeOpeningDatabase(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	var output bytes.Buffer
	err := run(
		t.Context(),
		[]string{"apikey", "create", "--help"},
		&output,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("run(apikey create --help) returned an error: %v", err)
	}
	if !strings.Contains(output.String(), "Usage of apikey create") {
		t.Fatalf("help output = %q, want apikey create usage", output.String())
	}
}

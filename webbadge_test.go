package main

import (
	"log/slog"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    slog.Level
		wantErr bool
	}{
		{name: "debug", input: "debug", want: slog.LevelDebug},
		{name: "default info", input: "info", want: slog.LevelInfo},
		{name: "case and whitespace", input: " WARN ", want: slog.LevelWarn},
		{name: "error", input: "error", want: slog.LevelError},
		{name: "invalid", input: "trace", want: slog.LevelInfo, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseLogLevel(test.input)
			if (err != nil) != test.wantErr {
				t.Fatalf("parseLogLevel(%q) error = %v, wantErr %t", test.input, err, test.wantErr)
			}
			if got != test.want {
				t.Errorf("parseLogLevel(%q) = %s, want %s", test.input, got, test.want)
			}
		})
	}
}

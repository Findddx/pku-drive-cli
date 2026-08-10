package apperr_test

import (
	"errors"
	"testing"

	"github.com/Findddx/pku-drive-cli/internal/apperr"
)

func TestExitCode(t *testing.T) {
	err := &apperr.Error{Category: apperr.Auth, Op: "token", Message: "login required"}
	if got := apperr.ExitCode(err); got != 3 {
		t.Fatalf("ExitCode() = %d, want 3", got)
	}
}

func TestExitCodeCategories(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
	}{
		{name: "nil", err: nil, wantCode: 0},
		{name: "usage", err: &apperr.Error{Category: apperr.Usage}, wantCode: 2},
		{name: "auth", err: &apperr.Error{Category: apperr.Auth}, wantCode: 3},
		{name: "remote", err: &apperr.Error{Category: apperr.Remote}, wantCode: 4},
		{name: "network", err: &apperr.Error{Category: apperr.Network}, wantCode: 5},
		{name: "local", err: &apperr.Error{Category: apperr.Local}, wantCode: 6},
		{name: "integrity", err: &apperr.Error{Category: apperr.Integrity}, wantCode: 7},
		{name: "interrupted", err: &apperr.Error{Category: apperr.Interrupted}, wantCode: 130},
		{name: "unknown category", err: &apperr.Error{Category: apperr.Category("other")}, wantCode: 5},
		{name: "plain error", err: errors.New("failed"), wantCode: 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := apperr.ExitCode(tt.err); got != tt.wantCode {
				t.Fatalf("ExitCode() = %d, want %d", got, tt.wantCode)
			}
		})
	}
}

func TestWrapPreservesCategoryAndCause(t *testing.T) {
	cause := errors.New("connection refused")
	err := apperr.Wrap(apperr.Network, "download", "request failed", cause)

	var appErr *apperr.Error
	if !errors.As(err, &appErr) {
		t.Fatalf("Wrap() error = %T, want *apperr.Error", err)
	}
	if appErr.Category != apperr.Network || appErr.Op != "download" || appErr.Message != "request failed" {
		t.Fatalf("Wrap() error = %#v", appErr)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("Wrap() did not preserve cause")
	}
}

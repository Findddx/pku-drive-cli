package apperr

import (
	"errors"
	"strings"
)

// Category identifies an error class with a stable process exit code.
type Category string

const (
	Usage       Category = "usage"
	Auth        Category = "auth"
	Remote      Category = "remote"
	Network     Category = "network"
	Local       Category = "local"
	Integrity   Category = "integrity"
	Interrupted Category = "interrupted"
)

// Error adds a stable category and operation context to an error.
type Error struct {
	Category Category
	Op       string
	Message  string
	Err      error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}

	parts := make([]string, 0, 3)
	if e.Op != "" {
		parts = append(parts, e.Op)
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	if e.Err != nil {
		parts = append(parts, e.Err.Error())
	}
	return strings.Join(parts, ": ")
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Wrap annotates err with the category and operation context used by the CLI.
func Wrap(category Category, op, message string, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Category: category, Op: op, Message: message, Err: err}
}

// ExitCode converts an application error into the CLI's stable exit contract.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}

	var appErr *Error
	if errors.As(err, &appErr) && appErr != nil {
		switch appErr.Category {
		case Usage:
			return 2
		case Auth:
			return 3
		case Remote:
			return 4
		case Network:
			return 5
		case Local:
			return 6
		case Integrity:
			return 7
		case Interrupted:
			return 130
		}
	}

	return 5
}

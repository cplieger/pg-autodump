package httpapi

import (
	"io"
	"log/slog"
	"testing"

	"github.com/cplieger/pg-autodump/internal/dump"
)

// --- httpapi.go L37:9 CONDITIONALS_NEGATION (`log == nil`) ----------------
//
// NewTrigger defaults a nil logger and keeps a supplied one:
// `if log == nil { log = slog.Default() }`. The negation `log != nil` inverts
// both branches: a nil logger would be left nil, and a supplied logger would
// be discarded for the package default. Exercise the condition-true (nil) and
// condition-false (non-nil) inputs and assert the stored logger in each.
func TestGkPgAutodumpU1NewTriggerLoggerDefaulting(t *testing.T) {
	guard := &dump.Guard{}

	// condition true (log == nil): the nil logger is replaced with a non-nil default.
	trNil := NewTrigger(guard, nil, nil)
	if trNil.log == nil {
		t.Fatalf("NewTrigger(.., nil) left log nil; want it defaulted to a non-nil logger")
	}

	// condition false (log != nil): the supplied logger is retained unchanged.
	custom := slog.New(slog.NewTextHandler(io.Discard, nil))
	trCustom := NewTrigger(guard, nil, custom)
	if trCustom.log != custom {
		t.Fatalf("NewTrigger(.., custom) did not retain the supplied logger; want it stored as-is")
	}
}

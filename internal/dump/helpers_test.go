package dump

import (
	"context"
	"io"
	"testing"
	"time"
)

// fakePG is a configurable PGTool for tests: no network, no real pg binaries.
type fakePG struct {
	probe  func(ctx context.Context, c Conn) (int, FailKind, error)
	dump   func(ctx context.Context, c Conn, w io.Writer) (int, string, error)
	verify func(ctx context.Context, path string) error
}

func (f *fakePG) Probe(ctx context.Context, c Conn) (int, FailKind, error) {
	if f.probe != nil {
		return f.probe(ctx, c)
	}
	return 18, FailNone, nil
}

func (f *fakePG) Dump(ctx context.Context, c Conn, w io.Writer) (int, string, error) {
	if f.dump != nil {
		return f.dump(ctx, c, w)
	}
	_, _ = io.WriteString(w, "PGDMP-fake")
	return 0, "", nil
}

func (f *fakePG) VerifyTOC(ctx context.Context, path string) error {
	if f.verify != nil {
		return f.verify(ctx, path)
	}
	return nil
}

func deadlineCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

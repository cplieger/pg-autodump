package dump

import (
	"context"
	"testing"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		ctxErr   error
		name     string
		want     Reason
		exitCode int
		kind     FailKind
	}{
		{name: "deadline wins", exitCode: 1, ctxErr: context.DeadlineExceeded, kind: FailConnect, want: ReasonTimeout},
		{name: "cancel wins", exitCode: 1, ctxErr: context.Canceled, kind: FailAuth, want: ReasonKilled},
		{name: "connect", exitCode: 0, ctxErr: nil, kind: FailConnect, want: ReasonConnectError},
		{name: "auth", exitCode: 0, ctxErr: nil, kind: FailAuth, want: ReasonAuthError},
		{name: "version", exitCode: 0, ctxErr: nil, kind: FailVersion, want: ReasonVersionMismatch},
		{name: "generic exit", exitCode: 2, ctxErr: nil, kind: FailNone, want: ReasonPGError},
		{name: "clean none", exitCode: 0, ctxErr: nil, kind: FailNone, want: ReasonOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classify(tt.exitCode, tt.ctxErr, tt.kind); got != tt.want {
				t.Fatalf("classify = %q, want %q", got, tt.want)
			}
		})
	}
}

// BodyDetail keeps raw pg_dump/pg_restore stderr out of the response body: the
// execution-tool failure reasons (pg_error, truncated, other) render as the
// bare reason word, while safe reasons render their Detail (falling back to the
// reason word when Detail is empty). The full detail still reaches the logs.
func TestResultBodyDetail(t *testing.T) {
	for _, r := range []Reason{ReasonPGError, ReasonTruncated, ReasonOther} {
		res := Result{Reason: r, Detail: "dump failed: FATAL secret-schema does not exist"}
		if got := res.BodyDetail(); got != string(r) {
			t.Errorf("BodyDetail() for %q = %q, want the reason word %q (stderr must not reach the body)", r, got, string(r))
		}
	}

	ok := Result{Reason: ReasonOK, Detail: "ok (5 bytes)"}
	if got := ok.BodyDetail(); got != "ok (5 bytes)" {
		t.Errorf("BodyDetail() ok = %q, want %q", got, "ok (5 bytes)")
	}

	mkdir := Result{Reason: ReasonMkdirFailed, Detail: "cannot create server dir /dumps/h_5432: permission denied"}
	if got := mkdir.BodyDetail(); got != mkdir.Detail {
		t.Errorf("BodyDetail() mkdir_failed = %q, want the operator-facing detail %q", got, mkdir.Detail)
	}

	empty := Result{Reason: ReasonConnectError}
	if got := empty.BodyDetail(); got != "connect_error" {
		t.Errorf("BodyDetail() empty detail = %q, want the reason word", got)
	}
}

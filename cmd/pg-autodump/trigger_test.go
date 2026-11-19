package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cplieger/pg-autodump/internal/httpapi"
)

// reportTrigger mirrors a run's lines to stdout, and turns a JSON refusal
// into one stderr line naming its message, code and remediation, so an
// ofelia log shows what to do without the envelope's raw JSON.
func TestReportTrigger(t *testing.T) {
	tests := []struct {
		name             string
		write            func(w http.ResponseWriter, r *http.Request)
		wantCode         int
		wantOut, wantErr string
		wantNoOut        bool
		wantNoE          bool
	}{
		{
			name: "run lines",
			write: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("h/db: ok\n"))
			},
			wantCode: 0, wantOut: "h/db: ok\n", wantNoE: true,
		},
		{
			name: "failed run lines",
			write: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("h/db: failed\n"))
			},
			wantCode: 1, wantOut: "h/db: failed\n", wantNoE: true,
		},
		{
			name: "json refusal",
			write: func(w http.ResponseWriter, r *http.Request) {
				httpapi.Errors(w, r, http.StatusTooManyRequests, "dump_in_progress", "dump already in progress")
			},
			wantCode: 1, wantNoOut: true,
			wantErr: "trigger: dump already in progress (dump_in_progress, status 429). Wait for the running dump to finish, then send the request again.\n",
		},
		{
			name: "unreadable json",
			write: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("{"))
			},
			wantCode: 1, wantNoOut: true,
			wantErr: "trigger: refused with status 503 and an unreadable error body\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tt.write(rec, httptest.NewRequest(http.MethodPost, "/dump", nil))
			var out, errOut bytes.Buffer

			got := reportTrigger(&out, &errOut, rec.Result())

			if got != tt.wantCode {
				t.Errorf("exit code = %d, want %d", got, tt.wantCode)
			}
			if tt.wantOut != "" && out.String() != tt.wantOut {
				t.Errorf("stdout = %q, want %q", out.String(), tt.wantOut)
			}
			if tt.wantNoOut && out.Len() != 0 {
				t.Errorf("stdout = %q, want nothing", out.String())
			}
			if tt.wantErr != "" && errOut.String() != tt.wantErr {
				t.Errorf("stderr = %q, want %q", errOut.String(), tt.wantErr)
			}
			if tt.wantNoE && errOut.Len() != 0 {
				t.Errorf("stderr = %q, want nothing", errOut.String())
			}
			if strings.Contains(out.String(), "{") {
				t.Errorf("stdout = %q carries JSON", out.String())
			}
		})
	}
}

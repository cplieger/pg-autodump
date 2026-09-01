package spec

import (
	"strings"
	"testing"
)

func TestParseSpecs(t *testing.T) {
	tests := []struct {
		check       func(t *testing.T, specs []DBSpec)
		name        string
		raw         string
		wantInvalid []bool
		wantCount   int
	}{
		{
			name:        "three-field defaults port",
			raw:         "host:db:user",
			wantCount:   1,
			wantInvalid: []bool{false},
			check: func(t *testing.T, s []DBSpec) {
				if s[0].Port != DefaultPort {
					t.Errorf("port = %d, want %d", s[0].Port, DefaultPort)
				}
			},
		},
		{
			name:        "four-field explicit port",
			raw:         "host:6543:db:user",
			wantCount:   1,
			wantInvalid: []bool{false},
			check: func(t *testing.T, s []DBSpec) {
				if s[0].Port != 6543 {
					t.Errorf("port = %d, want 6543", s[0].Port)
				}
			},
		},
		{name: "leading dash host rejected", raw: "-h:db:user", wantCount: 1, wantInvalid: []bool{true}},
		{name: "leading dash dbname rejected", raw: "host:-db:user", wantCount: 1, wantInvalid: []bool{true}},
		{name: "traversal rejected", raw: "host:..:user", wantCount: 1, wantInvalid: []bool{true}},
		{name: "bad port rejected", raw: "host:0:db:user", wantCount: 1, wantInvalid: []bool{true}},
		{name: "too many fields rejected", raw: "host:5432:db:user:extra", wantCount: 1, wantInvalid: []bool{true}},
		{name: "empty host rejected", raw: ":db:user", wantCount: 1, wantInvalid: []bool{true}},
		{name: "traversal in host rejected", raw: "a..b:db:user", wantCount: 1, wantInvalid: []bool{true}},
		{name: "empty dbname rejected", raw: "host::user", wantCount: 1, wantInvalid: []bool{true}},
		{name: "control char in dbname rejected", raw: "host:d\x01b:user", wantCount: 1, wantInvalid: []bool{true}},
		{name: "control char in user rejected", raw: "host:db:u\x01v", wantCount: 1, wantInvalid: []bool{true}},
		{
			name:        "same db different user is still a duplicate (output keyed by dbname)",
			raw:         "host:db:user host:db:user2",
			wantCount:   2,
			wantInvalid: []bool{false, true},
		},
		{
			name:        "true duplicate marked",
			raw:         "host:db:user host:db:user",
			wantCount:   2,
			wantInvalid: []bool{false, true},
		},
		{
			name:        "mixed valid and invalid preserved in order",
			raw:         "a:d1:u  bad  b:5433:d2:u",
			wantCount:   3,
			wantInvalid: []bool{false, true, false},
		},
		{name: "empty input", raw: "   ", wantCount: 0, wantInvalid: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			specs := Parse(tt.raw)
			if len(specs) != tt.wantCount {
				t.Fatalf("count = %d, want %d", len(specs), tt.wantCount)
			}
			for i, wantInvalid := range tt.wantInvalid {
				if (specs[i].Invalid != "") != wantInvalid {
					t.Errorf("spec[%d] invalid = %q, wantInvalid = %v", i, specs[i].Invalid, wantInvalid)
				}
			}
			if tt.check != nil {
				tt.check(t, specs)
			}
		})
	}
}

func TestParseSpecsNeverDropsTokens(t *testing.T) {
	raw := "a:b:c d e:f:g:h i:1:j:k"
	if got := len(Parse(raw)); got != 4 {
		t.Fatalf("got %d specs for 4 tokens; tokens must never be dropped", got)
	}
}

// Ports 1 and 65535 are both valid boundary values for the port guard.
func TestParseSpecsPortBoundaries(t *testing.T) {
	low := Parse("host:1:db:user")
	if len(low) != 1 || low[0].Invalid != "" {
		t.Fatalf("port 1 should be valid, got %+v", low)
	}
	if low[0].Port != 1 {
		t.Errorf("port = %d, want 1 (lower boundary)", low[0].Port)
	}

	high := Parse("host:65535:db:user")
	if len(high) != 1 || high[0].Invalid != "" {
		t.Fatalf("port 65535 should be valid, got %+v", high)
	}
	if high[0].Port != 65535 {
		t.Errorf("port = %d, want 65535 (upper boundary)", high[0].Port)
	}
}

// ServerDir ("<host>_<port>") is capped at maxServerDirLen (255), inclusive.
// With default port 5432 and the "_" separator, a 250-char host yields exactly
// 255 bytes and a 251-char host yields 256.
func TestParseSpecsServerDirLengthBoundary(t *testing.T) {
	atLimit := Parse(strings.Repeat("a", 250) + ":db:user")
	if len(atLimit) != 1 {
		t.Fatalf("count = %d, want 1", len(atLimit))
	}
	if got := len(ServerDir(atLimit[0].Host, atLimit[0].Port)); got != maxServerDirLen {
		t.Fatalf("ServerDir length = %d, want exactly %d (test fixture must sit on the boundary)", got, maxServerDirLen)
	}
	if atLimit[0].Invalid != "" {
		t.Errorf("a ServerDir of exactly %d bytes was rejected (%q); the limit is inclusive",
			maxServerDirLen, atLimit[0].Invalid)
	}

	over := Parse(strings.Repeat("a", 251) + ":db:user")
	if len(over) != 1 || over[0].Invalid == "" {
		t.Errorf("a ServerDir of %d bytes (one over the limit) must be rejected, got %+v", maxServerDirLen+1, over)
	}
}

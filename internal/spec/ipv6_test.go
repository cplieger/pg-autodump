package spec

import (
	"strings"
	"testing"
)

func TestParseSpecsIPv6(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantHost string
		wantPort int
		invalid  bool
	}{
		{name: "bracketed with port", raw: "[2001:db8::1]:5432:app:ro", wantHost: "2001:db8::1", wantPort: 5432},
		{name: "bracketed port omitted defaults 5432", raw: "[2001:db8::1]:app:ro", wantHost: "2001:db8::1", wantPort: 5432},
		{name: "loopback v6", raw: "[::1]:5432:app:ro", wantHost: "::1", wantPort: 5432},
		{name: "bracketed ipv4 canonicalizes (no @ dir)", raw: "[192.0.2.1]:5432:app:ro", wantHost: "192.0.2.1", wantPort: 5432},
		{name: "missing close bracket", raw: "[2001:db8::1:5432:app:ro", invalid: true},
		{name: "no colon after bracket", raw: "[2001:db8::1]5432:app:ro", invalid: true},
		{name: "not an ip in brackets", raw: "[not-an-ip]:5432:app:ro", invalid: true},
		{name: "zone id rejected", raw: "[fe80::1%eth0]:5432:app:ro", invalid: true},
		{name: "empty brackets", raw: "[]:5432:app:ro", invalid: true},
		{name: "bracket only port no db", raw: "[2001:db8::1]:5432", invalid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			specs := Parse(tt.raw)
			if len(specs) != 1 {
				t.Fatalf("len(specs) = %d, want 1", len(specs))
			}
			s := specs[0]
			if tt.invalid {
				if s.Invalid == "" {
					t.Fatalf("want invalid, got valid: %+v", s)
				}
				return
			}
			if s.Invalid != "" {
				t.Fatalf("want valid, got invalid: %q", s.Invalid)
			}
			if s.Host != tt.wantHost {
				t.Errorf("host = %q, want %q", s.Host, tt.wantHost)
			}
			if s.Port != tt.wantPort {
				t.Errorf("port = %d, want %d", s.Port, tt.wantPort)
			}
		})
	}
}

func TestParseSpecsIPv6Canonicalization(t *testing.T) {
	specs := Parse("[2001:DB8::1]:5432:app:ro [2001:db8:0:0:0:0:0:1]:5432:app:ro")
	if len(specs) != 2 {
		t.Fatalf("len(specs) = %d, want 2", len(specs))
	}
	if specs[0].Invalid != "" {
		t.Fatalf("first spec invalid: %q", specs[0].Invalid)
	}
	if specs[0].Host != "2001:db8::1" {
		t.Errorf("canonical host = %q, want 2001:db8::1", specs[0].Host)
	}
	if !specs[1].Duplicate {
		t.Errorf("the second spelling of the same address should be detected as a duplicate identity")
	}
}

func TestParseSpecsBracketedIPv4UnifiesWithBare(t *testing.T) {
	specs := Parse("[192.0.2.1]:5432:app:ro 192.0.2.1:5432:app:ro")
	if len(specs) != 2 {
		t.Fatalf("len(specs) = %d, want 2", len(specs))
	}
	if specs[0].Host != "192.0.2.1" {
		t.Errorf("host = %q, want 192.0.2.1", specs[0].Host)
	}
	if !specs[1].Duplicate {
		t.Errorf("bracketed and bare IPv4 of the same address should dedup to one identity")
	}
}

// Rejected at parse, not mid-run: the <host>_<port> pair becomes a directory name.
func TestParseSpecsOverlongHostRejected(t *testing.T) {
	long := strings.Repeat("a", 260)
	specs := Parse(long + ":5432:app:ro")
	if len(specs) != 1 {
		t.Fatalf("len(specs) = %d, want 1", len(specs))
	}
	if specs[0].Invalid == "" {
		t.Fatal("want invalid for an over-long host")
	}
	if !strings.Contains(specs[0].Invalid, "too long") {
		t.Errorf("invalid reason = %q, want it to mention length", specs[0].Invalid)
	}
}

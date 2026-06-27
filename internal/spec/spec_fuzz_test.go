package spec

import (
	"net"
	"strings"
	"testing"
	"unicode"
)

// FuzzParseSpecs asserts the untrusted-input invariants: ParseSpecs never
// panics, yields exactly one DBSpec per whitespace token, and any spec that is
// NOT marked Invalid satisfies the full grammar (so it is safe to pass to
// pg_dump). This is the boundary between operator config and a process argv.
func FuzzParseSpecs(f *testing.F) {
	seeds := []string{
		"", "   ", "host:db:user", "host:5432:db:user",
		"-bad:db:user", "host:..:user", "host:0:db:user",
		"a:b:c d:e:f", "host:db:user host:db:user",
		"a\x01b:db:user", "::::", "h:" + strings.Repeat("x", 300) + ":u",
		"[2001:db8::1]:5432:db:user", "[::1]:db:user", "[fe80::1%eth0]:5432:db:user",
		"[192.0.2.1]:5432:db:user", "[bad]:5432:db:user", "[]:db:user", "[2001:db8::1]db:user",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		specs := ParseSpecs(raw)

		if want := len(strings.Fields(raw)); len(specs) != want {
			t.Fatalf("got %d specs, want %d (one per whitespace token)", len(specs), want)
		}

		for _, s := range specs {
			if s.Invalid != "" {
				continue
			}
			if s.Port < 1 || s.Port > 65535 {
				t.Fatalf("valid spec has out-of-range port %d", s.Port)
			}
			assertSafeHost(t, s.Host)
			assertSafeIdent(t, "dbname", s.DBName)
			assertSafeIdent(t, "user", s.User)
		}
	})
}

func assertSafeHost(t *testing.T, v string) {
	t.Helper()
	if v == "" {
		t.Fatalf("valid spec has empty host")
	}
	// A bracketed IPv6/IPv4 literal is stored canonical and may contain ':' (and
	// '.' for embedded IPv4). net.ParseIP accepts only IP syntax — no '/', no
	// '..', no shell metacharacters — so a parseable IP is path-safe (ServerDir
	// further replaces ':' with '-' and prefixes '@').
	if ip := net.ParseIP(v); ip != nil {
		return
	}
	if strings.HasPrefix(v, "-") || strings.Contains(v, "..") {
		t.Fatalf("valid spec has unsafe host %q", v)
	}
	for _, r := range v {
		ok := unicode.IsLetter(r) && r < 128 || unicode.IsDigit(r) && r < 128 || r == '_' || r == '-' || r == '.'
		if !ok {
			t.Fatalf("valid spec host %q contains illegal rune %q", v, r)
		}
	}
}

func assertSafeIdent(t *testing.T, kind, v string) {
	t.Helper()
	if v == "" || strings.HasPrefix(v, "-") || strings.Contains(v, "..") {
		t.Fatalf("valid spec has unsafe %s %q", kind, v)
	}
	for _, r := range v {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
		if !ok {
			t.Fatalf("valid spec %s %q contains illegal rune %q", kind, v, r)
		}
	}
}

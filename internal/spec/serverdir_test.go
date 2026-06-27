package spec

import (
	"strings"
	"testing"
)

func TestServerDir(t *testing.T) {
	tests := []struct {
		name string
		host string
		want string
		port int
	}{
		{name: "hostname", host: "db.example.com", port: 5432, want: "db.example.com_5432"},
		{name: "service name", host: "authentik-pg", port: 5432, want: "authentik-pg_5432"},
		{name: "ipv4", host: "192.0.2.10", port: 5432, want: "192.0.2.10_5432"},
		{name: "second port", host: "h", port: 5433, want: "h_5433"},
		{name: "ipv6 canonical", host: "2001:db8::1", port: 5432, want: "@2001-db8--1_5432"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ServerDir(tt.host, tt.port); got != tt.want {
				t.Fatalf("ServerDir(%q, %d) = %q, want %q", tt.host, tt.port, got, tt.want)
			}
		})
	}
}

// Distinct (host, port) identities must map to distinct directories, and an
// IPv6 directory must never equal a hostname/IPv4 directory: the '@' prefix
// (which the host grammar can never produce) keeps the two namespaces disjoint,
// so the h-f3 silent overwrite cannot reappear in IPv6 form.
func TestServerDirDisjointAndInjective(t *testing.T) {
	seen := map[string]string{} // dir -> identity label
	add := func(label, host string, port int) {
		t.Helper()
		dir := ServerDir(host, port)
		if prev, ok := seen[dir]; ok {
			t.Fatalf("dir %q produced by both %q and %q", dir, prev, label)
		}
		seen[dir] = label
	}
	add("h:5432", "h", 5432)
	add("h:5433", "h", 5433)
	add("h2:5432", "h2", 5432)
	add("ipv6 a", "2001:db8::1", 5432)
	add("ipv6 b", "2001:db8::2", 5432)
	// A hostname that dash-encodes like the IPv6 body but lacks the '@' prefix.
	add("hostname lookalike", "2001-db8--1", 5432)

	if ServerDir("2001:db8::1", 5432) == ServerDir("2001-db8--1", 5432) {
		t.Fatal("an IPv6 dir collided with a hostname dir; '@' disjointness is broken")
	}
}

// The port is recovered as the trailing digit run, so a host whose name ends in
// digits never aliases another (host, port) pair.
func TestServerDirPortDigitsUnambiguous(t *testing.T) {
	a := ServerDir("h1", 5432) // "h1_5432"
	b := ServerDir("h", 15432) // "h_15432"
	if a == b {
		t.Fatalf("ServerDir collision: %q == %q", a, b)
	}
	if !strings.HasSuffix(a, "_5432") || !strings.HasSuffix(b, "_15432") {
		t.Fatalf("unexpected encodings: %q, %q", a, b)
	}
}

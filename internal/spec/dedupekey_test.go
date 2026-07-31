package spec

import (
	"strconv"
	"strings"
	"testing"

	"github.com/cplieger/keyenc"
)

// The dedupe key in ParseSpecs composes (Host, Port, DBName) through
// keyenc.Join. Host is the only one of the three that can contain keyenc's
// ':' separator: a bracketed spec stores a CANONICAL IPv6 literal there. These
// cases pin that a colon inside the host is never read as a field boundary, so
// two distinct servers are never collapsed into one "duplicate" verdict — a
// collapse would leave a real database undumped while the operator sees a
// duplicate report instead of a missing backup.
func TestParseSpecsDedupeKeyKeepsColonBearingHostsDistinct(t *testing.T) {
	cases := map[string]struct {
		raw     string
		wantDup []bool
	}{
		// The host's trailing group is spelled exactly like the port that
		// follows it, so the naive form's boundary is ambiguous by eye.
		"ipv6 group looks like the port": {
			raw:     "[2001:db8::5432]:5432:app:u [2001:db8::5432:5432]:5432:app:u",
			wantDup: []bool{false, false},
		},
		// Same address, different ports: distinct servers.
		"same ipv6 different port": {
			raw:     "[2001:db8::1]:5432:app:u [2001:db8::1]:5433:app:u",
			wantDup: []bool{false, false},
		},
		// Same address and port, different dbname: distinct databases.
		"same ipv6 server different db": {
			raw:     "[2001:db8::1]:5432:app:u [2001:db8::1]:5432:other:u",
			wantDup: []bool{false, false},
		},
		// An IPv6 host must not share a namespace with a hostname whose
		// spelling mirrors it.
		"ipv6 vs hostname lookalike": {
			raw:     "[::1]:5432:app:u 0-0-1:5432:app:u",
			wantDup: []bool{false, false},
		},
		// The genuine duplicate is still caught: same canonical address (two
		// spellings), same port, same dbname. The user field is not part of
		// the key.
		"genuine ipv6 duplicate across spellings": {
			raw:     "[2001:db8::1]:5432:app:u [2001:0db8:0000::0001]:5432:app:other",
			wantDup: []bool{false, true},
		},
		// And for an ordinary hostname.
		"genuine hostname duplicate": {
			raw:     "db.internal:app:u db.internal:5432:app:u2",
			wantDup: []bool{false, true},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			specs := ParseSpecs(tc.raw)
			if len(specs) != len(tc.wantDup) {
				t.Fatalf("len(specs) = %d, want %d", len(specs), len(tc.wantDup))
			}
			for i, s := range specs {
				if s.Duplicate != tc.wantDup[i] {
					t.Errorf("specs[%d] (%s): Duplicate = %v, want %v (invalid=%q, host=%q, port=%d, db=%q)",
						i, s.Raw, s.Duplicate, tc.wantDup[i], s.Invalid, s.Host, s.Port, s.DBName)
				}
				// A non-duplicate case must be valid, or the Duplicate
				// assertion above would pass for the wrong reason.
				if !tc.wantDup[i] && s.Invalid != "" {
					t.Errorf("specs[%d] (%s): Invalid = %q, want valid", i, s.Raw, s.Invalid)
				}
			}
		})
	}
}

// The dedupe key's bytes are UNCHANGED by the move to keyenc for every
// ordinary spec: a component containing neither ':' nor '\' is emitted
// verbatim, and Host is the only field that can contain either. This pins that
// property against the pre-adoption expression, so the claim in ParseSpecs'
// doc comment cannot go stale silently. An IPv6 host is the one case that does
// change bytes, and it is asserted here too rather than left implied.
func TestParseSpecsDedupeKeyByteIdenticalForColonFreeFields(t *testing.T) {
	naive := func(host string, port int, dbname string) string {
		return host + ":" + strconv.Itoa(port) + ":" + dbname
	}

	unchanged := []struct {
		host   string
		port   int
		dbname string
	}{
		{"db.internal", 5432, "app"},
		{"192.0.2.10", 5432, "app"},
		{"pg_primary-01", 65535, "a_b-c"},
		{"a", 1, "z"},
	}
	for _, c := range unchanged {
		got := keyenc.Join(c.host, strconv.Itoa(c.port), c.dbname)
		if want := naive(c.host, c.port, c.dbname); got != want {
			t.Errorf("keyenc.Join(%q, %d, %q) = %q, want byte-identical to the naive join %q",
				c.host, c.port, c.dbname, got, want)
		}
	}

	// The IPv6 host is where the encoding earns its keep: the colons inside the
	// host are escaped, so they can no longer be read as field boundaries.
	got := keyenc.Join("2001:db8::1", "5432", "app")
	if want := naive("2001:db8::1", 5432, "app"); got == want {
		t.Errorf("keyenc.Join for an IPv6 host = %q, want it to differ from the naive join (colons must be escaped)", got)
	}
	if !strings.Contains(got, `\:`) {
		t.Errorf("keyenc.Join for an IPv6 host = %q, want the host's colons escaped", got)
	}
}

// FuzzParseSpecsDedupeKeyInjective asserts the dedupe key is injective over the
// whole grammar: a spec is marked Duplicate EXACTLY when an earlier valid spec
// in the same list carries the same (Host, Port, DBName) triple. A key that
// collapsed two distinct servers would mark a spec Duplicate whose triple this
// test has not seen; a key that failed to merge a real repeat would leave
// Duplicate false on a triple it has. DB_SPECS is operator-supplied, so this
// runs over untrusted input rather than a hand-picked table.
func FuzzParseSpecsDedupeKeyInjective(f *testing.F) {
	seeds := []string{
		"",
		"host:db:user host:db:user",
		"host:db:user host:5432:db:user",
		"host:db:user host:db:user2 nope",
		"[2001:db8::1]:5432:app:u [2001:db8::1]:5432:app:u",
		"[2001:db8::5432]:5432:app:u [2001:db8::5432:5432]:5432:app:u",
		"[::1]:app:u 0-0-1:5432:app:u",
		"a:1:b:c a:1:b:d a:2:b:c",
		"h:" + strings.Repeat("x", 300) + ":u h:db:u",
		"[fe80::1%eth0]:5432:db:user [fe80::1]:5432:db:user",
		"::::  a:b:c",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	type triple struct {
		host   string
		dbname string
		port   int
	}

	f.Fuzz(func(t *testing.T, raw string) {
		specs := ParseSpecs(raw)
		seen := make(map[triple]string, len(specs))

		for i, s := range specs {
			// A spec reaches the dedupe key iff it passed the grammar, i.e.
			// iff it is still valid or was invalidated BY the dedupe itself.
			if s.Invalid != "" && !s.Duplicate {
				continue
			}
			tr := triple{host: s.Host, dbname: s.DBName, port: s.Port}
			first, repeat := seen[tr]
			switch {
			case repeat && !s.Duplicate:
				t.Fatalf("specs[%d] (%q) repeats the triple of %q but Duplicate = false", i, s.Raw, first)
			case !repeat && s.Duplicate:
				t.Fatalf("specs[%d] (%q) has an unseen triple %+v but Duplicate = true "+
					"(the dedupe key collapsed two distinct servers)", i, s.Raw, tr)
			}
			if !repeat {
				seen[tr] = s.Raw
			}
		}
	})
}

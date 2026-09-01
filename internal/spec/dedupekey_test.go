package spec

import (
	"strconv"
	"strings"
	"testing"

	"github.com/cplieger/keyenc"
)

// Host is the only field of the dedupe key that can contain keyenc's ':'
// separator (a bracketed spec stores a canonical IPv6 literal); a collapse
// here would leave a real database undumped while reporting it as a
// duplicate instead of missing.
func TestParseSpecsDedupeKeyKeepsColonBearingHostsDistinct(t *testing.T) {
	cases := map[string]struct {
		raw     string
		wantDup []bool
	}{
		"ipv6 group looks like the port": {
			raw:     "[2001:db8::5432]:5432:app:u [2001:db8::5432:5432]:5432:app:u",
			wantDup: []bool{false, false},
		},
		"same ipv6 different port": {
			raw:     "[2001:db8::1]:5432:app:u [2001:db8::1]:5433:app:u",
			wantDup: []bool{false, false},
		},
		"same ipv6 server different db": {
			raw:     "[2001:db8::1]:5432:app:u [2001:db8::1]:5432:other:u",
			wantDup: []bool{false, false},
		},
		"ipv6 vs hostname lookalike": {
			raw:     "[::1]:5432:app:u 0-0-1:5432:app:u",
			wantDup: []bool{false, false},
		},
		// User is not part of the dedupe key.
		"genuine ipv6 duplicate across spellings": {
			raw:     "[2001:db8::1]:5432:app:u [2001:0db8:0000::0001]:5432:app:other",
			wantDup: []bool{false, true},
		},
		"genuine hostname duplicate": {
			raw:     "db.internal:app:u db.internal:5432:app:u2",
			wantDup: []bool{false, true},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			specs := Parse(tc.raw)
			if len(specs) != len(tc.wantDup) {
				t.Fatalf("len(specs) = %d, want %d", len(specs), len(tc.wantDup))
			}
			for i, s := range specs {
				if s.Duplicate != tc.wantDup[i] {
					t.Errorf("specs[%d] (%s): Duplicate = %v, want %v (invalid=%q, host=%q, port=%d, db=%q)",
						i, s.Raw, s.Duplicate, tc.wantDup[i], s.Invalid, s.Host, s.Port, s.DBName)
				}
				if !tc.wantDup[i] && s.Invalid != "" {
					t.Errorf("specs[%d] (%s): Invalid = %q, want valid", i, s.Raw, s.Invalid)
				}
			}
		})
	}
}

// keyenc.Join is byte-identical to a naive colon-join for every field that
// contains neither ':' nor '\'; Host is the only field that can contain
// either, and its IPv6 form is the one case that diverges.
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

	got := keyenc.Join("2001:db8::1", "5432", "app")
	if want := naive("2001:db8::1", 5432, "app"); got == want {
		t.Errorf("keyenc.Join for an IPv6 host = %q, want it to differ from the naive join (colons must be escaped)", got)
	}
	if !strings.Contains(got, `\:`) {
		t.Errorf("keyenc.Join for an IPv6 host = %q, want the host's colons escaped", got)
	}
}

// The dedupe key must be injective: a spec is Duplicate exactly when an
// earlier valid spec in the same list shares its (Host, Port, DBName) triple.
// DB_SPECS is operator-supplied, hence the fuzz rather than a hand table.
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
		specs := Parse(raw)
		seen := make(map[triple]string, len(specs))

		for i, s := range specs {
			// Only a spec that passed the grammar (valid, or invalidated by
			// the dedupe itself) reaches the dedupe key.
			if s.Invalid != "" && !s.Duplicate {
				continue
			}
			tr := triple{host: s.Host, dbname: s.DBName, port: s.Port}
			first, repeat := seen[tr]
			switch {
			case repeat && !s.Duplicate:
				t.Fatalf("specs[%d] (%q) repeats the triple of %q but Duplicate = false", i, s.Raw, first)
			case !repeat && s.Duplicate:
				t.Fatalf("specs[%d] (%q) has an unseen triple %+v but Duplicate = true", i, s.Raw, tr)
			}
			if !repeat {
				seen[tr] = s.Raw
			}
		}
	})
}

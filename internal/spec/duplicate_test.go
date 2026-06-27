package spec

import "testing"

// ParseSpecs marks the second of two specs that collide on host:port:dbname as
// a Duplicate (the dedup key excludes the user), keeping the first. The flag is
// what makes invalidResult report a "duplicate" reason rather than a generic
// "invalid", so it must be set on the colliding spec and left false on both the
// keeper and a format-invalid token.
func TestParseSpecsMarksDuplicateFlag(t *testing.T) {
	specs := ParseSpecs("host:db:user host:db:user2 nope")
	if len(specs) != 3 {
		t.Fatalf("len(specs) = %d, want 3", len(specs))
	}
	if specs[0].Duplicate {
		t.Errorf("specs[0].Duplicate = true, want false (first occurrence is the keeper)")
	}
	if !specs[1].Duplicate {
		t.Errorf("specs[1].Duplicate = false, want true (collides on host:port:dbname)")
	}
	if specs[1].Invalid == "" {
		t.Errorf("specs[1].Invalid = %q, want a non-empty duplicate reason", specs[1].Invalid)
	}
	if specs[2].Duplicate {
		t.Errorf("specs[2].Duplicate = true, want false (format-invalid, not a duplicate)")
	}
}

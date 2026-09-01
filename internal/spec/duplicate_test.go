package spec

import "testing"

// ParseSpecs marks the second of two specs colliding on host:port:dbname as
// Duplicate (the dedup key excludes the user), keeping the first.
func TestParseSpecsMarksDuplicateFlag(t *testing.T) {
	specs := Parse("host:db:user host:db:user2 nope")
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

package httpapi

import (
	"maps"
	"testing"

	"github.com/cplieger/go/httpserver/v2/httpservertest"
)

// The package writes exactly codeSpecs plus the httpserver codes it answers
// itself, and every one reaches the client with a remediation.
func TestCodeSpecs_matchSource(t *testing.T) {
	written := maps.Clone(codeSpecs)
	httpservertest.CheckCodeSpecs(t, ".", written)
	for _, code := range []string{"unauthorized", "host_not_allowed", "too_many_auth_failures", "internal_error"} {
		if spec, ok := catalog.Lookup(code); !ok || spec.Remediation == "" {
			t.Errorf("catalog has no remediation for %q", code)
		}
	}
}

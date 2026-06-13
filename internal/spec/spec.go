// Package spec parses and validates the DB_SPECS environment variable: a
// whitespace-separated list of "host[:port]:dbname:user" tuples describing
// which PostgreSQL databases to dump and over which network coordinates.
//
// This is the single validation path. There is no second (shell)
// implementation to drift from, and the rules here are the only thing that
// stands between operator-supplied config and a pg_dump/psql argv. Passwords
// are never part of the grammar: libpq resolves them from .pgpass keyed on
// host:port:dbname:user.
package spec

import (
	"strconv"
	"strings"
)

// DefaultPort is the PostgreSQL port assumed when a spec omits one.
const DefaultPort = 5432

// DBSpec is one validated host[:port]:dbname:user tuple. A spec with a
// non-empty Invalid field failed validation and must never be passed to
// pg_dump; the orchestrator reports it per-DB and skips it.
type DBSpec struct {
	Host    string
	DBName  string
	User    string
	Raw     string // original token, for error reporting and logs
	Invalid string // non-empty => why this spec is invalid
	Port    int
}

// ParseSpecs splits raw on whitespace and validates each tuple. Every token
// yields exactly one DBSpec (valid or Invalid); tokens are never dropped
// silently. Duplicate (host, port, dbname) keys keep the first occurrence and
// mark later ones Invalid. The returned slice preserves input order.
func ParseSpecs(raw string) []DBSpec {
	tokens := strings.Fields(raw)
	specs := make([]DBSpec, 0, len(tokens))
	seen := make(map[string]struct{}, len(tokens))

	for _, tok := range tokens {
		s := parseOne(tok)
		if s.Invalid == "" {
			key := s.Host + ":" + strconv.Itoa(s.Port) + ":" + s.DBName
			if _, dup := seen[key]; dup {
				s.Invalid = "duplicate host:port:dbname (kept first)"
			} else {
				seen[key] = struct{}{}
			}
		}
		specs = append(specs, s)
	}
	return specs
}

// parseOne validates a single colon-separated token. It accepts 3 fields
// (host:dbname:user, port defaults to DefaultPort) or 4 fields
// (host:port:dbname:user, port numeric 1..65535). Any other shape is Invalid.
func parseOne(tok string) DBSpec {
	s := DBSpec{Raw: tok, Port: DefaultPort}
	parts := strings.Split(tok, ":")

	var host, portStr, dbname, user string
	switch len(parts) {
	case 3:
		host, dbname, user = parts[0], parts[1], parts[2]
	case 4:
		host, portStr, dbname, user = parts[0], parts[1], parts[2], parts[3]
	default:
		s.Invalid = "invalid format (want host[:port]:dbname:user)"
		return s
	}

	if reason := validateHost(host); reason != "" {
		s.Invalid = "host: " + reason
		return s
	}
	s.Host = host

	if portStr != "" {
		port, err := strconv.Atoi(portStr)
		if err != nil || port < 1 || port > 65535 {
			s.Invalid = "port: must be an integer in 1..65535"
			return s
		}
		s.Port = port
	}

	if reason := validateIdentifier("dbname", dbname); reason != "" {
		s.Invalid = reason
		return s
	}
	s.DBName = dbname

	if reason := validateIdentifier("user", user); reason != "" {
		s.Invalid = reason
		return s
	}
	s.User = user

	return s
}

// validateHost enforces the host grammar: non-empty, no leading '-', no
// control characters, no ".." traversal, and only [a-zA-Z0-9_.-] (dots permit
// FQDNs and docker service names). IPv6 literals contain ':' and are therefore
// unsupported by this grammar; use a hostname or service name. Returns "" when
// valid, else a human-readable reason.
func validateHost(v string) string {
	if v == "" {
		return "must not be empty"
	}
	if strings.HasPrefix(v, "-") {
		return "must not start with '-'"
	}
	if strings.Contains(v, "..") {
		return "must not contain '..'"
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.':
		default:
			return "must match [a-zA-Z0-9_.-]"
		}
	}
	return ""
}

// validateIdentifier enforces the dbname/user grammar: non-empty, no leading
// '-' (so a value can never be read as a pg_dump flag), no control characters,
// no "..", and only [a-zA-Z0-9_-]. Returns "" when valid, else a reason
// prefixed with kind.
func validateIdentifier(kind, v string) string {
	if v == "" {
		return kind + ": must not be empty"
	}
	if strings.HasPrefix(v, "-") {
		return kind + ": must not start with '-'"
	}
	if strings.Contains(v, "..") {
		return kind + ": must not contain '..'"
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return kind + ": must match [a-zA-Z0-9_-]"
		}
	}
	return ""
}

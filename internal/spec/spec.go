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
	"net"
	"strconv"
	"strings"

	"github.com/cplieger/keyenc"
)

// DefaultPort is the PostgreSQL port assumed when a spec omits one.
const DefaultPort = 5432

// maxServerDirLen bounds the per-server subdirectory name (ServerDir) to the
// common Linux filesystem per-component limit (NAME_MAX = 255 bytes on
// ext4/xfs/btrfs). A spec whose ServerDir would exceed it is rejected at parse
// so an over-long host surfaces as a per-DB "invalid" rather than a generic OS
// error mid-run.
const maxServerDirLen = 255

// DBSpec is one validated host[:port]:dbname:user tuple. A spec with a
// non-empty Invalid field failed validation and must never be passed to
// pg_dump; the orchestrator reports it per-DB and skips it.
type DBSpec struct {
	Host      string
	DBName    string
	User      string
	Raw       string // original token, for error reporting and logs
	Invalid   string // non-empty => why this spec is invalid
	Port      int
	Duplicate bool // true => Invalid because it duplicates an earlier host:port:dbname
}

// Parse splits raw on whitespace and validates each tuple. Every token
// yields exactly one DBSpec (valid or Invalid); tokens are never dropped
// silently. Duplicate (host, port, dbname) keys keep the first occurrence and
// mark later ones Invalid. The returned slice preserves input order.
//
// The dedupe key composes (host, port, dbname) through keyenc.Join. Host is
// the only field that can carry keyenc's ':' separator (a canonical IPv6
// literal); Port and DBName cannot, by grammar. keyenc.Join escapes each
// component, so the encoding stays injective regardless of field content —
// a plain "host:port:dbname" concatenation would not be, if the grammar ever
// widened or a field were reordered.
func Parse(raw string) []DBSpec {
	tokens := strings.Fields(raw)
	specs := make([]DBSpec, 0, len(tokens))
	seen := make(map[string]struct{}, len(tokens))

	for _, tok := range tokens {
		s := parseOne(tok)
		if s.Invalid == "" {
			key := keyenc.Join(s.Host, strconv.Itoa(s.Port), s.DBName)
			if _, dup := seen[key]; dup {
				s.Invalid = "duplicate host:port:dbname (kept first)"
				s.Duplicate = true
			} else {
				seen[key] = struct{}{}
			}
		}
		specs = append(specs, s)
	}
	return specs
}

// parseOne validates a single token. It accepts the plain
// host[:port]:dbname:user form (host a hostname or IPv4 literal) and the
// bracketed IPv6 form [addr][:port]:dbname:user (the brackets disambiguate the
// address colons from the field separators). The port defaults to DefaultPort
// when omitted and is numeric 1..65535 otherwise. Any other shape, an invalid
// host, or a host whose ServerDir would exceed the filesystem component limit
// is Invalid.
func parseOne(tok string) DBSpec {
	s := DBSpec{Raw: tok, Port: DefaultPort}

	host, portStr, dbname, user, invalid := splitFields(tok)
	if invalid != "" {
		s.Invalid = invalid
		return s
	}

	// A bracketed token is an IPv6 (or IPv4) literal: validate with net.ParseIP
	// (rejects zone-ids, "..", "/", shell metacharacters) and store the
	// canonical address so different spellings collapse to one identity.
	if strings.HasPrefix(tok, "[") {
		ip := net.ParseIP(host)
		if ip == nil {
			s.Invalid = "host: invalid IP literal in brackets (zone IDs and non-IP values are rejected)"
			return s
		}
		s.Host = ip.String()
	} else {
		if reason := validateHost(host); reason != "" {
			s.Invalid = "host: " + reason
			return s
		}
		s.Host = host
	}

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

	// The per-server subdirectory name must fit one filesystem path component.
	if len(ServerDir(s.Host, s.Port)) > maxServerDirLen {
		s.Invalid = "host: too long for artifact path (server directory name exceeds " +
			strconv.Itoa(maxServerDirLen) + " bytes)"
		return s
	}

	return s
}

// splitFields extracts (host, portStr, dbname, user) from a token. It handles
// the bracketed IPv6 form "[addr][:port]:dbname:user" and the plain
// "host[:port]:dbname:user" form; portStr is "" when the port is omitted. The
// returned invalid string is non-empty (a human reason) when the token matches
// neither shape. The trailing :dbname:user (and optional :port) is split on ':'
// safely because port, dbname, and user contain no ':'.
func splitFields(tok string) (host, portStr, dbname, user, invalid string) {
	if strings.HasPrefix(tok, "[") {
		end := strings.Index(tok, "]")
		if end < 0 {
			return "", "", "", "", "host: missing ']' for bracketed IP literal"
		}
		host = tok[1:end]
		rest := tok[end+1:]
		if !strings.HasPrefix(rest, ":") {
			return "", "", "", "", "host: expected ':' after ']' (want [addr][:port]:dbname:user)"
		}
		switch parts := strings.Split(rest[1:], ":"); len(parts) {
		case 2:
			return host, "", parts[0], parts[1], ""
		case 3:
			return host, parts[0], parts[1], parts[2], ""
		default:
			return "", "", "", "", "invalid format (want [addr][:port]:dbname:user)"
		}
	}

	switch parts := strings.Split(tok, ":"); len(parts) {
	case 3:
		return parts[0], "", parts[1], parts[2], ""
	case 4:
		return parts[0], parts[1], parts[2], parts[3], ""
	default:
		return "", "", "", "", "invalid format (want host[:port]:dbname:user)"
	}
}

// ServerDir is the per-server subdirectory name under DUMP_DIR for a
// database's dump artifacts, keyed on (host, port) so two databases with the
// same name on different servers never collide on disk. A hostname/IPv4 host
// (grammar has no ':') encodes as "<host>_<port>"; a canonical IPv6 host
// encodes as "@<addr-with-colons-as-dashes>_<port>" — the '@' prefix (outside
// validateHost's grammar) keeps the two namespaces disjoint, and ':'->'-' is
// reversible because a canonical IPv6 literal never contains '-'.
//
// Deliberately NOT routed through cplieger/keyenc, unlike the dedupe key in
// Parse: the output is persisted state (an existing on-disk directory name
// that nothing here migrates) and a filename, where keyenc's ':'/'\' escaping
// would break existing deployments and NAME_MAX budgeting alike. This
// encoding is already injective by construction (see the field reasoning
// above), so keyenc would trade one proven encoding for another at a real
// migration cost.
func ServerDir(host string, port int) string {
	if strings.ContainsRune(host, ':') {
		return "@" + strings.ReplaceAll(host, ":", "-") + "_" + strconv.Itoa(port)
	}
	return host + "_" + strconv.Itoa(port)
}

// validateHost enforces the plain host grammar (hostnames and IPv4 literals):
// non-empty, no leading '-', no control characters, no ".." traversal, and only
// [a-zA-Z0-9_.-] (dots permit FQDNs and docker service names). IPv6 literals
// contain ':' and are handled separately via the bracketed [addr] form in
// parseOne (validated with net.ParseIP), so they never reach this grammar.
// Returns "" when valid, else a human-readable reason.
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

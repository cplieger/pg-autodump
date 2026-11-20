package httpapi

import (
	"net/http"

	"github.com/cplieger/go/httpserver/v2"
	"github.com/cplieger/go/httpwire"
)

// codeSpecs are the codes pg-autodump answers that no library lists;
// httpservertest.CheckCodeSpecs holds them to the source.
var codeSpecs = map[httpserver.ErrorCode]httpserver.CodeSpec{
	"unauthorized":   {Status: http.StatusUnauthorized, Remediation: "Send the AUTH_TOKEN value as Authorization: Bearer <token>.", NoEffect: true},
	codeInProgress:   {Status: http.StatusTooManyRequests, Remediation: "Wait for the running dump to finish, then send the request again.", NoEffect: true},
	codeShuttingDown: {Status: http.StatusServiceUnavailable, Remediation: "Send the request again once pg-autodump has restarted.", NoEffect: true},
	codeCycleFailed:  {Status: http.StatusInternalServerError, Remediation: "Read the 'cycle coordination failed' line in pg-autodump's log, fix what it names, then send the request again.", NoEffect: true},
	// A run with no databases still records a failed run, so the container
	// turns unhealthy: the request had an effect.
	codeNoDatabases: {Status: http.StatusInternalServerError, Remediation: "Set DB_SPECS to the databases to dump, then restart pg-autodump."},
}

// The codes POST /dump refuses a run with. A run that dumped databases,
// whether every dump succeeded or not, answers its text lines instead.
const (
	codeInProgress   httpserver.ErrorCode = "dump_in_progress"
	codeShuttingDown httpserver.ErrorCode = "shutting_down"
	codeCycleFailed  httpserver.ErrorCode = "cycle_coordination_failed"
	codeNoDatabases  httpserver.ErrorCode = "no_databases_configured"
)

// catalog is every code the listener answers, with its status and
// remediation. Its codes are program data, so a refusal to add one is a bug
// in this file and stops the process at start.
var catalog = func() *httpwire.Catalog {
	c := httpwire.NewCatalog()
	for code, spec := range codeSpecs {
		if err := c.Add(code, spec); err != nil {
			panic(err)
		}
	}
	if err := c.SetRemediation("host_not_allowed", "Add the host name you sent to ALLOWED_HOSTS, or send one it lists."); err != nil {
		panic(err)
	}
	return c
}()

// Errors writes every refusal the listener answers in the JSON envelope, at
// the catalog's status and with its remediation.
var Errors = catalog.Responder()

package httpapi

import (
	"net/http"

	"github.com/cplieger/go/httpserver/v2"
	"github.com/cplieger/go/httpwire"
)

// codeSpecs are the codes pg-autodump answers that no library lists;
// httpservertest.CheckCodeSpecs holds them to the source.
var codeSpecs = map[httpserver.ErrorCode]httpserver.CodeSpec{
	"unauthorized": {Status: http.StatusUnauthorized, Remediation: "Send the AUTH_TOKEN value as Authorization: Bearer <token>.", NoEffect: true},
}

// catalog is every code the listener answers, with its status and
// remediation. Its codes are program data, so a refusal to add one is a bug
// in this file and stops the process at start.
var catalog = func() *httpwire.Catalog {
	c := httpwire.NewCatalog()
	for code, spec := range codeSpecs {
		if err := c.Add(string(code), spec); err != nil {
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

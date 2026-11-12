package main

import (
	"fmt"
	"io"
	"time"

	"github.com/cplieger/go/health/v2/probe"
	"github.com/cplieger/pg-autodump/internal/config"
)

// healthProbeTimeout sits inside the Dockerfile's 5s HEALTHCHECK --timeout, so
// a hung endpoint fails with a written reason instead of Docker's kill.
const healthProbeTimeout = 4 * time.Second

// probeHealth is `pg-autodump health`: it exits 0 only when GET /healthz
// answers 2xx within timeout, so Docker and an outside prober read one
// verdict, and a listener that stopped answering fails too.
func probeHealth(stderr io.Writer, getenv func(string) string, timeout time.Duration) int {
	// A config the server refuses to start on leaves nothing listening, which
	// the request below reports.
	cfg, _, _ := config.Load(getenv)
	url, err := probe.LocalURL(cfg.ListenAddr, "/healthz")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "unhealthy: LISTEN_ADDR %q: %v\n", cfg.ListenAddr, err)
		return 1
	}
	return probe.Check(stderr, timeout, url)
}

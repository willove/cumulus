package main

import (
	"net"
	"net/http"
	"time"

	"github.com/willove/cumulus/internal/deep"
	"github.com/willove/cumulus/internal/mcs"
)

// defaultScorerWorkers is the process-wide ceiling used to size the HTTP
// connection pool. The scorer worker caps (mcs/deep) default to 1 — serial —
// so the pool must still be able to hold a few idle sockets for the rare
// concurrent round, without reserving a socket per possible worker.
//
// Source of truth for the caps: envScorerWorkers / envDocWorkers below.
func defaultScorerWorkers() int {
	n := envScorerWorkers()
	if d := envDocWorkers(); d > n {
		n = d
	}
	if n < 2 {
		n = 2 // keep a spare idle connection; a pool of 1 is a pool of 0 in practice
	}
	return n
}

// The two caps live in the packages that consume them; these are the only
// place the command needs to read them, to size the connection pool above.
func envScorerWorkers() int { return mcs.ScorerWorkers() }
func envDocWorkers() int    { return deep.DocWorkers() }

// newPooledHTTPClient returns the single http.Client every LLM call in this
// process shares. ChatClient falls back to building a throwaway client per call
// when HTTPClient is nil, which is what this replaces.
//
// Timeouts: 60s matches the historical per-call value that evalexecutor.go
// already used, so switching the search stack onto a pooled client does not
// silently change the deadline either face applies.
func newPooledHTTPClient(perHost int) *http.Client {
	return &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			// Idle pool is sized for the scorer caps plus headroom; the
			// ceiling is deliberately generous because a cold query issues
			// ~10 calls that may overlap once the scorer is concurrent.
			MaxIdleConns:        4 * perHost,
			MaxIdleConnsPerHost: perHost,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}
}

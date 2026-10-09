// The part of the oxzoo-live contract (DESIGN.md) that release-lab needs:
// health, probe, and CORS.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

var serverRe = regexp.MustCompile(`^s[0-9]+$`)

func release() string {
	r := os.Getenv("OX_RELEASE")
	if r == "" {
		return "unknown"
	}
	return r[:min(12, len(r))]
}

func env() string {
	if e := os.Getenv("OX_ENV"); e != "" {
		return e
	}
	return "local"
}

// serverLabel returns the sN label of a host name, else "local".
func serverLabel(host string) string {
	for _, l := range strings.Split(host, ".") {
		if serverRe.MatchString(l) {
			return l
		}
	}
	return "local"
}

func server() string { return serverLabel(os.Getenv("PUBLIC_HOST")) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}

// origins is a CORS allowlist read from a comma list.
type origins []string

func parseOrigins(s string) origins {
	var o origins
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			o = append(o, p)
		}
	}
	return o
}

// wrap answers CORS for listed origins and the OPTIONS preflight, and lets
// only GET through to h.
func (o origins) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && slices.Contains(o, origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
				w.Header().Set("Access-Control-Max-Age", "600")
			}
		}
		switch r.Method {
		case http.MethodOptions:
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			h.ServeHTTP(w, r)
		default:
			writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		}
	})
}

type check struct {
	ID, Label string
	Env       []string
	Run       func(ctx context.Context) (string, error)
}

type checkResult struct {
	ID     string   `json:"id"`
	Label  string   `json:"label"`
	OK     bool     `json:"ok"`
	MS     int64    `json:"ms"`
	Detail string   `json:"detail,omitempty"`
	Error  string   `json:"error,omitempty"`
	Env    []string `json:"env"`
	Hops   []string `json:"hops"`
}

type plainVar struct {
	Name    string `json:"name"`
	Value   string `json:"value,omitempty"`
	Role    string `json:"role"`
	Missing bool   `json:"missing,omitempty"`
}

// prober serves GET /_zoo/probe, one at a time: a second caller waits up to
// 5 s, then gets 429.
type prober struct {
	checks []check
	vars   []string
	slot   chan struct{}
}

func newProber(checks []check, vars ...string) *prober {
	return &prober{checks: checks, vars: vars, slot: make(chan struct{}, 1)}
}

func (p *prober) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case p.slot <- struct{}{}:
		defer func() { <-p.slot }()
	case <-time.After(5 * time.Second):
		writeJSON(w, 429, map[string]string{"error": "probe busy"})
		return
	case <-r.Context().Done():
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	start := time.Now()
	results := make([]checkResult, len(p.checks))
	var wg sync.WaitGroup
	for i, c := range p.checks {
		wg.Go(func() { results[i] = runCheck(ctx, c) })
	}
	wg.Wait()
	ok := true
	for _, c := range results {
		ok = ok && c.OK
	}
	vars := []plainVar{}
	for _, name := range p.vars {
		v := os.Getenv(name)
		vars = append(vars, plainVar{Name: name, Value: v, Role: "plain", Missing: v == ""})
	}
	writeJSON(w, 200, map[string]any{
		"name": appName, "stack": appStack, "server": server(), "release": release(), "env": env(),
		"ok": ok, "ms": time.Since(start).Milliseconds(), "at": time.Now().UTC().Format(time.RFC3339),
		"checks": results, "vars": vars,
	})
}

func runCheck(ctx context.Context, c check) checkResult {
	res := checkResult{ID: c.ID, Label: c.Label, Env: c.Env, Hops: []string{appName + "@" + server()}}
	for _, name := range c.Env {
		if os.Getenv(name) == "" {
			res.Error = name + " is not set"
			return res
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	start := time.Now()
	detail, err := c.Run(ctx)
	res.MS = time.Since(start).Milliseconds()
	switch {
	case err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.Error = fmt.Sprintf("timeout after %d ms", 5000)
	case err != nil:
		res.Error = err.Error()
	default:
		res.OK, res.Detail = true, detail
	}
	return res
}

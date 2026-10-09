// release-lab: a stdlib-only Go app that shows which release answers, so
// deploys, staging, previews, promote, and rollback can be watched live.
// SIGTERM drains in-flight requests before the process exits.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"
)

const (
	appName  = "release-lab"
	appStack = "Go stdlib net/http"
	maxSlow  = 10000
)

var (
	started  = time.Now()
	pageTmpl = template.Must(template.New("page").Parse(page))
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	host, port := os.Getenv("HOST"), os.Getenv("PORT")
	if host == "" {
		host = "127.0.0.1"
	}
	if port == "" {
		port = "8080"
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("release %s (%s) listening on %s", release(), env(), ln.Addr())
	if err := serve(ctx, ln, routes()); err != nil {
		log.Fatal(err)
	}
	log.Printf("release %s drained and stopped", release())
}

// serve runs until ctx ends, then stops accepting and waits up to 25 s for
// in-flight requests (a /slow call included) to finish.
func serve(ctx context.Context, ln net.Listener, h http.Handler) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Print("SIGTERM: draining in-flight requests")
	sctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func routes() http.Handler {
	panel := parseOrigins(os.Getenv("ZOO_PANEL_ORIGIN"))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", index)
	mux.HandleFunc("GET /slow", slow)
	mux.Handle("/_zoo/health", panel.wrap(http.HandlerFunc(health)))
	mux.Handle("/_zoo/probe", panel.wrap(newProber([]check{
		{ID: "loopback-slow", Label: "GET /slow?ms=50 over loopback", Env: []string{"PORT"}, Run: checkSlow},
		{ID: "release", Label: "Release id present", Env: []string{"OX_RELEASE"}, Run: checkRelease},
		{ID: "env", Label: "Environment and label", Env: []string{"OX_ENV", "RELEASE_LABEL"}, Run: checkEnv},
	}, "RELEASE_LABEL", "ZOO_PANEL_ORIGIN")))
	return mux
}

// buildInfo reports the Go version, the binary's build time, and the VCS
// stamp when the build had one (a build from git archive has none).
func buildInfo() map[string]any {
	b := map[string]any{"runtime": runtime.Version()}
	if exe, err := os.Executable(); err == nil {
		if st, err := os.Stat(exe); err == nil {
			b["built_at"] = st.ModTime().UTC().Format(time.RFC3339)
		}
	}
	vcs := map[string]string{}
	if bi, ok := debug.ReadBuildInfo(); ok {
		b["module"] = bi.Main.Path
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				vcs["revision"] = s.Value
				b["tag"] = s.Value[:min(12, len(s.Value))]
			case "vcs.time":
				vcs["time"] = s.Value
			case "vcs.modified":
				vcs["modified"] = s.Value
			}
		}
	}
	if len(vcs) > 0 {
		b["vcs"] = vcs
	}
	return b
}

func health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"name": appName, "stack": appStack, "server": server(), "release": release(), "env": env(),
		"label": os.Getenv("RELEASE_LABEL"), "pid": os.Getpid(),
		"uptime_s": int(time.Since(started).Seconds()), "started_at": started.UTC().Format(time.RFC3339),
		"build": buildInfo(),
	})
}

// slowMS validates ms as an integer and clamps it to 0..maxSlow.
func slowMS(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || len(raw) > 12 {
		return 0, fmt.Errorf("ms must be an integer from 0 to %d", maxSlow)
	}
	return max(0, min(n, maxSlow)), nil
}

func slow(w http.ResponseWriter, r *http.Request) {
	ms, err := slowMS(r.URL.Query().Get("ms"))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	start := time.Now()
	select {
	case <-time.After(time.Duration(ms) * time.Millisecond):
	case <-r.Context().Done():
		return
	}
	writeJSON(w, 200, map[string]any{"slept_ms": ms, "took_ms": time.Since(start).Milliseconds(),
		"release": release(), "pid": os.Getpid()})
}

var loopback = &http.Client{Timeout: 5 * time.Second}

func checkSlow(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+os.Getenv("PORT")+"/slow?ms=50", nil)
	if err != nil {
		return "", err
	}
	start := time.Now()
	resp, err := loopback.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var got struct {
		SleptMS int    `json:"slept_ms"`
		Release string `json:"release"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || resp.StatusCode != 200 {
		return "", fmt.Errorf("status %d: %v", resp.StatusCode, err)
	}
	took := time.Since(start)
	if got.SleptMS != 50 || took < 50*time.Millisecond || got.Release != release() {
		return "", fmt.Errorf("slept %d ms in %s, release %q", got.SleptMS, took, got.Release)
	}
	return fmt.Sprintf("slept 50 ms, round trip %d ms, same release", took.Milliseconds()), nil
}

func checkRelease(context.Context) (string, error) {
	return "OX_RELEASE " + release(), nil
}

func checkEnv(context.Context) (string, error) {
	switch e := env(); e {
	case "production", "staging", "preview":
		return fmt.Sprintf("OX_ENV %s, RELEASE_LABEL %q", e, os.Getenv("RELEASE_LABEL")), nil
	default:
		return "", fmt.Errorf("OX_ENV %q is not production, staging, or preview", e)
	}
}

func index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := map[string]any{"Release": release(), "Label": os.Getenv("RELEASE_LABEL"), "Env": env(),
		"Server": server(), "Started": started.UTC().Format(time.RFC3339), "PID": os.Getpid()}
	if err := pageTmpl.Execute(w, data); err != nil {
		log.Printf("page: %v", err)
	}
}

const page = `<!doctype html><html><head><meta charset="utf-8"><title>release-lab {{.Release}}</title>
<style>body{font:15px system-ui;margin:2rem;max-width:40rem}dt{color:#666}dd{margin:0 0 .6rem;font-size:1.3rem}code{color:#555}</style></head>
<body><h1>release-lab</h1>
<dl><dt>release</dt><dd><code>{{.Release}}</code></dd><dt>label</dt><dd>{{if .Label}}{{.Label}}{{else}}(RELEASE_LABEL not set){{end}}</dd>
<dt>env</dt><dd>{{.Env}} on {{.Server}}</dd><dt>process</dt><dd>pid {{.PID}}, started {{.Started}}</dd></dl>
<p>Watching <code>/_zoo/health</code> every 500 ms: <span id="w">starting</span></p>
<p>Try <a href="/slow?ms=1500">/slow?ms=1500</a> during a deploy: the old release finishes it before it stops.</p>
<script>
let ok = 0, fail = 0, changes = 0, last = "";
setInterval(async () => {
  try {
    const h = await (await fetch("/_zoo/health", {cache: "no-store"})).json();
    ok++; if (last && h.release !== last) changes++; last = h.release;
  } catch { fail++; }
  document.getElementById("w").textContent = ok + " ok, " + fail + " failed, " + changes + " release changes, now " + last;
}, 500);
</script></body></html>`

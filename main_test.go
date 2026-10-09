package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSlowMS(t *testing.T) {
	for raw, want := range map[string]int{"": 0, "0": 0, "50": 50, "-5": 0, "10000": 10000, "99999": 10000} {
		if got, err := slowMS(raw); err != nil || got != want {
			t.Errorf("%q: %d %v, want %d", raw, got, err, want)
		}
	}
	for _, raw := range []string{"abc", "1.5", "1e3", "99999999999999999999"} {
		if _, err := slowMS(raw); err == nil {
			t.Errorf("%q accepted", raw)
		}
	}
}

// A request in flight when SIGTERM arrives still gets its answer, and the
// listener refuses new connections afterwards.
func TestGracefulShutdownDrains(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, ln, routes()) }()

	got := make(chan int, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/slow?ms=400")
		if err != nil {
			got <- 0
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		got <- resp.StatusCode
	}()
	time.Sleep(100 * time.Millisecond)
	stopped := time.Now()
	cancel()
	if code := <-got; code != 200 {
		t.Fatalf("in-flight request: status %d, want 200", code)
	}
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
	if time.Since(stopped) < 250*time.Millisecond {
		t.Error("shutdown returned before the in-flight request finished")
	}
	if _, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		t.Error("listener still accepts after shutdown")
	}
}

func TestCORSAndHealth(t *testing.T) {
	t.Setenv("ZOO_PANEL_ORIGIN", "https://zoo-control.s1.zoo.sorv.dev")
	t.Setenv("OX_RELEASE", "3f9c2a1b4d5e6f708192")
	t.Setenv("PUBLIC_HOST", "release-lab.s4.zoo.sorv.dev")
	h := routes()
	r := httptest.NewRequest("GET", "/_zoo/health", nil)
	r.Header.Set("Origin", "https://zoo-control.s1.zoo.sorv.dev")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Header().Get("Access-Control-Allow-Origin") != "https://zoo-control.s1.zoo.sorv.dev" || w.Header().Get("Vary") != "Origin" {
		t.Errorf("CORS headers: %v", w.Header())
	}
	if release() != "3f9c2a1b4d5e" || server() != "s4" {
		t.Errorf("release %q server %q", release(), server())
	}
	r = httptest.NewRequest("OPTIONS", "/_zoo/probe", nil)
	r.Header.Set("Origin", "https://other.example")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 204 || w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("unlisted preflight: %d %v", w.Code, w.Header())
	}
}

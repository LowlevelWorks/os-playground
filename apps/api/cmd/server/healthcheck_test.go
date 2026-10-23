package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHealthURLReachesEveryInterfaceOnLoopback(t *testing.T) {
	tests := map[string]string{
		":8080":          "http://127.0.0.1:8080/healthz",
		"0.0.0.0:8080":   "http://127.0.0.1:8080/healthz",
		"[::]:8080":      "http://[::1]:8080/healthz",
		"127.0.0.1:9000": "http://127.0.0.1:9000/healthz",
		"10.0.0.5:8080":  "http://10.0.0.5:8080/healthz",
		"localhost:8080": "http://localhost:8080/healthz",
	}
	for addr, want := range tests {
		if got := healthURL(addr); got != want {
			t.Errorf("healthURL(%q) = %q, want %q", addr, got, want)
		}
	}
}

func TestCheckHealth(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	url := healthURL(strings.TrimPrefix(srv.URL, "http://"))

	if err := checkHealth(context.Background(), srv.Client(), url); err != nil {
		t.Errorf("healthy server: %v", err)
	}

	status = http.StatusServiceUnavailable
	if err := checkHealth(context.Background(), srv.Client(), url); err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("unhealthy server: err = %v, want one with the status", err)
	}
}

func TestCheckHealthGivesUpOnAHungServer(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release) // before Close, which waits for the handler

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := checkHealth(ctx, srv.Client(), srv.URL+"/healthz"); err == nil {
		t.Fatal("checkHealth = nil for a server that never answers")
	}
}

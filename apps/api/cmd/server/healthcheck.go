package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/os-playground-api/internal/config"
)

// healthcheckTimeout stays under the Dockerfile's HEALTHCHECK --timeout, so a
// hung server is reported as this error rather than killed by Docker.
const healthcheckTimeout = 3 * time.Second

// runHealthcheck is "server -healthcheck": it asks the running server's
// /healthz, found from the same configuration the server reads, so a
// container needs no curl. It does not call Validate: whether the image
// exists is the server's concern, and it would have failed to start.
func runHealthcheck() error {
	loadDotEnv()
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), healthcheckTimeout)
	defer cancel()
	return checkHealth(ctx, http.DefaultClient, healthURL(cfg.ListenAddr))
}

// healthURL turns a listen address into one to connect to. A server bound to
// every interface (":8080", "0.0.0.0:8080", "[::]:8080") is reached on
// loopback.
func healthURL(listenAddr string) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "http://" + listenAddr + "/healthz" // config.Load has checked it
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
		if ip != nil && ip.To4() == nil {
			host = "::1"
		}
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz"
}

func checkHealth(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return nil
}

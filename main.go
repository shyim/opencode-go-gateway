// Package main implements a standalone Go gateway that exposes free OpenCode chat models
// through an OpenAI-compatible HTTP API.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"
)

const schemeHTTPS = "https"

func main() {
	listen := flag.String("listen", env("GATEWAY_LISTEN", "127.0.0.1:8080"), "gateway listen address")
	catalogURL := flag.String("catalog-url", env("MODELS_CATALOG_URL", "https://models.opencode.ai/experimental/v2.0/api.json?type=chat"), "OpenCode v2 catalog URL")
	consoleURL := flag.String("console-url", env("OPENCODE_INFERENCE_URL", "https://opencode.ai/inference/openai/v1"), "OpenCode inference base URL")
	goURL := flag.String("go-url", env("OPENCODE_GO_INFERENCE_URL", "https://opencode.ai/inference/go/openai/v1"), "OpenCode Go inference base URL")
	timeout := flag.Duration("timeout", 3*time.Minute, "maximum request duration")

	flag.Parse()

	catalog, err := parseURL(*catalogURL, true)
	if err != nil {
		log.Fatalf("catalog URL: %v", err)
	}

	console, err := parseURL(*consoleURL, false)
	if err != nil {
		log.Fatalf("console URL: %v", err)
	}

	goBase, err := parseURL(*goURL, false)
	if err != nil {
		log.Fatalf("Go inference URL: %v", err)
	}

	if *timeout <= 0 {
		log.Fatal("timeout must be positive")
	}

	gateway := &gateway{
		upstream: newUpstream(catalog, console, goBase, env("OPENCODE_API_KEY", "public")),
		apiKey:   os.Getenv("GATEWAY_API_KEY"),
		timeout:  *timeout,
	}

	version := env("OPENCODE_CLIENT_VERSION", defaultClientVersion)
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`).MatchString(version) {
		log.Fatal("invalid OpenCode client version")
	}

	gateway.upstream.clientVersion = version

	for name, value := range map[string]string{
		"OPENCODE_CLIENT":         env("OPENCODE_CLIENT", "cli"),
		"OPENCODE_CLIENT_CHANNEL": env("OPENCODE_CLIENT_CHANNEL", "latest"),
	} {
		if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`).MatchString(value) {
			log.Fatalf("invalid %s", name)
		}
	}

	gateway.upstream.clientName = env("OPENCODE_CLIENT", "cli")
	gateway.upstream.clientChannel = env("OPENCODE_CLIENT_CHANNEL", "latest")
	server := &http.Server{
		Addr:              *listen,
		Handler:           gateway.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()

		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		_ = server.Shutdown(shutdown)
	}()

	log.Printf("listening on %s", *listen)

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("server error: %v", err)
	}
}

func parseURL(raw string, allowQuery bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != schemeHTTPS) || u.User != nil || u.Fragment != "" || (!allowQuery && u.RawQuery != "") {
		return nil, errors.New("expected an HTTP(S) URL without credentials or fragment")
	}

	return u, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}

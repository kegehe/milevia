package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kegehe/milevia/apps/cloud-control/internal/cloud"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	databaseURL := os.Getenv("MILEVIA_CLOUD_DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("MILEVIA_CLOUD_DATABASE_URL is required")
	}
	agentTokens, err := agentTokensFromEnv(os.Getenv("MILEVIA_CLOUD_AGENT_TOKENS"))
	if err != nil {
		log.Fatal(err)
	}
	server, err := cloud.New(ctx, cloud.Config{
		DatabaseURL: databaseURL,
		AgentTokens: agentTokens,
		UserToken:   os.Getenv("MILEVIA_CLOUD_USER_TOKEN"),
		AppURL:      os.Getenv("MILEVIA_CLOUD_APP_URL"),
	})
	if err != nil {
		log.Fatal(err)
	}
	defer server.Close()
	addr := os.Getenv("MILEVIA_CLOUD_ADDR")
	if addr == "" {
		addr = ":8090"
	}
	// Do not set ReadTimeout/WriteTimeout here: upgraded WebSocket requests are
	// long-lived and must not be terminated by an HTTP request deadline.
	httpServer := &http.Server{Addr: addr, Handler: server.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	log.Printf("cloud control listening on %s", addr)
	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.ListenAndServe() }()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("cloud control graceful shutdown failed: %v", err)
		}
	}
}

func agentTokensFromEnv(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("MILEVIA_CLOUD_AGENT_TOKENS is required and must be a JSON object")
	}
	var tokens map[string]string
	if err := json.Unmarshal([]byte(raw), &tokens); err != nil {
		return nil, fmt.Errorf("invalid MILEVIA_CLOUD_AGENT_TOKENS: %w", err)
	}
	if len(tokens) == 0 {
		return nil, errors.New("MILEVIA_CLOUD_AGENT_TOKENS must contain at least one instance token")
	}
	normalized := make(map[string]string, len(tokens))
	for instanceID, token := range tokens {
		instanceID = strings.TrimSpace(instanceID)
		token = strings.TrimSpace(token)
		if instanceID == "" || token == "" {
			return nil, errors.New("MILEVIA_CLOUD_AGENT_TOKENS cannot contain empty instance IDs or tokens")
		}
		normalized[instanceID] = token
	}
	if len(normalized) != len(tokens) {
		return nil, errors.New("MILEVIA_CLOUD_AGENT_TOKENS contains duplicate instance IDs after trimming")
	}
	return normalized, nil
}

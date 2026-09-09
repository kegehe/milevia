package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/kegehe/milevia/apps/agent/internal/agent"
)

func main() {
	config := agent.ConfigFromEnv()
	hasStoredCredential := false
	if config.CredentialFile != "" {
		if _, err := os.Stat(config.CredentialFile); err == nil {
			hasStoredCredential = true
		}
	}
	if config.CloudURL == "" || config.LocalURL == "" || (!hasStoredCredential && config.EnrollmentToken == "" && (config.InstanceID == "" || config.CloudToken == "")) {
		log.Fatal("MILEVIA_CLOUD_URL and MILEVIA_LOCAL_URL are required; provide an enrollment token or existing agent credentials")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := agent.New(config).Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

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
	if config.InstanceID == "" || config.CloudURL == "" || config.CloudToken == "" || config.LocalURL == "" {
		log.Fatal("MILEVIA_INSTANCE_ID, MILEVIA_CLOUD_URL, MILEVIA_CLOUD_AGENT_TOKEN and MILEVIA_LOCAL_URL are required")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := agent.New(config).Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

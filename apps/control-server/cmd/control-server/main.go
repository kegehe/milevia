package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/tangmaoke/milevia/apps/control-server/internal/app"
)

func main() {
	server, err := app.New(context.Background(), app.ConfigFromEnv())
	if err != nil {
		log.Fatal(err)
	}
	defer server.Close()

	addr := os.Getenv("AUTO_HTTP_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	log.Printf("control server listening on http://%s", addr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		log.Printf("control server shutting down: %v", ctx.Err())
		server.Close()
	}()
	if err := server.Listen(addr); err != nil {
		log.Printf("control server stopped with error: %v", err)
	}
}

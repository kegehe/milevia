package main

import (
	"context"
	"log"
	"os"

	"github.com/tangmaoke/auto/apps/control-server/internal/app"
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
	log.Fatal(server.Listen(addr))
}

package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/Cadoranee/Doverka/internal/config"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	if err := run(ctx, cfg); err != nil {
		log.Fatal(err)
	}
}

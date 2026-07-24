package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		cancel()
	}()

	if err := run(ctx); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

func run(ctx context.Context) error {
	log.Println("code-reviewer starting...")
	<-ctx.Done()
	log.Println("code-reviewer shutting down...")
	return nil
}

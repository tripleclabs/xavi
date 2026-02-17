package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/3clabs/xavi/internal/agent"
)

func main() {
	var authToken string
	flag.StringVar(&authToken, "auth", "", "Authentication token for the Xavi agent")
	flag.Parse()

	// Setup signal handling for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		log.Printf("Received signal %s, shutting down...", sig)
		cancel()
	}()

	log.Println("Starting Xavi Agent...")
	a, err := agent.New(authToken)
	if err != nil {
		log.Fatalf("Failed to initialize agent: %v", err)
	}
	defer a.Close()

	if err := a.Run(ctx); err != nil {
		log.Fatalf("Agent runtime error: %v", err)
	}
	log.Println("Xavi Agent stopped.")
}

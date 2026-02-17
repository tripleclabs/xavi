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
	var configDir string
	flag.StringVar(&authToken, "auth", "", "Authentication token for the Xavi agent")
	flag.StringVar(&configDir, "config-dir", "/etc/tripleclabs", "Directory for configuration files")
	flag.StringVar(&configDir, "c", "/etc/tripleclabs", "Directory for configuration files (shorthand)")
	flag.Parse()

	// Handle the case where -c is used but --config-dir is also present (or vice versa)
	// Visit will only iterate over flags that have been set.
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "c" || f.Name == "config-dir" {
			configDir = f.Value.String()
		}
	})

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
	a, err := agent.New(authToken, configDir)
	if err != nil {
		log.Fatalf("Failed to initialize agent: %v", err)
	}
	defer a.Close()

	if err := a.Run(ctx); err != nil {
		log.Fatalf("Agent runtime error: %v", err)
	}
	log.Println("Xavi Agent stopped.")
}

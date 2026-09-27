package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/wardstone-project/wardstone/internal/tui"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "wardstone-tui:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("wardstone-tui", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	apiURL := flags.String("url", valueOrDefault("WARDSTONE_API_URL", "http://127.0.0.1:8080"), "Wardstone operator API URL")
	actor := flags.String("actor", defaultActor(), "operator identity recorded with approval decisions")
	refresh := flags.Duration("refresh", 15*time.Second, "automatic refresh interval")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *refresh < time.Second {
		return fmt.Errorf("refresh interval must be at least 1s")
	}

	client, err := tui.NewClient(*apiURL, os.Getenv("WARDSTONE_OPERATOR_TOKEN"), &http.Client{Timeout: 10 * time.Second})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return tui.Run(ctx, client, tui.Options{Actor: *actor, RefreshInterval: *refresh})
}

func defaultActor() string {
	if value := strings.TrimSpace(os.Getenv("WARDSTONE_OPERATOR_ACTOR")); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("USER")); value != "" {
		return value
	}
	return "terminal-admin"
}

func valueOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// Command synapse is a stateless, high-performance protocol adapter
// exposing the OpenAI Responses API on the client side and the OpenAI Chat
// Completions API on the upstream side. It forwards client Authorization
// headers verbatim and manages no credentials, sessions, or accounts.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dncore/synapse-protocol-adapter/internal/config"
	"github.com/dncore/synapse-protocol-adapter/internal/metrics"
	"github.com/dncore/synapse-protocol-adapter/internal/server"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "check-config" {
		if err := runCheckConfig(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "config check failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("config OK")
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := runHealthcheck(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "healthcheck failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	fs := flag.NewFlagSet("synapse", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to YAML config file (environment variables override)")
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *showVersion {
		fmt.Println("synapse " + version)
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration:\n%v\n", err)
		os.Exit(1)
	}

	logger := newLogger(cfg.Log)
	reg := metrics.NewRegistry()

	srv := server.New(cfg, logger, reg)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := srv.Run(ctx); err != nil {
		logger.Error("server exited", "err", err.Error())
		os.Exit(1)
	}
}

// runCheckConfig loads and validates a config without starting the server.
func runCheckConfig(args []string) error {
	fs := flag.NewFlagSet("check-config", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to YAML config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	fmt.Print(config.Describe(&cfg))
	return nil
}

// runHealthcheck GETs the health endpoint; used by the Docker HEALTHCHECK
// (distroless images ship no curl/wget).
func runHealthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	url := fs.String("url", "http://127.0.0.1:8787/health", "health endpoint URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(*url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("health endpoint returned %d", resp.StatusCode)
	}
	return nil
}

func newLogger(cfg config.Log) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(cfg.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	if strings.ToLower(cfg.Format) == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}

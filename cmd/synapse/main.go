// Command synapse is a stateless, high-performance protocol adapter
// exposing the OpenAI Responses API on the client side and the OpenAI Chat
// Completions API on the upstream side. It forwards client Authorization
// headers verbatim and manages no credentials, sessions, or accounts.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/dncore/synapse-protocol-adapter/internal/config"
	"github.com/dncore/synapse-protocol-adapter/internal/metrics"
	"github.com/dncore/synapse-protocol-adapter/internal/server"
	"github.com/dncore/synapse-protocol-adapter/internal/service"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch sub := os.Args[1]; sub {
		case "check-config":
			if err := runCheckConfig(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "config check failed: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("config OK")
			return
		case "healthcheck":
			if err := runHealthcheck(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "healthcheck failed: %v\n", err)
				os.Exit(1)
			}
			return
		case "init":
			if err := runInit(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "init failed: %v\n", err)
				os.Exit(1)
			}
			return
		case "start", "stop", "restart":
			var err error
			switch sub {
			case "start":
				err = service.Start()
			case "stop":
				err = service.Stop()
			case "restart":
				err = service.Restart()
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s failed: %v\n(is the service installed? `synapse service install`)\n", sub, err)
				os.Exit(1)
			}
			return
		case "status":
			if err := runStatus(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "status failed: %v\n", err)
				os.Exit(1)
			}
			return
		case "service":
			if err := runService(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "service failed: %v\n", err)
				os.Exit(1)
			}
			return
		case "-h", "--help", "help":
			usage()
			return
		}
		if !strings.HasPrefix(os.Args[1], "-") {
			fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
			usage()
			os.Exit(2)
		}
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
	server.Version = version // surface the build version on GET /version

	srv := server.New(cfg, logger, reg)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := srv.Run(ctx); err != nil {
		logger.Error("server exited", "err", err.Error())
		os.Exit(1)
	}
}

// usage prints the command surface. Service subcommands target the
// daemon installed via `synapse service install`.
func usage() {
	fmt.Print(`synapse — OpenAI Responses API to Chat Completions protocol adapter

Usage:
  synapse [--config PATH]              run in the foreground
  synapse init [--config PATH]         write the annotated default config
  synapse start | stop | restart       control the installed service
  synapse status [--config PATH]       service state + health + version
  synapse service install [--config PATH] [--now]
                                       install as an autostart service
                                       (systemd user unit / launchd agent)
  synapse service uninstall            remove the service (keeps config)
  synapse check-config [--config PATH] validate a config and exit
  synapse healthcheck [--url URL] [--config PATH]
                                       probe a running daemon (Docker HEALTHCHECK);
                                       default URL follows server.tls (https)
  synapse --version                    print version and exit

The default config path for service commands is
~/.config/synapse/config.yaml (see ` + "`synapse init`" + `). Every setting can
also be set via PROXY_* environment variables.
`)
}

// defaultServiceConfig resolves the config path service commands use
// when --config is not given.
func defaultServiceConfig() string {
	p, err := service.DetectPaths(runtime.GOOS)
	if err != nil {
		return "" // unsupported platform; commands will report the error
	}
	return p.Config
}

// runInit writes the annotated example config if not already present.
func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	configPath := fs.String("config", defaultServiceConfig(), "config file to create")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := os.Stat(*configPath); err == nil {
		return fmt.Errorf("%s already exists — edit it, or `synapse check-config --config %s` to validate", *configPath, *configPath)
	}
	if err := os.MkdirAll(filepath.Dir(*configPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(*configPath, []byte(config.ExampleConfig), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n\nnext steps:\n  1. edit it (upstream.base_url at minimum)\n  2. synapse check-config --config %s\n  3. synapse service install --now\n  4. synapse status\n", *configPath, *configPath)
	return nil
}

// runStatus prints the service/health summary.
func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	configPath := fs.String("config", defaultServiceConfig(), "config file the service runs with")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return service.Status(*configPath)
}

// runService dispatches `synapse service install|uninstall`.
func runService(args []string) error {
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "install":
		fs := flag.NewFlagSet("service install", flag.ContinueOnError)
		configPath := fs.String("config", "", "config file (default: `synapse init` location)")
		now := fs.Bool("now", false, "start immediately after installing")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return service.Install(*configPath, *now)
	case "uninstall":
		return service.Uninstall()
	default:
		fmt.Fprintf(os.Stderr, "unknown service subcommand %q (want install|uninstall)\n", args[0])
		os.Exit(2)
	}
	return nil
}

// runCheckConfig loads and validates a config without starting the server.
func runCheckConfig(args []string) error {
	fs := flag.NewFlagSet("check-config", flag.ContinueOnError)
	// Same default as init/status/service: the user config file. (An
	// empty default here made bare `synapse check-config` print built-in
	// defaults while the daemon read the edited ~/.config file.)
	configPath := fs.String("config", defaultServiceConfig(), "path to YAML config file")
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
// (distroless images ship no curl/wget). The default URL is derived from
// the config — https and skip-verify when server.tls is enabled (self-
// probe of the daemon's own liveness).
func runHealthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	url := fs.String("url", "", "health endpoint URL (default: derived from config)")
	configPath := fs.String("config", "", "config file the daemon runs with (default: `synapse init` location if present)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	target := *url
	client := &http.Client{Timeout: 3 * time.Second}
	if target == "" {
		// Same resolution as status: the user config file when present
		// (Docker mounts it elsewhere and drives TLS via env instead),
		// otherwise built-in defaults + env.
		path := *configPath
		if path == "" {
			path = defaultServiceConfig()
		}
		if _, err := os.Stat(path); err != nil {
			path = "" // absent: fall back to defaults + env
		}
		cfg, err := config.Load(path)
		if err != nil {
			return err
		}
		host, port, _ := net.SplitHostPort(cfg.Server.Listen)
		if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
			host = "127.0.0.1"
		}
		scheme := "http"
		if cfg.Server.TLS.Enabled {
			scheme = "https"
			client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
		}
		target = scheme + "://" + net.JoinHostPort(host, port) + "/health"
	} else if strings.HasPrefix(target, "https://") {
		// Explicit https URL: a liveness self-probe must not fail on the
		// self-signed certificate it is probing.
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	resp, err := client.Get(target)
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

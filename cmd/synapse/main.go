// Command synapse is a stateless, high-performance protocol adapter
// exposing the OpenAI Responses API on the client side and the OpenAI Chat
// Completions API on the upstream side. It forwards client Authorization
// headers verbatim and manages no credentials, sessions, or accounts.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/dncore/synapse-protocol-adapter/internal/config"
	"github.com/dncore/synapse-protocol-adapter/internal/metrics"
	"github.com/dncore/synapse-protocol-adapter/internal/server"
	"github.com/dncore/synapse-protocol-adapter/internal/service"
	"github.com/dncore/synapse-protocol-adapter/internal/tlscert"
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
		case "tls":
			if err := runTLS(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "tls failed: %v\n", err)
				os.Exit(1)
			}
			return
		case "users":
			if err := runUsers(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "users failed: %v\n", err)
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
  synapse tls add [--config PATH] <name>...
                                       add a DNS name/IP to the listener
                                       certificate (re-signs the leaf under
                                       the same CA, live via SIGHUP)
  synapse tls list [--config PATH]     show CA, SAN registry, leaf SANs
  synapse users list [--config PATH]   per-API-key queueing: live slots,
                                       queues, and limits of every user
  synapse users set [--config PATH] --concurrency N <name-or-id>
                                       change one user's concurrency limit
                                       live (non-persistent; config file
                                       is the source of truth)
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
		base, tlsOn := daemonBaseURL(cfg)
		if tlsOn {
			client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
		}
		target = base + "/health"
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

// runTLS manages the listener certificate's dynamic SAN registry:
//
//	synapse tls add <name>...   add DNS names / IPs, re-sign the leaf,
//	                            SIGHUP the daemon (live, no drain)
//	synapse tls list            show CA, registry, leaf SANs and expiry
func runTLS(args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: synapse tls add [--config PATH] <name>...\n       synapse tls list [--config PATH]\n       (flags go before the names)")
		os.Exit(2)
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("tls "+sub, flag.ContinueOnError)
	configPath := fs.String("config", defaultServiceConfig(), "config file the daemon runs with")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	path := *configPath
	if _, err := os.Stat(path); err != nil {
		path = "" // absent: fall back to defaults + env
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}

	switch sub {
	case "add":
		if !cfg.Server.TLS.Enabled {
			return errors.New("server.tls.enabled is false — nothing to manage")
		}
		if fs.NArg() == 0 {
			return errors.New("no names given: synapse tls add <dns-or-ip>...")
		}
		added, err := tlscert.AddSANs(cfg.Server.TLS, fs.Args())
		if err != nil {
			return err
		}
		if len(added) == 0 {
			fmt.Println("nothing to add — all names already covered")
			return nil
		}
		for _, n := range added {
			fmt.Println("added:", n)
		}
		if service.HUP() {
			fmt.Println("daemon signaled (SIGHUP) — re-signed certificate is live")
		} else {
			fmt.Println("daemon not detected — apply with `synapse restart` after starting it")
		}
		return nil

	case "list":
		info := tlscert.Describe(cfg.Server.TLS)
		fmt.Printf("mode:      %s\n", info.Mode)
		if info.Mode == "disabled" {
			return nil
		}
		if info.Dir != "" {
			fmt.Printf("dir:       %s\n", info.Dir)
		}
		fmt.Printf("ca:        %s\n", info.CACertPath)
		if info.RegistryPath != "" {
			fmt.Printf("registry:  %s (%d entries)\n", info.RegistryPath, len(info.Registry))
			for _, s := range info.Registry {
				fmt.Printf("  - %s\n", s)
			}
		}
		if !info.LeafNotAfter.IsZero() {
			days := int(time.Until(info.LeafNotAfter).Hours() / 24)
			fmt.Printf("leaf expires: %s (%dd)\n", info.LeafNotAfter.Format("2006-01-02"), days)
			fmt.Printf("leaf DNS:  %s\n", strings.Join(info.DNSNames, ", "))
			fmt.Printf("leaf IPs:  %s\n", strings.Join(info.IPs, ", "))
		}
		fmt.Println("download:  curl -k https://<host>:<port>/ca.pem   (tutorial: /tls-help)")
		return nil

	default:
		return fmt.Errorf("unknown tls subcommand %q (want add|list)", sub)
	}
}

// daemonBaseURL resolves the daemon's base URL from its config (listen
// address + TLS scheme) for local CLI-to-daemon calls. A wildcard listen
// address means localhost. The bool reports whether TLS is on (self-
// signed certificates must be skipped for local probes).
func daemonBaseURL(cfg config.Config) (string, bool) {
	host, port, _ := net.SplitHostPort(cfg.Server.Listen)
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	if cfg.Server.TLS.Enabled {
		return "https://" + net.JoinHostPort(host, port), true
	}
	return "http://" + net.JoinHostPort(host, port), false
}

// daemonClient builds the HTTP client for CLI-to-daemon calls.
func daemonClient(tlsOn bool) *http.Client {
	c := &http.Client{Timeout: 5 * time.Second}
	if tlsOn {
		c.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	return c
}

// runUsers manages per-API-key queueing against the running daemon:
//
//	synapse users list                 live state of every user
//	synapse users set <name> --concurrency N
//	                                   change one limit live (non-persistent)
func runUsers(args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: synapse users list [--config PATH]\n"+
			"       synapse users set [--config PATH] --concurrency N <name-or-id>\n"+
			"       (flags go before the reference)")
		os.Exit(2)
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("users "+sub, flag.ContinueOnError)
	configPath := fs.String("config", defaultServiceConfig(), "config file the daemon runs with")
	concurrency := fs.Int("concurrency", 0, "new concurrency limit (users set)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	path := *configPath
	if _, err := os.Stat(path); err != nil {
		path = "" // absent: fall back to defaults + env
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	base, tlsOn := daemonBaseURL(cfg)
	client := daemonClient(tlsOn)

	switch sub {
	case "list":
		resp, err := client.Get(base + "/users")
		if err != nil {
			return fmt.Errorf("daemon unreachable at %s: %w (is it running?)", base, err)
		}
		defer resp.Body.Close()
		var listing struct {
			Enabled            bool   `json:"enabled"`
			DefaultConcurrency int    `json:"default_concurrency"`
			MaxQueue           int    `json:"max_queue"`
			QueueTimeout       string `json:"queue_timeout"`
			QueueDepth         int64  `json:"queue_depth"`
			Users              []struct {
				ID          string    `json:"id"`
				Name        string    `json:"name"`
				Concurrency int       `json:"concurrency"`
				Active      int       `json:"active"`
				Queued      int       `json:"queued"`
				Requests    int64     `json:"requests_total"`
				QueuedTotal int64     `json:"queued_total"`
				Rejected    int64     `json:"rejected_total"`
				LastSeen    time.Time `json:"last_seen"`
			} `json:"users"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
			return err
		}
		if !listing.Enabled {
			fmt.Println("per-user queueing is disabled (users.enabled is false)")
			return nil
		}
		fmt.Printf("enabled: default concurrency %d, max queue %d, queue timeout %s\n",
			listing.DefaultConcurrency, listing.MaxQueue, listing.QueueTimeout)
		fmt.Printf("queue depth: %d\n\n", listing.QueueDepth)
		tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tLIMIT\tACTIVE\tQUEUED\tREQUESTS\tWAITED\tREJECTED\tLAST SEEN")
		for _, u := range listing.Users {
			last := "-"
			if !u.LastSeen.IsZero() {
				last = u.LastSeen.Format("2006-01-02 15:04:05")
			}
			fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%s\n",
				u.ID, u.Name, u.Concurrency, u.Active, u.Queued, u.Requests, u.QueuedTotal, u.Rejected, last)
		}
		return tw.Flush()

	case "set":
		if fs.NArg() != 1 {
			return errors.New("usage: synapse users set --concurrency N <name-or-id> (flags go before the reference)")
		}
		if *concurrency < 1 {
			return errors.New("--concurrency must be >= 1")
		}
		body, _ := json.Marshal(map[string]int{"concurrency": *concurrency})
		req, err := http.NewRequest(http.MethodPut, base+"/users/"+url.PathEscape(fs.Arg(0)), bytes.NewReader(body))
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("daemon unreachable at %s: %w (is it running?)", base, err)
		}
		defer resp.Body.Close()
		var out struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Concurrency int    `json:"concurrency"`
			Active      int    `json:"active"`
			Queued      int    `json:"queued"`
			Error       string `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return fmt.Errorf("daemon answered %d with unreadable body", resp.StatusCode)
		}
		switch resp.StatusCode {
		case http.StatusOK:
			fmt.Printf("updated %s (%s): concurrency %d — active %d, queued %d\n",
				orDefaultLabel(out.Name, out.ID), out.ID, out.Concurrency, out.Active, out.Queued)
			return nil
		case http.StatusConflict:
			return errors.New(out.Error + " — enable it in the config and restart")
		case http.StatusNotFound:
			return fmt.Errorf("no user matches %q (see `synapse users list`)", fs.Arg(0))
		default:
			return fmt.Errorf("daemon answered %d: %s", resp.StatusCode, orDefaultLabel(out.Error, "update failed"))
		}

	default:
		return fmt.Errorf("unknown users subcommand %q (want list|set)", sub)
	}
}

func orDefaultLabel(v, def string) string {
	if v == "" {
		return def
	}
	return v
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

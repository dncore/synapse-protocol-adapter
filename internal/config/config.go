// Package config loads, env-overrides, and validates the proxy
// configuration. The config deliberately contains no credentials: the
// proxy forwards client Authorization headers verbatim and never stores
// API keys.
package config

import (
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ExampleConfig is the fully annotated default configuration, served by
// `synapse init` so users get a self-documenting starting point. The
// repo-root config.example.yaml symlinks here (single source).
//
//go:embed example.yaml
var ExampleConfig string

// Config is the full proxy configuration tree.
type Config struct {
	Server   Server   `yaml:"server"`
	Upstream Upstream `yaml:"upstream"`
	Limits   Limits   `yaml:"limits"`
	Timeouts Timeouts `yaml:"timeouts"`
	Shutdown Shutdown `yaml:"shutdown"`
	Log      Log      `yaml:"log"`
}

// Server is the listener configuration.
type Server struct {
	Listen string `yaml:"listen"`
	TLS    TLS    `yaml:"tls"`
}

// TLS serves the listener over HTTPS — for agents that require an
// https:// base URL. It is independent of the upstream scheme:
// upstream.base_url keeps its own http:// or https:// as before.
type TLS struct {
	// Enabled serves server.listen over TLS. With CertFile/KeyFile unset,
	// a self-signed CA plus server certificate is generated on first start
	// and persisted under AutoDir (clients import the CA once).
	Enabled bool `yaml:"enabled"`
	// CertFile/KeyFile are PEM files for bring-your-own certificates
	// (Let's Encrypt, internal CA, mkcert). Both must be set together;
	// when set, nothing is generated.
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// AutoDir persists the generated CA and certificate. Empty means the
	// platform default (~/.config/synapse/tls).
	AutoDir string `yaml:"auto_dir"`
	// SANs are extra DNS names or IPs added to the generated certificate
	// (the defaults already cover localhost, the machine hostname, and
	// every interface IP). Ignored with CertFile/KeyFile.
	SANs []string `yaml:"sans"`
}

// Upstream is the target chat completions provider.
type Upstream struct {
	// BaseURL is everything before the completions path, e.g.
	// "http://127.0.0.1:8000/v1".
	BaseURL string `yaml:"base_url"`
	// Path is appended to BaseURL; defaults to /chat/completions.
	Path string `yaml:"path"`
	// ResponsesMode selects how POST /v1/responses is served:
	//   convert     (default) translate to chat completions upstream
	//   passthrough forward to the upstream's native /responses endpoint
	// Set passthrough when the provider already implements the Responses
	// API well; the proxy then adds only transport value (pooling,
	// cancellation, one endpoint) with zero semantic translation.
	ResponsesMode string `yaml:"responses_mode"`
}

// URL returns the absolute completions endpoint.
func (u Upstream) URL() string {
	path := u.Path
	if path == "" {
		path = "/chat/completions"
	}
	return strings.TrimRight(u.BaseURL, "/") + path
}

// Limits bounds resource usage.
type Limits struct {
	// MaxConcurrency caps in-flight proxy requests; further requests
	// queue (backpressure). 0 means unlimited — for trusted/internal
	// deployments that prefer to let the upstream set the pace.
	MaxConcurrency int `yaml:"max_concurrency"`
	MaxBodyBytes   int `yaml:"max_body_bytes"`
	MaxSSELine     int `yaml:"max_sse_line_bytes"`
}

// Timeouts tunes the HTTP server and upstream transport.
type Timeouts struct {
	Connect        time.Duration `yaml:"connect"`
	Request        time.Duration `yaml:"request"`
	Idle           time.Duration `yaml:"idle"`
	StreamWrite    time.Duration `yaml:"stream_write"`
	HeaderWrite    time.Duration `yaml:"header_write"`
}

// Shutdown tunes graceful shutdown.
type Shutdown struct {
	Timeout time.Duration `yaml:"timeout"`
}

// Log tunes logging output.
type Log struct {
	Format string `yaml:"format"` // "json" | "text"
	Level  string `yaml:"level"`  // "debug" | "info" | "warn" | "error"
}

// Defaults returns the built-in configuration.
func Defaults() Config {
	return Config{
		Server:   Server{Listen: "0.0.0.0:8787"},
		Upstream: Upstream{BaseURL: "http://127.0.0.1:8000/v1", ResponsesMode: "convert"},
		Limits: Limits{
			MaxConcurrency: 200,
			MaxBodyBytes:   64 << 20, // 64 MiB: conversation histories can be large
			MaxSSELine:     16 << 20, // 16 MiB max SSE data line
		},
		Timeouts: Timeouts{
			Connect:     10 * time.Second,
			Request:     30 * time.Minute,
			Idle:        5 * time.Minute,
			StreamWrite: 5 * time.Minute,
			HeaderWrite: 60 * time.Second,
		},
		Shutdown: Shutdown{Timeout: 30 * time.Second},
		Log:      Log{Format: "json", Level: "info"},
	}
}

// Load reads the YAML file (if path is non-empty), applies PROXY_*
// environment overrides, then validates. An empty path yields defaults
// plus env overrides.
func Load(path string) (Config, error) {
	cfg := Defaults()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config: %w", err)
		}
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	applyEnv(&cfg)
	if err := Validate(&cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// applyEnv overrides config values from PROXY_<SECTION>_<KEY> environment
// variables, e.g. PROXY_SERVER_LISTEN, PROXY_UPSTREAM_BASE_URL.
func applyEnv(cfg *Config) {
	if v := os.Getenv("PROXY_SERVER_LISTEN"); v != "" {
		cfg.Server.Listen = v
	}
	if v := os.Getenv("PROXY_SERVER_TLS_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.Server.TLS.Enabled = b
		}
	}
	if v := os.Getenv("PROXY_SERVER_TLS_CERT_FILE"); v != "" {
		cfg.Server.TLS.CertFile = v
	}
	if v := os.Getenv("PROXY_SERVER_TLS_KEY_FILE"); v != "" {
		cfg.Server.TLS.KeyFile = v
	}
	if v := os.Getenv("PROXY_SERVER_TLS_AUTO_DIR"); v != "" {
		cfg.Server.TLS.AutoDir = v
	}
	if v := os.Getenv("PROXY_SERVER_TLS_SANS"); v != "" {
		var sans []string
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				sans = append(sans, s)
			}
		}
		cfg.Server.TLS.SANs = sans
	}
	if v := os.Getenv("PROXY_UPSTREAM_BASE_URL"); v != "" {
		cfg.Upstream.BaseURL = v
	}
	if v := os.Getenv("PROXY_UPSTREAM_PATH"); v != "" {
		cfg.Upstream.Path = v
	}
	if v := os.Getenv("PROXY_UPSTREAM_RESPONSES_MODE"); v != "" {
		cfg.Upstream.ResponsesMode = v
	}
	if v := os.Getenv("PROXY_LIMITS_MAX_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Limits.MaxConcurrency = n
		}
	}
	if v := os.Getenv("PROXY_LIMITS_MAX_BODY_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Limits.MaxBodyBytes = n
		}
	}
	if v := os.Getenv("PROXY_TIMEOUTS_CONNECT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Timeouts.Connect = d
		}
	}
	if v := os.Getenv("PROXY_TIMEOUTS_REQUEST"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Timeouts.Request = d
		}
	}
	if v := os.Getenv("PROXY_TIMEOUTS_IDLE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Timeouts.Idle = d
		}
	}
	if v := os.Getenv("PROXY_TIMEOUTS_STREAM_WRITE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Timeouts.StreamWrite = d
		}
	}
	if v := os.Getenv("PROXY_SHUTDOWN_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Shutdown.Timeout = d
		}
	}
	if v := os.Getenv("PROXY_LOG_FORMAT"); v != "" {
		cfg.Log.Format = v
	}
	if v := os.Getenv("PROXY_LOG_LEVEL"); v != "" {
		cfg.Log.Level = v
	}
}

// Validate reports configuration errors with actionable messages.
func Validate(cfg *Config) error {
	var errs []error

	if cfg.Server.Listen == "" {
		errs = append(errs, errors.New("server.listen must be set (e.g. \"0.0.0.0:8787\")"))
	}
	if (cfg.Server.TLS.CertFile == "") != (cfg.Server.TLS.KeyFile == "") {
		errs = append(errs, errors.New("server.tls.cert_file and server.tls.key_file must be set together"))
	}
	if cfg.Server.TLS.CertFile != "" {
		if !cfg.Server.TLS.Enabled {
			errs = append(errs, errors.New("server.tls.cert_file is set but server.tls.enabled is false"))
		}
		for name, f := range map[string]string{"cert_file": cfg.Server.TLS.CertFile, "key_file": cfg.Server.TLS.KeyFile} {
			if _, err := os.Stat(f); err != nil {
				errs = append(errs, fmt.Errorf("server.tls.%s %q is not readable: %v", name, f, err))
			}
		}
	}
	if cfg.Upstream.BaseURL == "" {
		errs = append(errs, errors.New("upstream.base_url must be set (e.g. \"http://127.0.0.1:8000/v1\")"))
	} else {
		u, err := url.Parse(cfg.Upstream.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("upstream.base_url %q is not a valid http(s) URL", cfg.Upstream.BaseURL))
		}
	}
	if cfg.Upstream.ResponsesMode == "" {
		cfg.Upstream.ResponsesMode = "convert"
	}
	switch cfg.Upstream.ResponsesMode {
	case "convert", "passthrough":
	default:
		errs = append(errs, fmt.Errorf("upstream.responses_mode must be \"convert\" or \"passthrough\", got %q", cfg.Upstream.ResponsesMode))
	}
	if cfg.Limits.MaxConcurrency < 0 {
		errs = append(errs, errors.New("limits.max_concurrency must be >= 0 (0 = unlimited)"))
	}
	if cfg.Limits.MaxBodyBytes <= 0 {
		errs = append(errs, errors.New("limits.max_body_bytes must be > 0"))
	}
	if cfg.Timeouts.Connect <= 0 {
		errs = append(errs, errors.New("timeouts.connect must be > 0 (e.g. 10s)"))
	}
	if cfg.Timeouts.Request <= 0 {
		errs = append(errs, errors.New("timeouts.request must be > 0 (e.g. 30m)"))
	}
	if cfg.Timeouts.Idle <= 0 {
		errs = append(errs, errors.New("timeouts.idle must be > 0 (e.g. 5m)"))
	}
	if cfg.Timeouts.StreamWrite <= 0 {
		errs = append(errs, errors.New("timeouts.stream_write must be > 0 (e.g. 5m)"))
	}
	if cfg.Shutdown.Timeout <= 0 {
		errs = append(errs, errors.New("shutdown.timeout must be > 0 (e.g. 30s)"))
	}
	switch cfg.Log.Format {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("log.format must be \"json\" or \"text\", got %q", cfg.Log.Format))
	}
	switch cfg.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level must be debug|info|warn|error, got %q", cfg.Log.Level))
	}

	return errors.Join(errs...)
}

// Describe renders the effective config with secrets-free fields only
// (there are no secret fields by design).
func Describe(cfg *Config) string {
	b, _ := yaml.Marshal(cfg)
	return string(b)
}

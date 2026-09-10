// Package config loads, env-overrides, and validates the proxy
// configuration. The config deliberately contains no credentials: the
// proxy forwards client Authorization headers verbatim and never stores
// API keys.
package config

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
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
	Users    Users    `yaml:"users"`
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

// Users configures per-API-key concurrency queueing: every client key
// identifies a user whose concurrent upstream requests are capped; excess
// requests queue FIFO instead of hitting the upstream's own limits. The
// section is opt-in — absent or disabled means today's behavior (no
// per-user limiting at all).
type Users struct {
	// Enabled turns per-user queueing on. Default false: no limiting.
	Enabled bool `yaml:"enabled"`
	// DefaultConcurrency caps every user's concurrent upstream requests
	// (streaming requests hold their slot until the last byte). Default
	// 90: stay just under gateways that 429 around 100 concurrent.
	DefaultConcurrency int `yaml:"default_concurrency"`
	// MaxQueue bounds how many requests may wait per user; beyond it the
	// proxy answers 429 immediately (with Retry-After). 0 = unlimited.
	MaxQueue int `yaml:"max_queue"`
	// QueueTimeout fails requests that waited longer than this with 429 —
	// queued requests cannot receive any bytes (not even keep-alives), so
	// the wait must end before clients give up and retry. 0 = unlimited.
	QueueTimeout time.Duration `yaml:"queue_timeout"`
	// Keys optionally names users and overrides their concurrency; every
	// key queues at default_concurrency whether listed or not. Entries
	// may carry the raw key (key:) or its hash (key_hash:); raw keys are
	// hashed at load time and never kept in memory or printed by
	// `synapse check-config`.
	Keys []UserKey `yaml:"keys"`
}

// UserKey is one registered user. Exactly one of Key/KeyHash must be set.
type UserKey struct {
	Name string `yaml:"name"`
	// Key is the raw credential as sent by the client (Authorization
	// value, or x-api-key/api-key value). Replaced by KeyHash at load.
	Key string `yaml:"key"`
	// KeyHash is "sha256:<64 hex>" — what Key becomes at load, so
	// credentials never persist in the running config.
	KeyHash string `yaml:"key_hash"`
	// Concurrency overrides users.default_concurrency for this user.
	// 0 = use the default.
	Concurrency int `yaml:"concurrency"`
}

// Timeouts tunes the HTTP server and upstream transport.
type Timeouts struct {
	Connect     time.Duration `yaml:"connect"`
	Request     time.Duration `yaml:"request"`
	Idle        time.Duration `yaml:"idle"`
	StreamWrite time.Duration `yaml:"stream_write"`
	HeaderWrite time.Duration `yaml:"header_write"`
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
		Users: Users{
			Enabled:            false,
			DefaultConcurrency: 90,
			MaxQueue:           128,
			QueueTimeout:       120 * time.Second,
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
	normalizeUsers(&cfg)
	if err := Validate(&cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// HashCredential derives the user identity of a credential value
// ("Bearer sk-...", an x-api-key value, ...): sha256 over the exact bytes
// the client sent, rendered as "sha256:<64 hex>". The proxy never stores
// or logs credentials — only this hash.
func HashCredential(cred string) string {
	sum := sha256.Sum256([]byte(cred))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// normalizeUsers folds raw keys into hashes so no credential survives in
// the running config.
func normalizeUsers(cfg *Config) {
	for i := range cfg.Users.Keys {
		k := &cfg.Users.Keys[i]
		if k.Key != "" {
			k.KeyHash = HashCredential(k.Key)
			k.Key = ""
		} else {
			// Accept a bare hex digest too; canonical form carries the prefix.
			h := strings.TrimPrefix(k.KeyHash, "sha256:")
			if len(h) == 64 {
				k.KeyHash = "sha256:" + strings.ToLower(h)
			}
		}
	}
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
	if v := os.Getenv("PROXY_USERS_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.Users.Enabled = b
		}
	}
	if v := os.Getenv("PROXY_USERS_DEFAULT_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Users.DefaultConcurrency = n
		}
	}
	if v := os.Getenv("PROXY_USERS_MAX_QUEUE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Users.MaxQueue = n
		}
	}
	if v := os.Getenv("PROXY_USERS_QUEUE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Users.QueueTimeout = d
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
	if err := validateUsers(cfg); err != nil {
		errs = append(errs, err)
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

// validateUsers checks the per-user queueing section. Validated whether or
// not the feature is enabled, so `synapse check-config` catches bad values
// before they are switched on.
func validateUsers(cfg *Config) error {
	var errs []error
	u := cfg.Users
	if u.DefaultConcurrency < 1 {
		errs = append(errs, fmt.Errorf("users.default_concurrency must be >= 1, got %d", u.DefaultConcurrency))
	}
	if u.MaxQueue < 0 {
		errs = append(errs, fmt.Errorf("users.max_queue must be >= 0 (0 = unlimited), got %d", u.MaxQueue))
	}
	if u.QueueTimeout <= 0 {
		errs = append(errs, errors.New("users.queue_timeout must be > 0 (e.g. 120s)"))
	}
	names := map[string]bool{}
	seen := map[string]bool{}
	for i, k := range u.Keys {
		if k.Key != "" && k.KeyHash != "" {
			errs = append(errs, fmt.Errorf("users.keys[%d]: set either key or key_hash, not both", i))
			continue
		}
		if k.Key == "" && k.KeyHash == "" {
			errs = append(errs, fmt.Errorf("users.keys[%d]: key or key_hash must be set", i))
			continue
		}
		hash := k.KeyHash
		if hash == "" { // raw key set directly on the struct, not via Load
			hash = HashCredential(k.Key)
		}
		if !strings.HasPrefix(hash, "sha256:") || len(hash) != len("sha256:")+64 {
			errs = append(errs, fmt.Errorf("users.keys[%d].key_hash must be sha256:<64 hex>, got %q", i, k.KeyHash))
			continue
		}
		if seen[hash] {
			errs = append(errs, fmt.Errorf("users.keys[%d]: duplicate key", i))
		}
		seen[hash] = true
		if k.Name != "" {
			if names[k.Name] {
				errs = append(errs, fmt.Errorf("users.keys[%d]: duplicate name %q", i, k.Name))
			}
			names[k.Name] = true
		}
		if k.Concurrency < 0 {
			errs = append(errs, fmt.Errorf("users.keys[%d].concurrency must be >= 0 (0 = default)", i))
		}
	}
	return errors.Join(errs...)
}

// Describe renders the effective config with secrets-free fields only
// (there are no secret fields by design).
func Describe(cfg *Config) string {
	b, _ := yaml.Marshal(cfg)
	return string(b)
}

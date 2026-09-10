package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad_DefaultsWhenNoFile(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "0.0.0.0:8787" {
		t.Fatalf("default listen wrong: %s", cfg.Server.Listen)
	}
	if cfg.Upstream.URL() != "http://127.0.0.1:8000/v1/chat/completions" {
		t.Fatalf("default upstream URL wrong: %s", cfg.Upstream.URL())
	}
}

func TestLoad_YAML(t *testing.T) {
	path := writeTemp(t, `
server:
  listen: "127.0.0.1:9999"
upstream:
  base_url: "http://llm:8000/v1"
  path: "/completions"
limits:
  max_concurrency: 42
timeouts:
  connect: 5s
  request: 10m
shutdown:
  timeout: 15s
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "127.0.0.1:9999" {
		t.Fatalf("listen: %s", cfg.Server.Listen)
	}
	if cfg.Upstream.URL() != "http://llm:8000/v1/completions" {
		t.Fatalf("upstream url: %s", cfg.Upstream.URL())
	}
	if cfg.Limits.MaxConcurrency != 42 || cfg.Timeouts.Connect != 5*time.Second ||
		cfg.Timeouts.Request != 10*time.Minute || cfg.Shutdown.Timeout != 15*time.Second {
		t.Fatalf("values wrong: %+v", cfg)
	}
}

func TestLoad_EnvOverridesFile(t *testing.T) {
	path := writeTemp(t, `
server:
  listen: "127.0.0.1:9999"
upstream:
  base_url: "http://file:8000/v1"
`)
	t.Setenv("PROXY_SERVER_LISTEN", "0.0.0.0:7777")
	t.Setenv("PROXY_UPSTREAM_BASE_URL", "http://env:9000/v1")
	t.Setenv("PROXY_LIMITS_MAX_CONCURRENCY", "7")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "0.0.0.0:7777" {
		t.Fatalf("env listen override failed: %s", cfg.Server.Listen)
	}
	if cfg.Upstream.BaseURL != "http://env:9000/v1" {
		t.Fatalf("env upstream override failed: %s", cfg.Upstream.BaseURL)
	}
	if cfg.Limits.MaxConcurrency != 7 {
		t.Fatalf("env concurrency override failed: %d", cfg.Limits.MaxConcurrency)
	}
}

func TestValidate_Errors(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"no listen", func(c *Config) { c.Server.Listen = "" }, "server.listen"},
		{"bad url", func(c *Config) { c.Upstream.BaseURL = "not a url" }, "upstream.base_url"},
		{"bad scheme", func(c *Config) { c.Upstream.BaseURL = "ftp://x" }, "upstream.base_url"},
		{"negative concurrency", func(c *Config) { c.Limits.MaxConcurrency = -1 }, "max_concurrency"},
		{"zero connect", func(c *Config) { c.Timeouts.Connect = 0 }, "timeouts.connect"},
		{"bad log format", func(c *Config) { c.Log.Format = "syslog" }, "log.format"},
		{"bad responses_mode", func(c *Config) { c.Upstream.ResponsesMode = "both" }, "responses_mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			tc.mut(&cfg)
			err := Validate(&cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error mentioning %q, got %v", tc.want, err)
			}
		})
	}
}

// 0 means unlimited and must validate cleanly.
func TestValidate_UnlimitedConcurrency(t *testing.T) {
	cfg := Defaults()
	cfg.Limits.MaxConcurrency = 0
	if err := Validate(&cfg); err != nil {
		t.Fatalf("max_concurrency 0 (unlimited) must validate, got %v", err)
	}
}

func TestLoad_ResponsesModeNormalizationAndEnv(t *testing.T) {
	// Empty normalizes to convert.
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Upstream.ResponsesMode != "convert" {
		t.Fatalf("empty mode must normalize to convert, got %q", cfg.Upstream.ResponsesMode)
	}

	// YAML value survives; env overrides it.
	path := writeTemp(t, "upstream:\n  base_url: http://x:1/v1\n  responses_mode: passthrough\n")
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Upstream.ResponsesMode != "passthrough" {
		t.Fatalf("yaml mode lost: %q", cfg.Upstream.ResponsesMode)
	}

	t.Setenv("PROXY_UPSTREAM_RESPONSES_MODE", "convert")
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Upstream.ResponsesMode != "convert" {
		t.Fatalf("env override failed: %q", cfg.Upstream.ResponsesMode)
	}
}

func TestLoad_MalformedYAML(t *testing.T) {
	path := writeTemp(t, "server: [unclosed")
	if _, err := Load(path); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestLoad_UsersDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Users.Enabled {
		t.Fatal("per-user queueing must default to disabled")
	}
	if cfg.Users.DefaultConcurrency != 90 || cfg.Users.MaxQueue != 128 || cfg.Users.QueueTimeout != 120*time.Second {
		t.Fatalf("users defaults wrong: %+v", cfg.Users)
	}
}

// Raw keys must be hashed at load: neither the running Config nor
// Describe/check-config output may contain plaintext credentials.
func TestLoad_UsersRawKeyHashedAndWiped(t *testing.T) {
	path := writeTemp(t, `
users:
  enabled: true
  keys:
    - name: dean
      key: "Bearer sk-secret-123"
      concurrency: 12
    - name: ci
      key_hash: "9f3a6b2c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Users.Keys[0].Key != "" {
		t.Fatalf("raw key survived load: %q", cfg.Users.Keys[0].Key)
	}
	want := HashCredential("Bearer sk-secret-123")
	if cfg.Users.Keys[0].KeyHash != want {
		t.Fatalf("key not hashed: %q want %q", cfg.Users.Keys[0].KeyHash, want)
	}
	// Bare 64-hex digests normalize to the sha256:-prefixed form.
	if cfg.Users.Keys[1].KeyHash != "sha256:9f3a6b2c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8" {
		t.Fatalf("bare hash not normalized: %q", cfg.Users.Keys[1].KeyHash)
	}
	if out := Describe(&cfg); strings.Contains(out, "sk-secret-123") {
		t.Fatalf("Describe leaks raw key:\n%s", out)
	}
}

func TestLoad_UsersEnvOverrides(t *testing.T) {
	t.Setenv("PROXY_USERS_ENABLED", "true")
	t.Setenv("PROXY_USERS_DEFAULT_CONCURRENCY", "8")
	t.Setenv("PROXY_USERS_MAX_QUEUE", "7")
	t.Setenv("PROXY_USERS_QUEUE_TIMEOUT", "9s")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Users.Enabled || cfg.Users.DefaultConcurrency != 8 ||
		cfg.Users.MaxQueue != 7 || cfg.Users.QueueTimeout != 9*time.Second {
		t.Fatalf("users env overrides failed: %+v", cfg.Users)
	}
}

func TestValidate_UsersErrors(t *testing.T) {
	hash64 := "9f3a6b2c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8"
	cases := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"zero default concurrency", func(c *Config) { c.Users.DefaultConcurrency = 0 }, "default_concurrency"},
		{"negative queue", func(c *Config) { c.Users.MaxQueue = -1 }, "max_queue"},
		{"zero queue timeout", func(c *Config) { c.Users.QueueTimeout = 0 }, "queue_timeout"},
		{"key entry empty", func(c *Config) {
			c.Users.Keys = []UserKey{{Name: "x"}}
		}, "must be set"},
		{"key and hash both", func(c *Config) {
			c.Users.Keys = []UserKey{{Key: "k", KeyHash: "sha256:" + hash64}}
		}, "not both"},
		{"bad hash", func(c *Config) {
			c.Users.Keys = []UserKey{{KeyHash: "sha256:short"}}
		}, "64 hex"},
		{"duplicate key", func(c *Config) {
			c.Users.Keys = []UserKey{{Key: "same"}, {Key: "same"}}
		}, "duplicate key"},
		{"duplicate name", func(c *Config) {
			c.Users.Keys = []UserKey{{Name: "a", KeyHash: "sha256:" + hash64}, {Name: "a", Key: "other"}}
		}, "duplicate name"},
		{"negative concurrency", func(c *Config) {
			c.Users.Keys = []UserKey{{Key: "k", Concurrency: -1}}
		}, "concurrency"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			tc.mut(&cfg)
			err := Validate(&cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error mentioning %q, got %v", tc.want, err)
			}
		})
	}

	// A well-formed section validates (raw key on the struct is fine too).
	cfg := Defaults()
	cfg.Users.Enabled = true
	cfg.Users.Keys = []UserKey{{Name: "dean", Key: "Bearer sk-x", Concurrency: 12}, {KeyHash: "sha256:" + hash64}}
	if err := Validate(&cfg); err != nil {
		t.Fatalf("valid users config rejected: %v", err)
	}
}

func TestLoad_TLSYAMLAndEnv(t *testing.T) {
	path := writeTemp(t, `
server:
  listen: "127.0.0.1:9999"
  tls:
    enabled: true
    sans: ["box.example.com", "10.1.2.3"]
upstream:
  base_url: "http://llm:8000/v1"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Server.TLS.Enabled || len(cfg.Server.TLS.SANs) != 2 {
		t.Fatalf("tls yaml not parsed: %+v", cfg.Server.TLS)
	}

	t.Setenv("PROXY_SERVER_TLS_ENABLED", "false")
	t.Setenv("PROXY_SERVER_TLS_SANS", " other.example.com ,  10.9.8.7,")
	t.Setenv("PROXY_SERVER_TLS_AUTO_DIR", "/tmp/synapse-tls")
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.TLS.Enabled {
		t.Fatal("env tls enabled override failed")
	}
	if cfg.Server.TLS.AutoDir != "/tmp/synapse-tls" {
		t.Fatalf("env auto_dir override failed: %q", cfg.Server.TLS.AutoDir)
	}
	want := []string{"other.example.com", "10.9.8.7"}
	if len(cfg.Server.TLS.SANs) != len(want) || cfg.Server.TLS.SANs[0] != want[0] || cfg.Server.TLS.SANs[1] != want[1] {
		t.Fatalf("env sans override failed: %v", cfg.Server.TLS.SANs)
	}
}

func TestValidate_TLSErrors(t *testing.T) {
	cert := filepath.Join(t.TempDir(), "cert.pem")
	if err := os.WriteFile(cert, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"cert without key", func(c *Config) {
			c.Server.TLS = TLS{Enabled: true, CertFile: cert}
		}, "must be set together"},
		{"key without cert", func(c *Config) {
			c.Server.TLS = TLS{Enabled: true, KeyFile: cert}
		}, "must be set together"},
		{"missing file", func(c *Config) {
			c.Server.TLS = TLS{Enabled: true, CertFile: "/nonexistent/cert.pem", KeyFile: "/nonexistent/key.pem"}
		}, "is not readable"},
		{"files but disabled", func(c *Config) {
			c.Server.TLS = TLS{CertFile: cert, KeyFile: cert}
		}, "enabled is false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			tc.mut(&cfg)
			err := Validate(&cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error mentioning %q, got %v", tc.want, err)
			}
		})
	}

	// Present, readable files with enabled validate cleanly.
	cfg := Defaults()
	cfg.Server.TLS = TLS{Enabled: true, CertFile: cert, KeyFile: cert}
	if err := Validate(&cfg); err != nil {
		t.Fatalf("valid tls config rejected: %v", err)
	}
}

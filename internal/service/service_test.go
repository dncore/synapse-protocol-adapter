package service

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUnitTextGolden(t *testing.T) {
	got := UnitText(Paths{
		Binary: "/home/u/.npm-global/lib/node_modules/@dncore/synapse-linux-x64/bin/synapse",
		Config: "/home/u/.config/synapse/config.yaml",
	})
	for _, want := range []string{
		"ExecStart=/home/u/.npm-global/lib/node_modules/@dncore/synapse-linux-x64/bin/synapse --config /home/u/.config/synapse/config.yaml",
		"KillSignal=SIGTERM",
		"TimeoutStopSec=35s",
		"Restart=on-failure",
		"WantedBy=default.target", // user unit, not multi-user.target
	} {
		if !strings.Contains(got, want) {
			t.Errorf("unit text missing %q\n---\n%s", want, got)
		}
	}
	// Root-install hardening must not leak into user units.
	for _, banned := range []string{"User=", "ProtectSystem=", "/etc/systemd"} {
		if strings.Contains(got, banned) {
			t.Errorf("user unit should not contain %q", banned)
		}
	}
}

func TestPlistTextGolden(t *testing.T) {
	got := PlistText(Paths{
		Binary: "/Users/u/node_modules/@dncore/synapse-darwin-arm64/bin/synapse",
		Config: "/Users/u/.config/synapse/config.yaml",
		Log:    "/Users/u/Library/Logs/synapse.log",
	})
	for _, want := range []string{
		"<string>com.synapse.proxy</string>",
		"<string>/Users/u/node_modules/@dncore/synapse-darwin-arm64/bin/synapse</string>",
		"<string>--config</string>",
		"<string>/Users/u/.config/synapse/config.yaml</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<string>/Users/u/Library/Logs/synapse.log</string>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plist missing %q\n---\n%s", want, got)
		}
	}
}

func TestDetectPathsPerGOOS(t *testing.T) {
	t.Run("linux", func(t *testing.T) {
		p, err := DetectPaths("linux")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(p.Unit, "/.config/systemd/user/synapse.service") {
			t.Errorf("linux unit path: %s", p.Unit)
		}
		if !strings.HasSuffix(p.Config, "/synapse/config.yaml") {
			t.Errorf("linux config path: %s", p.Config)
		}
		if p.Log != "" {
			t.Errorf("linux logs go to journald, want empty Log, got %q", p.Log)
		}
		if p.Binary == "" {
			t.Error("binary path unresolved")
		}
	})
	t.Run("darwin", func(t *testing.T) {
		p, err := DetectPaths("darwin")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(p.Unit, "/Library/LaunchAgents/com.synapse.proxy.plist") {
			t.Errorf("darwin unit path: %s", p.Unit)
		}
		if !strings.HasSuffix(p.Log, "/Library/Logs/synapse.log") {
			t.Errorf("darwin log path: %s", p.Log)
		}
	})
	t.Run("unsupported", func(t *testing.T) {
		if _, err := DetectPaths("windows"); err != ErrUnsupported {
			t.Errorf("want ErrUnsupported, got %v", err)
		}
	})
}

func TestProbes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health", "/ready":
			w.WriteHeader(http.StatusOK)
		case "/version":
			w.Write([]byte(`{"version":"0.2.0"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	if err := probeOK(srv.Client(), srv.URL+"/health"); err != nil {
		t.Errorf("health probe: %v", err)
	}
	if v, err := probeVersion(srv.Client(), srv.URL+"/version"); err != nil || v != "0.2.0" {
		t.Errorf("version probe: %q, %v", v, err)
	}
	if err := probeOK(srv.Client(), srv.URL+"/nope"); err == nil {
		t.Error("404 probe should fail")
	}
}

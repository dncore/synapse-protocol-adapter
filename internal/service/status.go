package service

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/dncore/synapse-protocol-adapter/internal/config"
)

// Status prints a one-screen summary: service-manager state, config and
// unit locations, effective listen/upstream, and live /health /ready
// /version probes. It reports on bare foreground runs too (probes still
// hit the listen address), so it is useful before `service install`.
func Status(configPath string) error {
	p, err := DetectPaths(runtime.GOOS)
	if err != nil {
		return err
	}
	if configPath != "" {
		p.Config = configPath
	}

	// Effective config: the named file if present, else defaults+env.
	cfg := config.Defaults()
	if _, err := os.Stat(p.Config); err == nil {
		cfg, err = config.Load(p.Config)
		if err != nil {
			fmt.Printf("config:    %s (INVALID: %v)\n", p.Config, err)
		}
	}

	fmt.Printf("service:   %s\n", serviceState(p))
	fmt.Printf("unit:      %s\n", p.Unit)
	fmt.Printf("config:    %s\n", p.Config)

	host, port, _ := net.SplitHostPort(cfg.Server.Listen)
	probeHost := host
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		probeHost = "127.0.0.1"
	}
	fmt.Printf("listen:    %s\n", cfg.Server.Listen)
	fmt.Printf("upstream:  %s (%s)\n", cfg.Upstream.BaseURL, cfg.Upstream.ResponsesMode)

	client := &http.Client{Timeout: 3 * time.Second}
	base := fmt.Sprintf("http://%s", net.JoinHostPort(probeHost, port))
	for _, probe := range []struct{ name, path string }{
		{"health", "/health"},
		{"ready", "/ready"},
	} {
		if err := probeOK(client, base+probe.path); err != nil {
			fmt.Printf("%-9s  unreachable (%v)\n", probe.name+":", err)
		} else {
			fmt.Printf("%-9s  ok\n", probe.name+":")
		}
	}
	if v, err := probeVersion(client, base+"/version"); err != nil {
		fmt.Printf("version:   unreachable (%v)\n", err)
	} else {
		fmt.Printf("version:   %s\n", v)
	}
	return nil
}

// serviceState asks the platform service manager whether the service is
// installed and running.
func serviceState(p Paths) string {
	if _, err := os.Stat(p.Unit); err != nil {
		return "not installed (foreground or `synapse service install`)"
	}
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("launchctl", "print", guiDomain()+"/"+LaunchdLabel).Output()
		if err != nil {
			return "installed, not loaded"
		}
		if strings.Contains(string(out), "pid = ") {
			return "running (launchd agent)"
		}
		return "loaded, not running (launchd agent)"
	}
	out, _ := exec.Command("systemctl", "--user", "is-active", ServiceName).Output()
	state := strings.TrimSpace(string(out))
	if state == "active" {
		return "active (running, systemd user unit)"
	}
	return fmt.Sprintf("%s (systemd user unit)", state)
}

func probeOK(client *http.Client, url string) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func probeVersion(client *http.Client, url string) (string, error) {
	var body struct {
		Version string `json:"version"`
	}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	return body.Version, nil
}

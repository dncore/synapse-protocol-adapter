package server

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/dncore/synapse-protocol-adapter/internal/config"
	"github.com/dncore/synapse-protocol-adapter/internal/metrics"
	"github.com/dncore/synapse-protocol-adapter/internal/tlscert"
)

// newTLSTestServer starts the real mux over TLS with the auto-generated
// certificate material, and returns a client that trusts only the
// generated CA — so the handshake actually verifies, exactly like a
// client that imported ca.pem. The TLS config is returned for tests that
// mutate the material afterwards.
func newTLSTestServer(t *testing.T, upstreamURL string) (*httptest.Server, *http.Client, config.TLS) {
	t.Helper()
	cfg := config.TLS{Enabled: true, AutoDir: t.TempDir()}
	mat, err := tlscert.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	proxyCfg := testConfig(upstreamURL)
	proxyCfg.Server.TLS = cfg // routes like /ca.pem register only when enabled
	s := New(proxyCfg, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.NewRegistry())

	ts := httptest.NewUnstartedServer(s.httpSrv.Handler)
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{mat.Certificate}}
	ts.StartTLS()
	t.Cleanup(ts.Close)

	caPEM, err := os.ReadFile(mat.CACertPath)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("generated CA not parseable")
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}}
	return ts, client, cfg
}

// tlsCfg is the TLS config used by reload tests.
func tlsCfg(t *testing.T) config.TLS {
	t.Helper()
	return config.TLS{Enabled: true, AutoDir: t.TempDir()}
}

// The full convert path works over TLS, verified against the generated CA.
func TestProxy_TLSNonStreaming(t *testing.T) {
	up := &fakeUpstream{}
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()

	proxySrv, client, _ := newTLSTestServer(t, upSrv.URL)

	req, _ := http.NewRequest("POST", proxySrv.URL+"/v1/responses", strings.NewReader(responsesBody(false)))
	req.Header.Set("Authorization", "Bearer tls-secret-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("TLS request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.TLS == nil {
		t.Fatal("connection is not TLS")
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := up.lastAuth; got != "Bearer tls-secret-token" {
		t.Fatalf("Authorization not forwarded: %q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"output"`) {
		t.Fatalf("no converted output: %s", body)
	}
}

// SSE streaming converts and flushes per event over TLS (HTTP/2 is
// negotiated by ServeTLS in production; httptest here negotiates 1.2+).
func TestProxy_TLSStreaming(t *testing.T) {
	up := &fakeUpstream{stream: true}
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()

	proxySrv, client, _ := newTLSTestServer(t, upSrv.URL)

	req, _ := http.NewRequest("POST", proxySrv.URL+"/v1/responses", strings.NewReader(responsesBody(true)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("TLS stream failed: %v", err)
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	var events []string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
	}
	if len(events) == 0 {
		t.Fatal("no SSE events received")
	}
	if events[0] != "response.created" || events[len(events)-1] != "response.completed" {
		t.Fatalf("event sequence wrong: %v", events)
	}
}

// A client that does NOT trust the generated CA is refused by the
// handshake — the certificate is verified, not decorative.
func TestProxy_TLSUntrustedClientRejected(t *testing.T) {
	up := &fakeUpstream{}
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()

	proxySrv, _, _ := newTLSTestServer(t, upSrv.URL)

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: x509.NewCertPool()}, // trusts nothing
	}}
	resp, err := client.Get(proxySrv.URL + "/health")
	if err == nil {
		resp.Body.Close()
		t.Fatal("untrusted client must fail the handshake")
	}
}

// GET /ca.pem serves exactly the generated CA bytes over the TLS listener.
func TestCACertEndpoint(t *testing.T) {
	up := &fakeUpstream{}
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()

	proxySrv, client, cfg := newTLSTestServer(t, upSrv.URL)

	resp, err := client.Get(proxySrv.URL + "/ca.pem")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	want, err := os.ReadFile(tlscert.CACertPath(cfg))
	if err != nil || string(got) != string(want) {
		t.Fatalf("served CA differs from ca.pem (err=%v, %d vs %d bytes)", err, len(got), len(want))
	}
	if !strings.Contains(string(got), "BEGIN CERTIFICATE") {
		t.Fatal("not a PEM certificate")
	}
}

// The tutorial page embeds the address the client actually used.
func TestTLSHelpEndpoint(t *testing.T) {
	up := &fakeUpstream{}
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()

	proxySrv, client, _ := newTLSTestServer(t, upSrv.URL)

	resp, err := client.Get(proxySrv.URL + "/tls-help")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{
		strings.TrimPrefix(proxySrv.URL, "https://") + "/ca.pem",
		"security add-trusted-cert", "update-ca-certificates",
		"trust anchor --store", "Import-Certificate",
		"NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("tls-help missing %q", want)
		}
	}
}

// The ops endpoints are absent without TLS — nothing to bootstrap.
func TestTLSEndpointsNotRegisteredWithoutTLS(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1")
	ts := httptest.NewServer(s.httpSrv.Handler)
	defer ts.Close()
	for _, path := range []string{"/ca.pem", "/tls-help"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: want 404 without tls, got %d", path, resp.StatusCode)
		}
	}
}

// reloadTLS (the SIGHUP path) swaps in a re-signed leaf carrying the new
// SAN — no restart, no CA change.
func TestReloadTLSSwapsCertificate(t *testing.T) {
	up := &fakeUpstream{}
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()

	cfg := testConfig(upSrv.URL)
	cfg.Server.TLS = tlsCfg(t)
	s := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.NewRegistry())

	mat, err := tlscert.Load(cfg.Server.TLS)
	if err != nil {
		t.Fatal(err)
	}
	s.tlsCert.Store(&mat.Certificate)
	before := s.tlsCert.Load()

	if _, err := tlscert.AddSANs(cfg.Server.TLS, []string{"swapped.example.com"}); err != nil {
		t.Fatal(err)
	}
	s.reloadTLS()
	after := s.tlsCert.Load()

	if before == after {
		t.Fatal("certificate pointer not swapped")
	}
	leaf, err := x509.ParseCertificate(after.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range leaf.DNSNames {
		if d == "swapped.example.com" {
			found = true
		}
	}
	if !found {
		t.Fatalf("swapped leaf lacks new SAN: %v", leaf.DNSNames)
	}
}

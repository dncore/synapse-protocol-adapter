package server

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/dncore/synapse-protocol-adapter/internal/config"
	"github.com/dncore/synapse-protocol-adapter/internal/tlscert"
)

// newTLSTestServer starts the real mux over TLS with the auto-generated
// certificate material, and returns a client that trusts only the
// generated CA — so the handshake actually verifies, exactly like a
// client that imported ca.pem.
func newTLSTestServer(t *testing.T, upstreamURL string) (*httptest.Server, *http.Client) {
	t.Helper()
	mat, err := tlscert.Load(config.TLS{Enabled: true, AutoDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, upstreamURL)

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
	return ts, client
}

// The full convert path works over TLS, verified against the generated CA.
func TestProxy_TLSNonStreaming(t *testing.T) {
	up := &fakeUpstream{}
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()

	proxySrv, client := newTLSTestServer(t, upSrv.URL)

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

	proxySrv, client := newTLSTestServer(t, upSrv.URL)

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

	proxySrv, _ := newTLSTestServer(t, upSrv.URL)

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: x509.NewCertPool()}, // trusts nothing
	}}
	resp, err := client.Get(proxySrv.URL + "/health")
	if err == nil {
		resp.Body.Close()
		t.Fatal("untrusted client must fail the handshake")
	}
}

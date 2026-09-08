package tlscert

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dncore/synapse-protocol-adapter/internal/config"
)

func autoCfg(dir string, sans ...string) config.TLS {
	return config.TLS{Enabled: true, AutoDir: dir, SANs: sans}
}

// loadCA parses the generated CA and builds a root pool for verification.
func loadCA(t *testing.T, path string) (*x509.Certificate, *x509.CertPool) {
	t.Helper()
	caPEM, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := parseCertificate(caPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return ca, pool
}

func leafOf(t *testing.T, pair tls.Certificate) *x509.Certificate {
	t.Helper()
	if len(pair.Certificate) == 0 {
		t.Fatal("no leaf certificate")
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func containsDNS(cert *x509.Certificate, name string) bool {
	for _, d := range cert.DNSNames {
		if d == name {
			return true
		}
	}
	return false
}

func containsIP(cert *x509.Certificate, ip string) bool {
	for _, i := range cert.IPAddresses {
		if i.String() == ip {
			return true
		}
	}
	return false
}

func TestAutoGenerate(t *testing.T) {
	dir := t.TempDir()
	mat, err := Load(autoCfg(dir, "box.example.com", "10.1.2.3"))
	if err != nil {
		t.Fatal(err)
	}
	if !mat.GeneratedCA || mat.CACertPath != filepath.Join(dir, "ca.pem") {
		t.Fatalf("expected first-start generation, got %+v", mat)
	}

	// All four files exist; keys are 0600.
	for name, mode := range map[string]os.FileMode{
		"ca.pem": 0o644, "ca-key.pem": 0o600, "cert.pem": 0o644, "key.pem": 0o600,
	} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
		if info.Mode().Perm() != mode {
			t.Errorf("%s mode %o, want %o", name, info.Mode().Perm(), mode)
		}
	}

	// Leaf chains to the CA and covers the default + configured SANs.
	_, pool := loadCA(t, mat.CACertPath)
	leaf := leafOf(t, mat.Certificate)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("leaf does not verify against generated CA: %v", err)
	}
	if !containsDNS(leaf, "localhost") || !containsDNS(leaf, "box.example.com") {
		t.Errorf("DNS SANs wrong: %v", leaf.DNSNames)
	}
	if !containsIP(leaf, "127.0.0.1") || !containsIP(leaf, "::1") || !containsIP(leaf, "10.1.2.3") {
		t.Errorf("IP SANs wrong: %v", leaf.IPAddresses)
	}
}

// A second Load must reuse the persisted CA and leaf, not regenerate.
func TestAutoReloadReusesMaterial(t *testing.T) {
	dir := t.TempDir()
	first, err := Load(autoCfg(dir))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Load(autoCfg(dir))
	if err != nil {
		t.Fatal(err)
	}
	if second.GeneratedCA {
		t.Error("healthy persisted CA was regenerated")
	}
	if string(first.Certificate.Certificate[0]) != string(second.Certificate.Certificate[0]) {
		t.Error("persisted leaf was not reused")
	}
}

// An expired leaf renews against the persisted CA: new serial, same CA
// (imported trust survives), no CA regeneration.
func TestAutoRenewsExpiredLeaf(t *testing.T) {
	dir := t.TempDir()
	// Generate "far in the past": the 825d leaf is already expired
	// relative to real time, the 10y CA is not.
	reset := now
	now = func() time.Time { return time.Now().Add(-3 * 365 * 24 * time.Hour) }
	first, err := Load(autoCfg(dir))
	if err != nil {
		now = reset
		t.Fatal(err)
	}
	now = reset

	second, err := Load(autoCfg(dir))
	if err != nil {
		t.Fatal(err)
	}
	if second.GeneratedCA {
		t.Error("CA must not regenerate while valid")
	}
	if string(first.Certificate.Certificate[0]) == string(second.Certificate.Certificate[0]) {
		t.Error("expired leaf was not renewed")
	}
	_, pool := loadCA(t, second.CACertPath)
	if _, err := leafOf(t, second.Certificate).Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("renewed leaf does not verify: %v", err)
	}
}

// A leaf signed by a different CA does not verify against the persisted
// CA, so it is replaced: the served material must chain to dirA's CA.
func TestAutoReplacesForeignLeaf(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	if _, err := Load(autoCfg(dirA)); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(autoCfg(dirB)); err != nil {
		t.Fatal(err)
	}
	// Swap B's leaf into A's directory: CA mismatch.
	for _, name := range []string{"cert.pem", "key.pem"} {
		b, err := os.ReadFile(filepath.Join(dirB, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dirA, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mat, err := Load(autoCfg(dirA))
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Load(autoCfg(dirB)); err != nil {
		t.Fatal(err)
	} else if string(second.Certificate.Certificate[0]) == string(mat.Certificate.Certificate[0]) {
		t.Error("foreign leaf was served instead of being replaced")
	}
	_, pool := loadCA(t, mat.CACertPath)
	if _, err := leafOf(t, mat.Certificate).Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("post-load leaf does not verify: %v", err)
	}
}

// Bring-your-own files load verbatim with no generation side effects.
func TestUserProvidedFiles(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(autoCfg(dir)); err != nil {
		t.Fatal(err)
	}
	mat, err := Load(config.TLS{
		Enabled:  true,
		CertFile: filepath.Join(dir, "cert.pem"),
		KeyFile:  filepath.Join(dir, "key.pem"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if mat.GeneratedCA || mat.CACertPath != "" {
		t.Fatalf("files mode must not report generation: %+v", mat)
	}
	if len(mat.Certificate.Certificate) == 0 {
		t.Fatal("no certificate loaded")
	}
}

func TestCollectSANs(t *testing.T) {
	dnsNames, ips := collectSANs([]string{"extra.example.com", "192.0.2.10"})
	if dnsNames[0] != "localhost" {
		t.Errorf("localhost missing: %v", dnsNames)
	}
	if !containsStr(dnsNames, "extra.example.com") {
		t.Errorf("extra DNS missing: %v", dnsNames)
	}
	found := false
	for _, ip := range ips {
		if ip.String() == "192.0.2.10" {
			found = true
		}
	}
	if !found {
		t.Errorf("extra IP missing: %v", ips)
	}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

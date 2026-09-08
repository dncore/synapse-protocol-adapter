// Package tlscert provisions the listener's TLS material. Certificates
// configured via server.tls.cert_file/key_file are loaded as-is; without
// them, a mkcert-style self-signed CA plus server certificate is generated
// on first start and persisted, so the CA can be imported into clients'
// trust stores once and survives restarts (Docker users: mount auto_dir
// as a volume, or the CA is regenerated per container).
package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/dncore/synapse-protocol-adapter/internal/config"
)

// leafValidity is the server certificate lifetime. 825 days is the longest
// Safari accepts without warnings; renewal is automatic on startup once
// fewer than 30 days remain, so the exact figure only matters for clock
// skew tolerance.
const (
	caValidity   = 10 * 365 * 24 * time.Hour
	leafValidity = 825 * 24 * time.Hour
	renewWithin  = 30 * 24 * time.Hour
)

// now is swappable so tests can simulate certificate expiry.
var now = time.Now

// Material is the loaded listener certificate plus provenance for logging.
type Material struct {
	Certificate tls.Certificate
	// CACertPath is the generated CA certificate in auto mode ("" when
	// user-provided files are in use).
	CACertPath string
	// GeneratedCA is true when this run created a new CA (first start, or
	// the previous CA had expired) — clients must (re)import it.
	GeneratedCA bool
}

// Load returns the listener certificate: the configured files when set,
// otherwise the persisted auto-generated pair under AutoDir, generating or
// renewing as needed.
func Load(cfg config.TLS) (Material, error) {
	if cfg.CertFile != "" || cfg.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return Material{}, fmt.Errorf("load server.tls cert/key: %w", err)
		}
		return Material{Certificate: cert}, nil
	}
	dir := cfg.AutoDir
	if dir == "" {
		var err error
		dir, err = DefaultDir()
		if err != nil {
			return Material{}, err
		}
	}
	return ensureGenerated(dir, cfg.SANs)
}

// DefaultDir mirrors service.DetectPaths so the generated certificates
// live beside the config: XDG_CONFIG_HOME/synapse/tls on Linux,
// ~/.config/synapse/tls elsewhere.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	dir := filepath.Join(home, ".config", "synapse", "tls")
	if runtime.GOOS == "linux" {
		if d, err := os.UserConfigDir(); err == nil {
			dir = filepath.Join(d, "synapse", "tls")
		}
	}
	return dir, nil
}

// ensureGenerated loads the persisted CA + leaf, generating them on first
// use and renewing the leaf (with the existing CA) when it is near or past
// expiry. A full regeneration happens only when the CA itself is gone or
// expired — that invalidates imported trust, so it is never done silently
// for a healthy CA.
func ensureGenerated(dir string, sans []string) (Material, error) {
	caPath := filepath.Join(dir, "ca.pem")
	caKeyPath := filepath.Join(dir, "ca-key.pem")
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	var caCert *x509.Certificate
	var caKey *ecdsa.PrivateKey
	var generatedCA bool
	if caPEM, err := os.ReadFile(caPath); err == nil {
		caCert, err = parseCertificate(caPEM)
		if err != nil {
			return Material{}, fmt.Errorf("parse %s: %w", caPath, err)
		}
		if now().After(caCert.NotAfter) {
			// Expired CA: every trust anchor it signed is gone with it.
			caCert = nil
		}
	}
	if caCert != nil {
		keyPEM, err := os.ReadFile(caKeyPath)
		if err != nil {
			return Material{}, fmt.Errorf("read %s (delete the directory to regenerate): %w", caKeyPath, err)
		}
		caKey, err = parseECDSAKey(keyPEM)
		if err != nil {
			return Material{}, fmt.Errorf("parse %s: %w", caKeyPath, err)
		}
	}

	if caCert == nil {
		var err error
		caCert, caKey, err = generateCA()
		if err != nil {
			return Material{}, err
		}
		generatedCA = true
		if err := writePair(dir,
			"ca.pem", certPEM(caCert), 0o644,
			"ca-key.pem", keyPEM(caKey), 0o600,
		); err != nil {
			return Material{}, err
		}
	}

	// Existing leaf: reuse when verifiable against the CA and not close to
	// expiry; renew otherwise. SAN changes only apply at (re)generation —
	// documented behavior, not worth a restart loop.
	if certPEMBytes, keyPEMBytes, err := readPair(certPath, keyPath); err == nil {
		leaf, err := tls.X509KeyPair(certPEMBytes, keyPEMBytes)
		if err == nil && leafIsValid(leaf, caCert) {
			return Material{
				Certificate: leaf,
				CACertPath:  caPath,
				GeneratedCA: generatedCA,
			}, nil
		}
	}

	dnsNames, ips := collectSANs(sans)
	leaf, err := generateLeaf(caCert, caKey, dnsNames, ips)
	if err != nil {
		return Material{}, err
	}
	if err := writePair(dir,
		"cert.pem", certPEM(leaf.cert), 0o644,
		"key.pem", keyPEM(leaf.key), 0o600,
	); err != nil {
		return Material{}, err
	}
	pair, err := tls.X509KeyPair(certPEM(leaf.cert), keyPEM(leaf.key))
	if err != nil {
		return Material{}, err
	}
	return Material{Certificate: pair, CACertPath: caPath, GeneratedCA: generatedCA}, nil
}

// leafIsValid reports whether the leaf chains to ca and has renewal
// headroom left.
func leafIsValid(pair tls.Certificate, ca *x509.Certificate) bool {
	if len(pair.Certificate) == 0 {
		return false
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return false
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return false
	}
	return now().Add(renewWithin).Before(cert.NotAfter)
}

// --- generation ---

// generated holds a freshly created certificate with its private key.
type generated struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func generateCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate CA key: %w", err)
	}
	hostname, _ := os.Hostname()
	tmpl := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{Organization: []string{"synapse"}, CommonName: "synapse local CA (" + hostname + ")"},
		NotBefore:             now().Add(-time.Hour), // tolerate minor clock skew
		NotAfter:              now().Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("parse created CA certificate: %w", err)
	}
	return cert, key, nil
}

func generateLeaf(ca *x509.Certificate, caKey *ecdsa.PrivateKey, dnsNames []string, ips []net.IP) (generated, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return generated{}, fmt.Errorf("generate server key: %w", err)
	}
	commonName := "synapse"
	if len(dnsNames) > 0 {
		commonName = dnsNames[0]
	}
	tmpl := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{Organization: []string{"synapse"}, CommonName: commonName},
		NotBefore:             now().Add(-time.Hour),
		NotAfter:              now().Add(leafValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return generated{}, fmt.Errorf("create server certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return generated{}, fmt.Errorf("parse created server certificate: %w", err)
	}
	return generated{cert: cert, key: key}, nil
}

// randomSerial returns a positive 128-bit serial number (CA/B forum
// requires ≥64 bits of entropy; 128 keeps it simple).
func randomSerial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return n
}

// collectSANs builds the certificate's Subject Alternative Names: localhost
// in all its forms, the machine hostname, every routable interface IP
// (LAN clients connect by those), plus configured extras (DNS or IP).
func collectSANs(extra []string) (dnsNames []string, ips []net.IP) {
	dnsNames = []string{"localhost"}
	ips = []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	if hn, err := os.Hostname(); err == nil && hn != "" && hn != "localhost" {
		dnsNames = append(dnsNames, hn)
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.IsGlobalUnicast() {
				ips = append(ips, ipnet.IP)
			}
		}
	}
	for _, s := range extra {
		if ip := net.ParseIP(s); ip != nil {
			ips = append(ips, ip)
		} else {
			dnsNames = append(dnsNames, s)
		}
	}
	return dnsNames, ips
}

// --- PEM encode/decode and file persistence ---

func certPEM(cert *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}

func keyPEM(key *ecdsa.PrivateKey) []byte {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		panic(err) // P-256 keys always marshal
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

func parseCertificate(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("not a PEM CERTIFICATE")
	}
	return x509.ParseCertificate(block.Bytes)
}

func parseECDSAKey(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("not a PEM key")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

// writePair persists two PEM files atomically-enough for first-start
// generation (plain writes; generation happens once, before serving).
func writePair(dir, nameA string, bytesA []byte, modeA os.FileMode, nameB string, bytesB []byte, modeB os.FileMode) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	for name, b := range map[string]struct {
		data []byte
		mode os.FileMode
	}{nameA: {bytesA, modeA}, nameB: {bytesB, modeB}} {
		if err := os.WriteFile(filepath.Join(dir, name), b.data, b.mode); err != nil {
			return fmt.Errorf("write %s: %w", filepath.Join(dir, name), err)
		}
	}
	return nil
}

func readPair(certPath, keyPath string) ([]byte, []byte, error) {
	certBytes, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, err
	}
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	return certBytes, keyBytes, nil
}

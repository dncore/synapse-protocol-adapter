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
	"slices"
	"strings"
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

// sansFileName is the dynamic SAN registry beside the certificates: one
// DNS name or IP per line ('#' starts a comment). It is the mutable
// counterpart to the declarative server.tls.sans in config.yaml; the
// effective SAN set is the union of both, so neither channel can lose
// entries at renewal time.
const sansFileName = "sans"

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

// resolveDir is the single AutoDir→default resolution shared by every
// entry point.
func resolveDir(cfg config.TLS) (string, error) {
	if cfg.AutoDir != "" {
		return cfg.AutoDir, nil
	}
	return DefaultDir()
}

// Load returns the listener certificate: the configured files when set,
// otherwise the persisted auto-generated pair under AutoDir, generating or
// renewing as needed. The effective SAN set (config ∪ registry) is the
// coverage requirement: an existing leaf missing any required SAN is
// re-signed under the same CA, so adding names needs no manual deletion.
func Load(cfg config.TLS) (Material, error) {
	if cfg.CertFile != "" || cfg.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return Material{}, fmt.Errorf("load server.tls cert/key: %w", err)
		}
		return Material{Certificate: cert}, nil
	}
	dir, err := resolveDir(cfg)
	if err != nil {
		return Material{}, err
	}
	sans, err := EffectiveSANs(cfg)
	if err != nil {
		return Material{}, err
	}
	return ensureGenerated(dir, sans)
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

	// Existing leaf: reuse when it chains to the CA, has renewal headroom,
	// and covers every required SAN. Anything else (expiry, hostname/IP
	// drift, a registry addition) re-signs the leaf — the CA and the trust
	// clients imported stay untouched.
	dnsNames, ips := collectSANs(sans)
	if certPEMBytes, keyPEMBytes, err := readPair(certPath, keyPath); err == nil {
		leaf, err := tls.X509KeyPair(certPEMBytes, keyPEMBytes)
		if err == nil && leafSufficient(leaf, caCert, dnsNames, ips) {
			return Material{
				Certificate: leaf,
				CACertPath:  caPath,
				GeneratedCA: generatedCA,
			}, nil
		}
	}

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

// leafSufficient is the reuse gate: valid chain + headroom + every
// required DNS name and IP present in the SAN extension.
func leafSufficient(pair tls.Certificate, ca *x509.Certificate, dnsNames []string, ips []net.IP) bool {
	if !leafIsValid(pair, ca) {
		return false
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return false
	}
	for _, d := range dnsNames {
		if !slices.Contains(cert.DNSNames, d) {
			return false
		}
	}
	for _, ip := range ips {
		found := false
		for _, c := range cert.IPAddresses {
			if c.Equal(ip) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
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

// --- dynamic SAN registry ---

// readSANRegistry loads auto_dir/sans. A missing file is an empty
// registry, not an error.
func readSANRegistry(dir string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(dir, sansFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var sans []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0]); line != "" {
			sans = append(sans, line)
		}
	}
	return sans, nil
}

// writeSANRegistry persists the registry atomically (write temp + rename)
// so a concurrent daemon reload never observes a partial file.
func writeSANRegistry(dir string, sans []string) error {
	var sb strings.Builder
	for _, s := range sans {
		sb.WriteString(s)
		sb.WriteByte('\n')
	}
	return atomicWrite(filepath.Join(dir, sansFileName), []byte(sb.String()), 0o644)
}

func atomicWrite(path string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// EffectiveSANs merges the declarative config sans with the dynamic
// registry (config first), deduplicated, order-preserving.
func EffectiveSANs(cfg config.TLS) ([]string, error) {
	dir, err := resolveDir(cfg)
	if err != nil {
		return nil, err
	}
	reg, err := readSANRegistry(dir)
	if err != nil {
		return nil, fmt.Errorf("read sans registry: %w", err)
	}
	seen := make(map[string]bool, len(cfg.SANs)+len(reg))
	var out []string
	for _, s := range append(slices.Clone(cfg.SANs), reg...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out, nil
}

// ValidateSANName reports whether s is usable as a SAN entry: an IP
// address or an RFC 1123 DNS name, optionally with a "*." wildcard label.
func ValidateSANName(s string) bool {
	if net.ParseIP(s) != nil {
		return true
	}
	if strings.HasPrefix(s, "*.") {
		s = s[2:]
	}
	if s == "" {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			case c == '-':
				if i == 0 || i == len(label)-1 {
					return false
				}
			default:
				return false
			}
		}
	}
	return true
}

// AddSANs validates names, appends the new ones to the dynamic registry,
// and re-signs the server certificate under the existing CA so a daemon
// reload (SIGHUP) or restart serves them immediately. The CA is never
// touched: client-side imported trust survives every change. Returns the
// names actually added (already-known names are skipped).
func AddSANs(cfg config.TLS, names []string) ([]string, error) {
	if cfg.CertFile != "" || cfg.KeyFile != "" {
		return nil, errors.New("server.tls.cert_file mode: SANs live in the provided certificate, not the registry")
	}
	for _, n := range names {
		if !ValidateSANName(n) {
			return nil, fmt.Errorf("%q is not a valid DNS name or IP address", n)
		}
	}
	dir, err := resolveDir(cfg)
	if err != nil {
		return nil, err
	}
	effective, err := EffectiveSANs(cfg)
	if err != nil {
		return nil, err
	}
	registry, err := readSANRegistry(dir)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool, len(effective))
	for _, s := range effective {
		seen[s] = true
	}
	var added []string
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		registry = append(registry, n)
		added = append(added, n)
	}
	if len(added) == 0 {
		return nil, nil
	}
	if err := writeSANRegistry(dir, registry); err != nil {
		return nil, fmt.Errorf("write sans registry: %w", err)
	}

	// Re-sign immediately when a CA exists; otherwise the registry is
	// saved and the next start generates everything with the new SANs.
	caPEM, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if errors.Is(err, os.ErrNotExist) {
		return added, nil
	}
	if err != nil {
		return nil, err
	}
	ca, err := parseCertificate(caPEM)
	if err != nil {
		return nil, err
	}
	caKeyPEM, err := os.ReadFile(filepath.Join(dir, "ca-key.pem"))
	if err != nil {
		return nil, fmt.Errorf("read ca-key.pem (delete the directory to regenerate): %w", err)
	}
	caKey, err := parseECDSAKey(caKeyPEM)
	if err != nil {
		return nil, err
	}
	dnsNames, ips := collectSANs(append(effective, added...))
	leaf, err := generateLeaf(ca, caKey, dnsNames, ips)
	if err != nil {
		return nil, err
	}
	if err := writePair(dir,
		"cert.pem", certPEM(leaf.cert), 0o644,
		"key.pem", keyPEM(leaf.key), 0o600,
	); err != nil {
		return nil, err
	}
	return added, nil
}

// Info is a read-only snapshot for `synapse tls list`.
type Info struct {
	Mode         string // "disabled" | "files" | "auto"
	Dir          string
	CACertPath   string
	RegistryPath string
	Registry     []string
	LeafNotAfter time.Time // zero when no leaf exists
	DNSNames     []string
	IPs          []string
}

// Describe inspects the material on disk without generating or re-signing
// anything; missing pieces stay zero-valued.
func Describe(cfg config.TLS) Info {
	if !cfg.Enabled {
		return Info{Mode: "disabled"}
	}
	if cfg.CertFile != "" || cfg.KeyFile != "" {
		info := Info{Mode: "files", CACertPath: cfg.CertFile}
		if pair, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile); err == nil && len(pair.Certificate) > 0 {
			fillLeaf(&info, pair)
		}
		return info
	}
	dir, err := resolveDir(cfg)
	if err != nil {
		return Info{Mode: "auto"}
	}
	info := Info{
		Mode:         "auto",
		Dir:          dir,
		CACertPath:   filepath.Join(dir, "ca.pem"),
		RegistryPath: filepath.Join(dir, sansFileName),
	}
	info.Registry, _ = readSANRegistry(dir)
	if pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")); err == nil && len(pair.Certificate) > 0 {
		fillLeaf(&info, pair)
	}
	return info
}

func fillLeaf(info *Info, pair tls.Certificate) {
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return
	}
	info.LeafNotAfter = cert.NotAfter
	info.DNSNames = cert.DNSNames
	for _, ip := range cert.IPAddresses {
		info.IPs = append(info.IPs, ip.String())
	}
}

// CACertPath returns the CA certificate location in auto mode ("" in
// files mode — there is no generated CA to download).
func CACertPath(cfg config.TLS) string {
	if cfg.CertFile != "" || cfg.KeyFile != "" {
		return ""
	}
	dir, err := resolveDir(cfg)
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "ca.pem")
}

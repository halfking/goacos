package gateway

// Self-signed certificate fallback.
//
// # What this is for
//
// A domain with no certificate still has to answer on 443, or the failure
// looks like "the site is down" rather than "the certificate is not configured".
// Serving a locally generated certificate keeps the site reachable and, more
// importantly, keeps the failure visible: the response carries
// X-Goacos-Cert-Source: self_signed and the console lists the domain as
// running on a self-signed certificate.
//
// # Why it is never promoted to a real one
//
// A self-signed certificate cannot be trusted by a browser, so it is a
// diagnostic, not a solution. Everything it produces is marked as such in the
// database, and the renewal path for a real ACME certificate always replaces
// it rather than extending it. If a domain ever needs a production certificate
// and ACME cannot issue one, the answer is an internal CA — not a longer-lived
// self-signed certificate.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/halfking/goacos/internal/storage"
)

// selfSignedLifetime is long enough that a fallback certificate does not churn
// every week, short enough that regenerating it stays a routine operation.
const selfSignedLifetime = 90 * 24 * time.Hour

// selfSignedRenewBefore is when a cached fallback is regenerated.
const selfSignedRenewBefore = 30 * 24 * time.Hour

// generateSelfSigned issues a certificate for one domain, cached under dir.
//
// The cache exists so that restarting the gateway does not produce a new key
// and force every client to re-trust the site again. The directory is
// deliberately separate from the ACME account key directory: neither is
// important state, and mixing them makes it far too easy to point
// GOACOS_SELFSIGNED_DIR at a path that gets wiped.
func generateSelfSigned(domain, dir string) (certPEM, keyPEM string, notAfter time.Time, err error) {
	if dir != "" {
		certPath := filepath.Join(dir, sanitizeFilename(domain)+".crt")
		keyPath := filepath.Join(dir, sanitizeFilename(domain)+".key")
		if c, k, na, err := loadSelfSigned(certPath, keyPath, domain); err == nil {
			return c, k, na, nil
		}
	}

	certPEM, keyPEM, notAfter, err = newSelfSigned(domain)
	if err != nil {
		return "", "", time.Time{}, err
	}
	if dir == "" {
		return certPEM, keyPEM, notAfter, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		// A cache that cannot be written is a performance problem, not a
		// correctness one: the certificate is still valid for this process.
		return certPEM, keyPEM, notAfter, nil
	}
	_ = os.WriteFile(filepath.Join(dir, sanitizeFilename(domain)+".crt"), []byte(certPEM), 0o600)
	_ = os.WriteFile(filepath.Join(dir, sanitizeFilename(domain)+".key"), []byte(keyPEM), 0o600)
	return certPEM, keyPEM, notAfter, nil
}

// loadSelfSigned returns a cached certificate when it is still inside its
// renewal window and still actually covers the domain it is stored under.
func loadSelfSigned(certPath, keyPath, domain string) (string, string, time.Time, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return "", "", time.Time{}, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return "", "", time.Time{}, err
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", "", time.Time{}, fmt.Errorf("cached certificate for %s is not PEM", domain)
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", "", time.Time{}, err
	}
	if time.Now().After(leaf.NotAfter.Add(-selfSignedRenewBefore)) {
		return "", "", time.Time{}, fmt.Errorf("cached certificate for %s is expiring", domain)
	}
	if err := leaf.VerifyHostname(domain); err != nil {
		// A cached certificate under the wrong name is worse than none: it
		// would be served to a domain it does not cover.
		return "", "", time.Time{}, fmt.Errorf("cached certificate does not cover %s: %w", domain, err)
	}
	return string(certPEM), string(keyPEM), leaf.NotAfter, nil
}

// newSelfSigned mints a fresh P-256 certificate for one domain.
//
// P-256 rather than RSA: the handshake cost is lower and every client that
// accepts RSA accepts ECDSA. The serial number is random rather than derived
// from a counter because two gateway instances starting at the same second
// would otherwise produce identical serials.
func newSelfSigned(domain string) (certPEM, keyPEM string, notAfter time.Time, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("generate key: %w", err)
	}
	serialMax := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialMax)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("generate serial: %w", err)
	}
	notBefore := time.Now().Add(-time.Hour) // tolerate small clock skew on clients
	notAfter = time.Now().Add(selfSignedLifetime)

	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: domain, Organization: []string{"goacos gateway (self-signed)"}},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		// The certificate signs only itself. Marking it a CA would let one
		// self-signed fallback vouch for others, which is not a property a
		// gateway certificate should ever have.
		IsCA: false,
	}
	if ip := net.ParseIP(domain); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{domain}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("create certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("marshal key: %w", err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPEM, keyPEM, notAfter, nil
}

// sanitizeFilename makes a domain safe to use as a file name without
// introducing a path escape: every character outside [A-Za-z0-9._-] becomes an
// underscore, so a hostile domain string cannot write outside the cache
// directory.
func sanitizeFilename(domain string) string {
	d := storage.NormalizeDomain(domain)
	var b strings.Builder
	for _, r := range d {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := b.String()
	// A name made only of dots and underscores is still a valid filename but
	// a hostile one: it collapses to "." or ".." for any tool that resolves
	// the cache path. Refuse it outright rather than trusting every caller to
	// check afterwards.
	if strings.Trim(name, "._") == "" {
		return "unnamed"
	}
	return name
}

package gateway

// Tests for loading certificates from a shared directory.
//
// This path is a trust boundary: a file dropped into a mounted volume starts
// being served as a certificate. These tests cover the parsing of every layout
// in the wild, the precedence rule between directory and database, and the
// atomicity of the write-back that keeps the two in sync.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/halfking/goacos/internal/config"
	"github.com/halfking/goacos/internal/storage"
)

// writePair generates a real certificate/key pair for a domain and returns
// both PEMs, so directory tests exercise genuine TLS material rather than
// fixtures that happen to parse.
func writePair(t *testing.T, domain string) (certPEM, keyPEM string) {
	t.Helper()
	c, k, _, err := newSelfSigned(domain)
	if err != nil {
		t.Fatalf("generate for %s: %v", domain, err)
	}
	return c, k
}

// writeExternalPair mints a certificate that looks like it came from a real CA.
//
// The scenario under test is "an operator mounted a directory of certificates
// their CA already issued", so using a goacos self-signed pair would test the
// wrong thing: classification would correctly call it a placeholder, and the
// test would be asserting that a placeholder beats a real certificate.
func writeExternalPair(t *testing.T, domain string) (certPEM, keyPEM string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: domain},
		// An issuer that is unmistakably not this gateway.
		Issuer:                pkix.Name{CommonName: "Test CA R3", Organization: []string{"Test Authority"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(60 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{domain},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}

func put(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The certbot "live" layout is the single most common way certificates exist
// on a host, so it must be picked up without any conversion step.
func TestScanCertDirFindsCertbotLayout(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM := writePair(t, "certbot.example.com")
	put(t, filepath.Join(dir, "certbot.example.com", "fullchain.pem"), certPEM)
	put(t, filepath.Join(dir, "certbot.example.com", "privkey.pem"), keyPEM)

	found := scanCertDir(dir)
	fc, ok := found["certbot.example.com"]
	if !ok {
		t.Fatalf("certbot layout not found, got %v", keysOf(found))
	}
	if err := fc.leaf.VerifyHostname("certbot.example.com"); err != nil {
		t.Errorf("loaded certificate does not cover its domain: %v", err)
	}
}

func TestScanCertDirFindsFlatPair(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM := writePair(t, "flat.example.com")
	put(t, filepath.Join(dir, "flat.example.com.crt"), certPEM)
	put(t, filepath.Join(dir, "flat.example.com.key"), keyPEM)

	if _, ok := scanCertDir(dir)["flat.example.com"]; !ok {
		t.Fatalf("flat .crt/.key pair not found, got %v", keysOf(scanCertDir(dir)))
	}
}

func TestScanCertDirFindsCombinedPEM(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM := writePair(t, "combined.example.com")
	// Chain first, then key: the OpenSSL convention.
	put(t, filepath.Join(dir, "combined.example.com.pem"), certPEM+keyPEM)

	found := scanCertDir(dir)
	fc, ok := found["combined.example.com"]
	if !ok {
		t.Fatalf("combined pem not found, got %v", keysOf(found))
	}
	if err := fc.leaf.VerifyHostname("combined.example.com"); err != nil {
		t.Errorf("combined pem produced an unusable certificate: %v", err)
	}
}

// Some tools write the key first. Refusing that layout would mean a working
// deployment silently serves nothing.
func TestSplitCombinedPEMAcceptsKeyFirst(t *testing.T) {
	certPEM, keyPEM := writePair(t, "keyfirst.example.com")
	c, k, ok := splitCombinedPEM(keyPEM + certPEM)
	if !ok {
		t.Fatal("key-first layout must be accepted")
	}
	if _, _, err := parseCertificate(c, k); err != nil {
		t.Errorf("key-first split produced an unusable pair: %v", err)
	}
}

func TestSplitCombinedPEMRejectsIncomplete(t *testing.T) {
	certPEM, _ := writePair(t, "only-cert.example.com")
	if _, _, ok := splitCombinedPEM(certPEM); ok {
		t.Error("a file with no key must not be treated as a certificate pair")
	}
}

// A half-written or temporary file in the mount must never become a served
// certificate: a truncated PEM fails the handshake and takes the domain down.
func TestScanCertDirIgnoresTempFiles(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM := writePair(t, "real.example.com")
	put(t, filepath.Join(dir, "real.example.com.crt"), certPEM)
	put(t, filepath.Join(dir, "real.example.com.key"), keyPEM)
	// Exactly the name atomicWrite uses.
	put(t, filepath.Join(dir, ".goacos-tmp-12345"), "-----BEGIN CERTIFICATE-----\ntruncated")

	found := scanCertDir(dir)
	if len(found) != 1 {
		t.Fatalf("only the complete pair should load, got %v", keysOf(found))
	}
	if _, ok := found["real.example.com"]; !ok {
		t.Error("the valid pair must still load")
	}
}

// A stray README or checksum file in the same mount must not become a
// "certificate for a domain called readme".
func TestScanCertDirIgnoresNonCertificateNames(t *testing.T) {
	dir := t.TempDir()
	put(t, filepath.Join(dir, "README"), "hello")
	put(t, filepath.Join(dir, "notes.txt"), "hello")
	put(t, filepath.Join(dir, "backup-old"), "hello")
	if found := scanCertDir(dir); len(found) != 0 {
		t.Fatalf("expected nothing to load, got %v", keysOf(found))
	}
}

// A directory that does not exist is the normal state for a gateway that does
// not use one. It must never be an error, and must never block startup.
func TestScanCertDirMissingDirectoryIsEmpty(t *testing.T) {
	if got := scanCertDir(filepath.Join(t.TempDir(), "does-not-exist")); len(got) != 0 {
		t.Errorf("missing directory should yield no certificates, got %v", keysOf(got))
	}
	if got := scanCertDir(""); len(got) != 0 {
		t.Errorf("unset directory should yield no certificates, got %v", keysOf(got))
	}
}

// The rule that matters most: a real certificate on disk must beat the
// self-signed placeholder the database holds.
//
// This is the ordinary sequence — declare a domain, goacos provisions a
// fallback and tries ACME, then an operator mounts the certificates certbot
// already obtained. Without this rule the domain would serve a self-signed
// certificate forever while a valid one sat unread on disk.
func TestDirectoryRealCertificateBeatsSelfSignedFallback(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM := writeExternalPair(t, "late.example.com")
	put(t, filepath.Join(dir, "late.example.com.crt"), certPEM)
	put(t, filepath.Join(dir, "late.example.com.key"), keyPEM)

	dbCert := mustPair(t, "late.example.com")
	info, certs := mergeCerts(
		map[string]CertInfo{"late.example.com": {
			Domain: "late.example.com", Source: storage.CertSourceSelfSigned,
			NotAfter: time.Now().Add(90 * 24 * time.Hour),
		}},
		map[string]*tls.Certificate{"late.example.com": dbCert},
		scanCertDir(dir),
	)

	got := info["late.example.com"]
	if got.Source != storage.CertSourceFile {
		t.Fatalf("source=%q, want file — the real certificate must win", got.Source)
	}
	if got.FilePath == "" {
		t.Error("a file-sourced certificate must record where it came from")
	}
	if certs["late.example.com"] == dbCert {
		t.Error("the placeholder must no longer be the served certificate")
	}
}

// A directory is not automatically a source of real certificates: a lab
// self-signed pair dropped into the mount must not outrank a genuine
// certificate the database already holds.
func TestSelfSignedFileNeverBeatsDatabaseCertificate(t *testing.T) {
	dir := t.TempDir()
	// A self-signed pair in the mount — ordinary in a lab or internal setup.
	certPEM, keyPEM := writePair(t, "good.example.com")
	put(t, filepath.Join(dir, "good.example.com.crt"), certPEM)
	put(t, filepath.Join(dir, "good.example.com.key"), keyPEM)

	dbCert := mustPair(t, "good.example.com")
	info, certs := mergeCerts(
		map[string]CertInfo{"good.example.com": {
			Domain: "good.example.com", Source: storage.CertSourceACME,
			NotAfter: time.Now().Add(60 * 24 * time.Hour),
		}},
		map[string]*tls.Certificate{"good.example.com": dbCert},
		scanCertDir(dir),
	)

	// The file really is self-signed, so it must be recognised as such and
	// ranked below the database certificate — and the database entry is given
	// a LATER expiry here, so only the classification rule can explain the
	// outcome. Comparing expiry alone would get this backwards.
	if info["good.example.com"].Source != storage.CertSourceACME {
		t.Errorf("source=%q, want the database certificate to be kept", info["good.example.com"].Source)
	}
	if certs["good.example.com"] != dbCert {
		t.Error("the database certificate must remain the one served")
	}
}

func TestClassifyFileCert(t *testing.T) {
	certPEM, keyPEM := writePair(t, "classify.example.com")
	_, leaf, err := parseCertificate(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if got := classifyFileCert(leaf); got != storage.CertSourceSelfSigned {
		t.Errorf("a goacos-minted certificate on disk classifies as %q, want self_signed", got)
	}
	if got := classifyFileCert(nil); got != storageCertSourceFile {
		t.Errorf("an unparseable certificate should not be trusted as real, got %q", got)
	}
}

// An operator who uploads a certificate has stated an intent that no
// automated source may quietly override.
func TestManualCertificateOutranksDirectory(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM := writeExternalPair(t, "pinned.example.com")
	put(t, filepath.Join(dir, "pinned.example.com.crt"), certPEM)
	put(t, filepath.Join(dir, "pinned.example.com.key"), keyPEM)

	dbCert := mustPair(t, "pinned.example.com")
	info, _ := mergeCerts(
		map[string]CertInfo{"pinned.example.com": {
			Domain: "pinned.example.com", Source: storage.CertSourceManual,
			NotAfter: time.Now().Add(10 * 24 * time.Hour),
		}},
		map[string]*tls.Certificate{"pinned.example.com": dbCert},
		scanCertDir(dir),
	)
	if info["pinned.example.com"].Source != storage.CertSourceManual {
		t.Errorf("source=%q — a shorter-lived manual certificate still outranks the directory", info["pinned.example.com"].Source)
	}
}

func TestCertRankOrdering(t *testing.T) {
	if certRank(storage.CertSourceManual) <= certRank(storage.CertSourceACME) {
		t.Error("manual must outrank acme")
	}
	if certRank(storage.CertSourceACME) <= certRank(storage.CertSourceSelfSigned) {
		t.Error("acme must outrank self_signed")
	}
	if certRank(storage.CertSourceFile) <= certRank(storage.CertSourceSelfSigned) {
		t.Error("a file-sourced certificate must outrank self_signed")
	}
	// An unrecognised label must never outrank a known-good one.
	if certRank("something-new") > certRank(storage.CertSourceSelfSigned) {
		t.Error("an unknown source must rank lowest, not above self_signed")
	}
}

// A wildcard certificate is stored under its pattern and serves any
// single-label name beneath it — which is what certbot's layout produces.
func TestWildcardCertificateServesSubdomain(t *testing.T) {
	m := &CertManager{certs: map[string]*tls.Certificate{}, info: map[string]CertInfo{}}
	m.install("*.example.com", mustPair(t, "wildcard.example.com"), CertInfo{
		Domain: "*.example.com", Source: storageCertSourceFile,
	})

	if got := m.lookupWildcard("api.example.com"); got == nil {
		t.Error("a single-label subdomain must be served by the wildcard")
	}
	if got := m.lookupWildcard("a.b.example.com"); got != nil {
		t.Error("*.example.com does not cover a two-label name; serving it would be wrong")
	}
	if got := m.lookupWildcard("example.com"); got != nil {
		t.Error("a wildcard does not cover the bare domain")
	}
	if got := m.lookupWildcard("api.other.com"); got != nil {
		t.Error("a wildcard must not leak across registrable domains")
	}
}

func TestCoversWildcard(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"*.example.com", "api.example.com", true},
		{"*.example.com", "a.b.example.com", false},
		{"*.example.com", "example.com", false},
		{"*.example.com", "api.example.org", false},
		{"example.com", "api.example.com", false},
	}
	for _, c := range cases {
		if got := coversWildcard(c.pattern, c.name); got != c.want {
			t.Errorf("coversWildcard(%q,%q)=%v want %v", c.pattern, c.name, got, c.want)
		}
	}
}

// Write-back is what keeps the directory useful to other consumers, and it
// must be atomic: a reader scanning mid-write must never see a truncated file.
func TestWriteCertPairIsAtomicAndReadable(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM := writeExternalPair(t, "mirror.example.com")
	if err := writeCertPair(dir, "mirror.example.com", certPEM, keyPEM); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The written files must load straight back through the scanner.
	found := scanCertDir(dir)
	if _, ok := found["mirror.example.com"]; !ok {
		t.Fatalf("written pair is not readable back, got %v", keysOf(found))
	}
	if err := found["mirror.example.com"].leaf.VerifyHostname("mirror.example.com"); err != nil {
		t.Errorf("written certificate does not cover its domain: %v", err)
	}

	// No temporary file may survive a successful write.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".goacos-tmp-") {
			t.Errorf("temporary file %s was left behind", e.Name())
		}
	}
	if len(entries) != 2 {
		t.Errorf("expected exactly the .crt and .key, got %d files", len(entries))
	}
}

// The private key must not be world readable, even inside a shared directory.
func TestWriteCertPairProtectsKeyPermissions(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM := writePair(t, "perm.example.com")
	if err := writeCertPair(dir, "perm.example.com", certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "perm.example.com.key"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("key is readable beyond its owner (mode %o)", mode)
	}
}

// A domain the gateway issued must appear in the shared directory, which is
// the entire point of write-back: other components can reuse it.
func TestIssuedCertificateIsMirroredToDirectory(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{GatewayCertDir: dir, GatewayCertWriteBack: true}
	certs, keyPEM := writePair(t, "issued.example.com")
	m := newTestCertManager(cfg)

	m.mirrorToDir("issued.example.com", certs, keyPEM)

	found := scanCertDir(dir)
	if _, ok := found["issued.example.com"]; !ok {
		t.Errorf("issued certificate was not mirrored, dir has %v", keysOf(found))
	}
}

// With write-back off — a read-only mount — issuing must still succeed.
func TestWriteBackDisabledLeavesDirectoryUntouched(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{GatewayCertDir: dir, GatewayCertWriteBack: false}
	certs, keyPEM := writePair(t, "readonly.example.com")
	m := newTestCertManager(cfg)

	m.mirrorToDir("readonly.example.com", certs, keyPEM)

	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("write-back disabled but directory has %d files", len(entries))
	}
}

// An unwritable directory must not break issuance. The in-memory and database
// copies are already in place by then, so this costs redundancy, not service.
func TestUnwritableDirectoryDoesNotBreakIssuance(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skipf("cannot make directory read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	cfg := &config.Config{GatewayCertDir: filepath.Join(dir, "readonly-sub"), GatewayCertWriteBack: true}
	certs, keyPEM := writePair(t, "blocked.example.com")
	m := newTestCertManager(cfg)
	m.mirrorToDir("blocked.example.com", certs, keyPEM) // must not panic
}

func TestDomainFromName(t *testing.T) {
	cases := map[string]string{
		"example.com":      "example.com",
		"EXAMPLE.com":      "example.com",
		"*.example.com":    "*.example.com",
		"app-1.example.cn": "app-1.example.cn",
		"README":           "",
		"":                 "",
		".hidden.com":      "",
		"has space.com":    "",
	}
	for in, want := range cases {
		if got := domainFromName(in); got != want {
			t.Errorf("domainFromName(%q)=%q want %q", in, got, want)
		}
	}
}

func keysOf(m map[string]fileCert) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mustPair(t *testing.T, domain string) *tls.Certificate {
	t.Helper()
	certPEM, keyPEM := writePair(t, domain)
	cert, _, err := parseCertificate(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return cert
}

func newTestCertManager(cfg *config.Config) *CertManager {
	return &CertManager{
		cfg:        cfg,
		certs:      map[string]*tls.Certificate{},
		info:       map[string]CertInfo{},
		onDemand:   map[string]*tls.Certificate{},
		failures:   map[string]*failureRecord{},
		challenges: map[string]string{},
	}
}

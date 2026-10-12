package gateway

// Loading certificates from a shared directory.
//
// # Why a directory at all
//
// The database is the authoritative record of what goacos itself issued or
// fell back to, and it is what makes a certificate auditable and revocable. It
// is a poor fit for one thing though: certificates that some *other* tool
// already manages. A host running certbot, cert-manager or an internal PKI
// agent has those files on disk, and copying them into a database by hand is
// exactly the kind of duplicate state this project exists to remove.
//
// So the directory is an additional source, not a replacement. Both feed the
// same in-memory cache, and one precedence rule decides between them.
//
// # Never read on the handshake path
//
// Nothing here is consulted while serving traffic. The directory is scanned on
// a timer, parsed once, and handed to the same in-memory cache the database
// feeds. A TLS handshake is a read lock and a map lookup; if it hit the disk,
// a busy host or a slow volume would surface as a handshake failure storm
// rather than as what it actually is — a slow disk.
//
// # Precedence: a real certificate beats a self-signed one
//
// This is the rule that matters in practice. Consider the ordinary sequence:
// a domain is declared, goacos provisions a self-signed fallback and tries
// ACME, and *then* an operator points a mounted directory at certificates
// certbot already obtained. If the database always won, that domain would
// serve a self-signed certificate forever while a valid one sat on disk
// unread. A gateway that ignores a real certificate in favour of its own
// placeholder is worse than one that has no certificate at all.
//
// Where both sides hold a real certificate, the later expiry wins, and a tie
// goes to the database — the record an operator can see and revoke.

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// fileCert is one certificate pair discovered on disk.
type fileCert struct {
	domain  string
	path    string
	cert    *tls.Certificate
	leaf    *x509.Certificate
	certPEM string
	keyPEM  string
	modTime time.Time
}

// certRank orders sources by how much an operator should trust them to keep
// serving. It exists to answer one question: given two certificates for the
// same domain, which one wins?
//
//	file    = 2  a real certificate managed by an external tool
//	acme    = 2  a real certificate this gateway obtained
//	manual  = 3  explicitly uploaded by a human: never silently overridden
//	self_.. = 1  a placeholder that exists only so the handshake completes
func certRank(source string) int {
	switch source {
	case storageCertSourceManual:
		return 3
	case storageCertSourceACME, storageCertSourceFile:
		return 2
	default:
		// Everything unrecognised, including self_signed, ranks lowest. An
		// unrecognised label must never be able to outrank a known-good
		// certificate.
		return 1
	}
}

// betterCert reports whether candidate should replace incumbent.
//
// The comparison is tier first, then expiry. Ranking by expiry alone would let
// a long-lived self-signed placeholder outrank a freshly issued real
// certificate, which is the exact inversion described above.
func betterCert(incumbent, candidate CertInfo) bool {
	ri, rc := certRank(incumbent.Source), certRank(candidate.Source)
	if rc != ri {
		return rc > ri
	}
	if !candidate.NotAfter.Equal(incumbent.NotAfter) {
		return candidate.NotAfter.After(incumbent.NotAfter)
	}
	// Same rank, same expiry: the database wins, because it is the copy an
	// operator can inspect and revoke through the API.
	return candidate.Source != storageCertSourceFile
}

// classifyFileCert decides what a certificate found on disk actually is.
//
// A directory is not automatically a source of real certificates. Putting a
// self-signed pair in a mount is ordinary in a lab or an internal setup, and
// treating that as an externally-issued certificate would let it outrank a
// genuine certificate the database already holds — the exact inversion the
// precedence rule exists to prevent. The issuer is what tells them apart.
func classifyFileCert(leaf *x509.Certificate) string {
	if leaf == nil {
		return storageCertSourceFile
	}
	for _, org := range leaf.Issuer.Organization {
		if strings.Contains(org, "goacos gateway") {
			return storageCertSourceSelfSigned
		}
	}
	if strings.Contains(leaf.Issuer.CommonName, "goacos gateway") {
		return storageCertSourceSelfSigned
	}
	return storageCertSourceFile
}

// mergeCerts combines database and directory certificates per domain.
func mergeCerts(db map[string]CertInfo, dbCerts map[string]*tls.Certificate,
	files map[string]fileCert) (map[string]CertInfo, map[string]*tls.Certificate) {

	info := make(map[string]CertInfo, len(db)+len(files))
	certs := make(map[string]*tls.Certificate, len(db)+len(files))
	for domain, i := range db {
		info[domain] = i
		if c, ok := dbCerts[domain]; ok {
			certs[domain] = c
		}
	}
	for domain, f := range files {
		candidate := CertInfo{
			Domain:    domain,
			Source:    classifyFileCert(f.leaf),
			Issuer:    issuerOf(f.leaf, ""),
			NotBefore: f.leaf.NotBefore,
			NotAfter:  f.leaf.NotAfter,
			Status:    storageCertStatusActive,
			// Recording the path is what turns "some certificate" into an
			// answer an operator can act on when the console says source=file.
			FilePath: f.path,
		}
		if existing, ok := info[domain]; ok && !betterCert(existing, candidate) {
			continue
		}
		info[domain] = candidate
		certs[domain] = f.cert
	}
	return info, certs
}

// scanCertDir discovers certificate pairs under a directory.
//
// Three layouts are recognised because the tools that produce them all exist
// in the wild:
//
//	<dir>/example.com/fullchain.pem + privkey.pem   (certbot "live" layout)
//	<dir>/example.com.crt          + example.com.key (flat pair)
//	<dir>/example.com.pem                            (combined)
//
// Supporting all three is what lets an operator mount whatever their existing
// tooling already produces, instead of writing a conversion script.
//
// A missing directory is not an error: it is the normal state for a gateway
// that does not use one, and it must never stop the server from starting.
func scanCertDir(dir string) map[string]fileCert {
	out := map[string]fileCert{}
	if dir == "" {
		return out
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}

	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			// Skip dotfiles and, importantly, the temporary files used for
			// atomic writes: serving a half-written certificate would fail the
			// handshake for that domain.
			continue
		}
		if entry.IsDir() {
			collectFromSubdir(dir, name, out)
			continue
		}
		collectFlat(dir, name, out)
	}
	return out
}

// collectFromSubdir handles the certbot layout: one directory per domain.
func collectFromSubdir(root, name string, out map[string]fileCert) {
	domain := domainFromName(name)
	if domain == "" {
		return
	}
	dir := filepath.Join(root, name)

	certFile, keyFile := "", ""
	for _, candidate := range []string{"fullchain.pem", "cert.pem", domain + ".crt", domain + ".pem"} {
		p := filepath.Join(dir, candidate)
		if fileExists(p) {
			certFile = p
			break
		}
	}
	for _, candidate := range []string{"privkey.pem", domain + ".key", "key.pem"} {
		p := filepath.Join(dir, candidate)
		if fileExists(p) {
			keyFile = p
			break
		}
	}
	if certFile == "" || keyFile == "" {
		return
	}
	load(certFile, keyFile, domain, out)
}

// collectFlat handles <domain>.crt + <domain>.key and combined <domain>.pem.
func collectFlat(root, name string, out map[string]fileCert) {
	ext := strings.ToLower(filepath.Ext(name))
	stem := strings.TrimSuffix(name, filepath.Ext(name))

	// Combined file: one PEM holding the chain followed by the key.
	if ext == ".pem" {
		p := filepath.Join(root, name)
		raw, err := os.ReadFile(p)
		if err != nil {
			return
		}
		certPEM, keyPEM, ok := splitCombinedPEM(string(raw))
		if !ok {
			return
		}
		store(p, certPEM, keyPEM, domainFromName(stem), out)
		return
	}

	switch ext {
	case ".crt", ".cert", ".key":
		certFile := filepath.Join(root, stem+".crt")
		if ext != ".crt" {
			certFile = filepath.Join(root, stem+".cert")
			if !fileExists(certFile) {
				certFile = filepath.Join(root, stem+".crt")
			}
		}
		keyFile := filepath.Join(root, stem+".key")
		if fileExists(certFile) && fileExists(keyFile) {
			load(certFile, keyFile, domainFromName(stem), out)
		}
	}
}

// load reads a certificate/key pair and adds it to the result.
func load(certFile, keyFile, domain string, out map[string]fileCert) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return
	}
	store(certFile, string(certPEM), string(keyPEM), domain, out)
}

// store parses and indexes a pair, skipping anything that does not parse.
func store(path, certPEM, keyPEM, domain string, out map[string]fileCert) {
	if domain == "" {
		return
	}
	cert, leaf, err := parseCertificate(certPEM, keyPEM)
	if err != nil {
		return
	}
	info, err := os.Stat(path)
	var modTime time.Time
	if err == nil {
		modTime = info.ModTime()
	}
	out[domain] = fileCert{
		domain: domain, path: path, cert: cert, leaf: leaf,
		certPEM: certPEM, keyPEM: keyPEM, modTime: modTime,
	}
}

// splitCombinedPEM separates a chain from a key inside one file.
//
// Order matters and both orders are accepted: some tools write the chain first
// (OpenSSL convention), others write the key first. Everything before the key
// block is treated as chain material.
func splitCombinedPEM(raw string) (certPEM, keyPEM string, ok bool) {
	var certs, key strings.Builder
	rest := []byte(raw)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		encoded := pem.EncodeToMemory(block)
		switch block.Type {
		case "CERTIFICATE":
			certs.Write(encoded)
		case "PRIVATE KEY", "RSA PRIVATE KEY", "EC PRIVATE KEY":
			if key.Len() == 0 {
				key.Write(encoded)
			}
		}
	}
	if certs.Len() == 0 || key.Len() == 0 {
		return "", "", false
	}
	return certs.String(), key.String(), true
}

// domainFromName turns a file or directory name into a domain.
//
// Only names that plausibly are hostnames are accepted, so that a stray
// README or a checksum file in the same directory cannot become a "certificate
// for a domain called readme".
func domainFromName(name string) string {
	d := strings.TrimSpace(strings.ToLower(name))
	if d == "" || strings.HasPrefix(d, ".") {
		return ""
	}
	// Wildcards are legal in a certificate's SAN and common in certbot output.
	if strings.HasPrefix(d, "*.") {
		d = d[2:]
		if d == "" {
			return ""
		}
		return "*." + strings.TrimSuffix(d, "/")
	}
	if !strings.Contains(d, ".") {
		return ""
	}
	if strings.ContainsAny(d, "/\\ \t") {
		return ""
	}
	for _, c := range d {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
		default:
			return ""
		}
	}
	return d
}

// writeCertPair writes a certificate back into the shared directory.
//
// The write is atomic: a temporary file in the same directory is filled,
// flushed and renamed into place. A non-atomic write would leave a window in
// which the periodic scan reads a truncated PEM and the domain stops serving —
// a self-inflicted outage from what is supposed to be a redundancy feature.
//
// Only the chain and the key are written, in the flat <domain>.crt /
// <domain>.key layout, which is the one every other tool in this space also
// understands.
func writeCertPair(dir, domain, certPEM, keyPEM string) error {
	if dir == "" || domain == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(dir, domain+".crt"), []byte(certPEM), 0o644); err != nil {
		return fmt.Errorf("write certificate for %s: %w", domain, err)
	}
	if err := atomicWrite(filepath.Join(dir, domain+".key"), []byte(keyPEM), 0o600); err != nil {
		return fmt.Errorf("write key for %s: %w", domain, err)
	}
	return nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".goacos-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		// Best-effort cleanup: on the success path the rename already moved
		// the file, so this removal simply fails harmlessly.
		_ = os.Remove(tmpName)
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// coversWildcard reports whether a stored certificate covers a name.
//
// A wildcard certificate is stored under its wildcard name ("*.example.com")
// and served for any single-label subdomain beneath it. Matching is bounded to
// a few labels so a pathological name cannot drive an unbounded loop.
func coversWildcard(pattern, name string) bool {
	if !strings.HasPrefix(pattern, "*.") {
		return false
	}
	suffix := pattern[2:]
	if !strings.HasSuffix(name, "."+suffix) {
		return false
	}
	label := strings.TrimSuffix(name, "."+suffix)
	// Exactly one label: *.example.com covers a.example.com but not
	// a.b.example.com, which is what every CA actually issues.
	return label != "" && !strings.Contains(label, ".")
}

// leafNames is a small helper used by status output.
func leafNames(leaf *x509.Certificate) []string {
	if leaf == nil {
		return nil
	}
	return leaf.DNSNames
}

var errNoCertDir = errors.New("certificate directory is not configured")

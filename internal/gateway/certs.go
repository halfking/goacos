package gateway

// Certificate manager: the TLS side of the gateway.
//
// # The rule everything else follows
//
// A declared domain must always complete a TLS handshake. Not "should, once
// someone installs a certificate", but always — because a refused handshake
// presents to an operator as a dead site, and the actual fault (no certificate
// issued yet) is invisible at that layer. So the order is: serve something
// immediately, improve it when the CA answers.
//
//   declared, no certificate -> generate a self-signed fallback at once AND
//                               ask the CA in parallel
//   CA answers               -> replace the fallback with the real certificate
//   CA refuses or is unreachable -> the fallback keeps serving, loudly
//
// The fallback is recorded with source=self_signed, so "this domain is on a
// self-signed certificate" is answerable from the console at all times rather
// than being a thing someone discovers from a browser warning.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/halfking/goacos/internal/config"
	"github.com/halfking/goacos/internal/storage"
)

// acmeBackoffBase is the first retry delay after a failed issuance. It grows
// with consecutive failures up to maxIssuanceBackoff.
//
// The growth matters more than the base value: a domain that cannot be issued
// (wrong DNS, port 80 blocked, name that the CA refuses) would otherwise be
// retried every cycle forever, and every retry is a charge against a shared,
// finite rate limit.
const (
	acmeBackoffBase        = 5 * time.Minute
	maxIssuanceBackoff     = 6 * time.Hour
	maxOnDemandSelfSigned  = 64 // bound on certificates minted for unseen SNI names
	issuanceSerialInterval = 2 * time.Second
)

// CertInfo is what the console needs to know about one domain's certificate,
// without ever handing out the private key.
type CertInfo struct {
	Domain    string
	Source    string
	Issuer    string
	NotBefore time.Time
	NotAfter  time.Time
	Status    string
	LastError string
	// FilePath is set only for certificates discovered in the shared
	// directory. Recording it turns "served from a file" into an answer an
	// operator can act on when a domain stops working.
	FilePath string
}

// DaysLeft is the number of whole days until expiry, negative once expired.
// It is what turns an opaque timestamp into the number an operator reacts to.
func (c CertInfo) DaysLeft() int {
	return int(time.Until(c.NotAfter).Hours() / 24)
}

// Expired reports whether the certificate can no longer be served.
func (c CertInfo) Expired() bool { return time.Now().After(c.NotAfter) }

// CertManager owns the in-memory certificate cache, the ACME issuance loop and
// the challenge registry. All of it is safe for concurrent use: TLS handshakes
// read while the renewal loop writes.
type CertManager struct {
	cfg    *config.Config
	st     *storage.Store
	ns     string
	issuer *acmeIssuer

	mu          sync.RWMutex
	certs       map[string]*tls.Certificate
	info        map[string]CertInfo
	defaultCert *tls.Certificate

	// challenges holds live ACME HTTP-01 tokens. The CA fetches them over
	// plain HTTP on port 80, which is why this lives on the gateway's HTTP
	// listener rather than anywhere else.
	challengeMu sync.RWMutex
	challenges  map[string]string

	// issuing serialises issuance: one CA conversation at a time per
	// instance. Parallel orders are how a single mistake turns into a batch
	// of rate-limit failures.
	issueMu sync.Mutex

	failMu   sync.Mutex
	failures map[string]*failureRecord

	// accountOK records that the ACME account is established, so registration
	// is attempted once rather than on every reconcile cycle.
	accountMu sync.Mutex
	accountOK bool

	// onDemand bounds certificates minted for SNI names we do not have a
	// stored certificate for. Without a bound, an unauthenticated client
	// could mint unlimited certificates by sending arbitrary SNI values.
	onDemandMu sync.Mutex
	onDemand   map[string]*tls.Certificate
}

type failureRecord struct {
	count int
	next  time.Time
}

// NewCertManager builds the manager and loads any certificates already stored.
func NewCertManager(ctx context.Context, cfg *config.Config, st *storage.Store, ns string) (*CertManager, error) {
	issuer, err := newACMEIssuer(
		acmeDirectory(cfg),
		cfg.ACMEEmail,
		cfg.ACMEDataDir,
		time.Duration(cfg.ACMETimeout)*time.Second,
	)
	if err != nil {
		// A missing account key directory is not fatal to the gateway: it
		// serves what it has and reports why it cannot get more.
		log.Printf("[gateway] acme disabled: %v", err)
		issuer = &acmeIssuer{}
	}
	m := &CertManager{
		cfg:        cfg,
		st:         st,
		ns:         ns,
		issuer:     issuer,
		certs:      map[string]*tls.Certificate{},
		info:       map[string]CertInfo{},
		challenges: map[string]string{},
		failures:   map[string]*failureRecord{},
		onDemand:   map[string]*tls.Certificate{},
	}
	m.buildDefaultCert()
	if err := m.reload(ctx); err != nil {
		return nil, err
	}
	return m, nil
}

func acmeDirectory(cfg *config.Config) string {
	if !cfg.ACMEEnabled {
		return ""
	}
	return cfg.ACMEDirectoryURL
}

// buildDefaultCert mints the certificate served for an SNI name the gateway
// does not recognise. It is not a security control — it exists so that an
// unknown host produces a name mismatch the operator can read, instead of a
// handshake that simply fails.
func (m *CertManager) buildDefaultCert() {
	certPEM, keyPEM, _, err := newSelfSigned("goacos-gateway.invalid")
	if err != nil {
		log.Printf("[gateway] could not build default certificate: %v", err)
		return
	}
	cert, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		log.Printf("[gateway] default certificate is unusable: %v", err)
		return
	}
	m.defaultCert = &cert
}

// GetCertificate is the TLS callback. It is on the hot path of every new
// connection and must never block: it only takes a read lock.
func (m *CertManager) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	name := storage.NormalizeDomain(hello.ServerName)
	if name != "" {
		m.mu.RLock()
		cert, ok := m.certs[name]
		m.mu.RUnlock()
		if ok {
			return cert, nil
		}
		// A wildcard certificate is stored under its pattern ("*.example.com")
		// and serves any single-label name beneath it. Certbot's live layout
		// produces exactly this shape, so without this lookup a mounted
		// wildcard certificate would load successfully and then never be used.
		if cert := m.lookupWildcard(name); cert != nil {
			return cert, nil
		}
		// An SNI name we serve but have not issued for yet — issuance is in
		// flight, or the CA is unreachable. Minting a fallback for the exact
		// name the client asked for turns a bare handshake failure into the
		// accurate "self-signed" message an operator can act on.
		if c := m.onDemandCert(name); c != nil {
			return c, nil
		}
	}
	if m.defaultCert != nil {
		return m.defaultCert, nil
	}
	// Returning an error makes the handshake fail explicitly, which is the
	// correct outcome: serving nothing would look like a broken listener.
	return nil, fmt.Errorf("no certificate available for %q", hello.ServerName)
}

// lookupWildcard walks up the labels of a name looking for a stored wildcard
// certificate. The walk is bounded by the label count, so it cannot be driven
// into an unbounded loop by a crafted SNI value.
func (m *CertManager) lookupWildcard(name string) *tls.Certificate {
	if name == "" {
		return nil
	}
	labels := strings.Split(name, ".")
	// Leave at least two labels: a pattern needs a registrable suffix plus the
	// "*" label, so "*.com" would be meaningless and is never stored.
	for i := 1; i < len(labels)-1; i++ {
		pattern := "*." + strings.Join(labels[i:], ".")
		m.mu.RLock()
		cert, ok := m.certs[pattern]
		m.mu.RUnlock()
		if ok && coversWildcard(pattern, name) {
			return cert
		}
	}
	return nil
}

// onDemandCert returns a cached fallback for an unknown name, minting one at
// most once per name.
func (m *CertManager) onDemandCert(name string) *tls.Certificate {
	m.onDemandMu.Lock()
	defer m.onDemandMu.Unlock()
	if cert, ok := m.onDemand[name]; ok {
		return cert
	}
	if len(m.onDemand) >= maxOnDemandSelfSigned {
		// Cache full: fall back to the default rather than growing without
		// bound on behalf of whoever connects next.
		return m.defaultCert
	}
	certPEM, keyPEM, _, err := newSelfSigned(name)
	if err != nil {
		return m.defaultCert
	}
	cert, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return m.defaultCert
	}
	if m.onDemand == nil {
		m.onDemand = map[string]*tls.Certificate{}
	}
	m.onDemand[name] = &cert
	return &cert
}

// ServeChallenge answers an ACME HTTP-01 validation request.
//
// It runs before every redirect rule: a challenge that gets a 301 is a failed
// validation, and a failed validation is a spent rate-limit slot.
func (m *CertManager) ServeChallenge(w http.ResponseWriter, token string) bool {
	m.challengeMu.RLock()
	value, ok := m.challenges[token]
	m.challengeMu.RUnlock()
	if !ok {
		return false
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write([]byte(value))
	return true
}

func (m *CertManager) setChallenge(token, value string) {
	m.challengeMu.Lock()
	defer m.challengeMu.Unlock()
	if value == "" {
		delete(m.challenges, token)
		return
	}
	// Created on first use rather than in the constructor: a challenge
	// registered before NewCertManager finished would otherwise panic on a
	// nil map and take the whole issuance attempt with it.
	if m.challenges == nil {
		m.challenges = map[string]string{}
	}
	m.challenges[token] = value
}

// reload rebuilds the cache from the database.
//
// A read failure keeps the previous cache: certificates already in hand keep
// working, and a database blip must not become a TLS outage.
func (m *CertManager) reload(ctx context.Context) error {
	rows, err := m.st.ListCertificates(ctx, m.ns)
	if err != nil {
		return err
	}
	certs := make(map[string]*tls.Certificate, len(rows))
	info := make(map[string]CertInfo, len(rows))
	for i := range rows {
		row := rows[i]
		cert, leaf, err := parseCertificate(row.CertPEM, row.KeyPEM)
		if err != nil {
			log.Printf("[gateway] certificate for %s is unusable (%s): %v", row.Domain, row.Source, err)
			continue
		}
		domain := storage.NormalizeDomain(row.Domain)
		certs[domain] = cert
		info[domain] = CertInfo{
			Domain:    domain,
			Source:    row.Source,
			Issuer:    issuerOf(leaf, row.Issuer),
			NotBefore: leaf.NotBefore,
			NotAfter:  leaf.NotAfter,
			Status:    row.Status,
			LastError: row.LastError,
		}
	}
	// The directory is scanned on the same schedule as the database and is
	// merged in one direction only: it can win over the database when it holds
	// a real certificate, and can never demote a manually uploaded one.
	fileCerts := scanCertDir(m.cfg.GatewayCertDir)
	info, certs = mergeCerts(info, certs, fileCerts)

	m.mu.Lock()
	m.certs = certs
	m.info = info
	m.mu.Unlock()
	return nil
}

// parseCertificate loads a key pair and returns the leaf it covers.
func parseCertificate(certPEM, keyPEM string) (*tls.Certificate, *x509.Certificate, error) {
	cert, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return nil, nil, fmt.Errorf("key pair does not match: %w", err)
	}
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return nil, nil, fmt.Errorf("certificate is not PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse leaf: %w", err)
	}
	if len(cert.Certificate) > 0 {
		cert.Leaf = leaf
	}
	return &cert, leaf, nil
}

func issuerOf(leaf *x509.Certificate, fallback string) string {
	if leaf != nil && leaf.Issuer.CommonName != "" {
		return leaf.Issuer.CommonName
	}
	return fallback
}

// Info returns the console-facing view of one domain's certificate.
func (m *CertManager) Info(domain string) (CertInfo, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	info, ok := m.info[storage.NormalizeDomain(domain)]
	return info, ok
}

// ListInfo returns every certificate currently loaded, domain ascending.
func (m *CertManager) ListInfo() []CertInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]CertInfo, 0, len(m.info))
	for _, info := range m.info {
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out
}

// Run drives directory scanning, issuance and renewal until the context is
// cancelled.
//
// Scanning runs on its own, slower clock: it touches the disk, and a
// certificate does not become valid faster because the gateway polls harder.
// Issuance runs on the routing clock, because a domain waiting for a
// certificate is a domain users are hitting right now.
func (m *CertManager) Run(ctx context.Context, table *Table) {
	gwInterval := time.Duration(m.cfg.GatewayCertRefreshSeconds) * time.Second
	if gwInterval <= 0 {
		gwInterval = 10 * time.Second
	}
	scanInterval := time.Duration(m.cfg.GatewayCertScanSeconds) * time.Second
	if scanInterval <= 0 {
		scanInterval = 30 * time.Second
	}
	if m.cfg.GatewayCertDir == "" {
		scanInterval = 0
	}

	m.reconcile(ctx, table)

	gwTicker := time.NewTicker(gwInterval)
	defer gwTicker.Stop()
	var scanTicker *time.Ticker
	var scanC <-chan time.Time
	if scanInterval > 0 {
		scanTicker = time.NewTicker(scanInterval)
		defer scanTicker.Stop()
		scanC = scanTicker.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-gwTicker.C:
			m.reconcile(ctx, table)
		case <-scanC:
			m.scanDirectory(ctx)
		}
	}
}

// scanDirectory re-reads the certificate directory on its own schedule.
//
// A failure keeps the current cache, exactly as a database read failure does:
// an unreadable volume must not take every domain down.
func (m *CertManager) scanDirectory(ctx context.Context) {
	if m.cfg.GatewayCertDir == "" {
		return
	}
	if _, err := os.Stat(m.cfg.GatewayCertDir); err != nil {
		log.Printf("[gateway] certificate directory %s is unreadable, keeping current certificates: %v",
			m.cfg.GatewayCertDir, err)
		return
	}
	if err := m.reload(ctx); err != nil {
		log.Printf("[gateway] certificate reload failed, keeping current cache: %v", err)
	}
}

// CertDirSources reports the domains currently served from the shared
// directory, so the console can answer "what is coming from where".
func (m *CertManager) CertDirSources() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []string
	for domain, info := range m.info {
		if info.Source == storageCertSourceFile {
			out = append(out, domain)
		}
	}
	sort.Strings(out)
	return out
}

// reconcile looks at every served domain and decides what it needs.
func (m *CertManager) reconcile(ctx context.Context, table *Table) {
	if err := m.reload(ctx); err != nil {
		log.Printf("[gateway] certificate reload failed, keeping current cache: %v", err)
	}
	if m.issuer.enabled && !m.accountReady() {
		if err := m.issuer.registerAccount(ctx); err != nil {
			log.Printf("[gateway] acme account unavailable: %v", err)
		} else {
			m.markAccountReady()
		}
	}

	domains := table.Domains
	// Deterministic order: two gateway replicas refreshing at the same moment
	// then make the same issuance attempt in the same sequence, which keeps a
	// replicated database from recording two different failures for one domain.
	sort.Strings(domains)

	for _, domain := range domains {
		info, has := m.Info(domain)
		switch {
		case !has:
			m.provisionFallback(ctx, domain)
			m.attemptIssuance(ctx, domain)
		case info.Source == storageCertSourceFile:
			// Another tool owns this certificate and its renewal schedule.
			// Renewing it here would fight that tool and, when the two
			// disagree about expiry, would quietly take the domain down.
			// Watching for it to expire is still this gateway's job.
			if info.Expired() {
				log.Printf("[gateway] WARNING: certificate for %s from %s expired on %s",
					domain, info.FilePath, info.NotAfter.Format(time.RFC3339))
			}
		case info.Source == storage.CertSourceACME:
			if m.needsRenewal(info) {
				m.attemptIssuance(ctx, domain)
			}
		case info.Source == storage.CertSourceSelfSigned:
			// A fallback is only acceptable while it is still serving. When
			// it approaches expiry the ACME path is retried; if the CA can
			// never serve this name, the fallback is simply regenerated.
			if m.needsRenewal(info) {
				if m.acmePossible(domain) {
					m.attemptIssuance(ctx, domain)
				} else {
					m.provisionFallback(ctx, domain)
				}
			}
		default:
			// A manually uploaded certificate is never touched automatically.
			// Nobody else can know what it should be renewed with, and
			// replacing an operator's certificate is worse than letting it
			// expire visibly.
			if info.Expired() {
				log.Printf("[gateway] WARNING: manually uploaded certificate for %s expired on %s",
					domain, info.NotAfter.Format(time.RFC3339))
			}
		}
	}
}

// needsRenewal reports whether a certificate is inside its renewal window.
func (m *CertManager) needsRenewal(info CertInfo) bool {
	if info.Expired() {
		return true
	}
	window := time.Duration(storage.DefaultRenewBeforeDays) * 24 * time.Hour
	if info.Source == storage.CertSourceSelfSigned {
		window = selfSignedRenewBefore
	}
	return time.Now().Add(window).After(info.NotAfter)
}

// acmePossible is the eligibility guard, applied before any CA traffic.
func (m *CertManager) acmePossible(domain string) bool {
	if !m.issuer.enabled {
		return false
	}
	if !eligibleForACME(domain) {
		return false
	}
	return resolvablePublicly(domain)
}

// attemptIssuance runs the ACME path for one domain, respecting backoff.
//
// A failure is recorded per domain, and the next attempt waits 5 minutes, then
// 10, then 20 and so on up to six hours. A domain that the CA structurally
// cannot serve therefore costs a handful of slots in its first hour and then
// essentially nothing.
func (m *CertManager) attemptIssuance(ctx context.Context, domain string) {
	if !m.acmePossible(domain) {
		return
	}
	// A domain inside its retry wait is skipped silently: the wait exists
	// precisely so that a domain the CA cannot serve stops consuming failure
	// budget, and logging every skipped cycle would bury the real failures.
	if _, waiting := m.backoffRemaining(domain); waiting {
		return
	}
	m.issueMu.Lock()
	defer m.issueMu.Unlock()

	// Record the attempt before talking to the CA, so a second domain in the
	// same cycle cannot re-enter this path while this one is mid-conversation.
	m.recordAttempt(domain)
	time.Sleep(issuanceSerialInterval)

	issueCtx, cancel := context.WithTimeout(ctx, time.Duration(m.cfg.ACMETimeout)*time.Second)
	defer cancel()

	certPEM, keyPEM, err := m.issuer.issue(issueCtx, domain, m.setChallenge)
	if err != nil {
		log.Printf("[gateway] acme issuance failed for %s: %v", domain, err)
		m.recordFailure(domain)
		_ = m.st.SetCertificateStatus(ctx, m.ns, domain, storage.CertStatusFailed, err.Error())
		return
	}
	if err := m.storeIssued(ctx, domain, certPEM, keyPEM); err != nil {
		log.Printf("[gateway] could not store issued certificate for %s: %v", domain, err)
		m.recordFailure(domain)
		return
	}
	m.recordSuccess(domain)
	log.Printf("[gateway] issued certificate for %s", domain)
}

// storeIssued persists a freshly obtained certificate and puts it in the cache.
func (m *CertManager) storeIssued(ctx context.Context, domain, certPEM, keyPEM string) error {
	cert, leaf, err := parseCertificate(certPEM, keyPEM)
	if err != nil {
		return err
	}
	if _, err := m.st.UpsertCertificate(ctx, storage.Certificate{
		NamespaceId:     m.ns,
		Domain:          domain,
		Source:          storage.CertSourceACME,
		Issuer:          issuerOf(leaf, ""),
		CertPEM:         certPEM,
		KeyPEM:          keyPEM,
		NotBefore:       leaf.NotBefore,
		NotAfter:        leaf.NotAfter,
		RenewBeforeDays: storage.DefaultRenewBeforeDays,
		Status:          storage.CertStatusActive,
	}); err != nil {
		return err
	}
	m.install(domain, cert, CertInfo{
		Domain:    domain,
		Source:    storage.CertSourceACME,
		Issuer:    issuerOf(leaf, ""),
		NotBefore: leaf.NotBefore,
		NotAfter:  leaf.NotAfter,
		Status:    storage.CertStatusActive,
	})
	m.mirrorToDir(domain, certPEM, keyPEM)
	return nil
}

// mirrorToDir publishes an issued certificate into the shared directory.
//
// The in-memory and database copies are already in place by this point, so a
// directory that is unwritable costs redundancy, not service: the gateway keeps
// serving. That is why the failure is logged rather than raised.
func (m *CertManager) mirrorToDir(domain, certPEM, keyPEM string) {
	if !m.cfg.GatewayCertWriteBack || m.cfg.GatewayCertDir == "" {
		return
	}
	if err := writeCertPair(m.cfg.GatewayCertDir, domain, certPEM, keyPEM); err != nil {
		log.Printf("[gateway] could not mirror certificate for %s into %s: %v",
			domain, m.cfg.GatewayCertDir, err)
		return
	}
	log.Printf("[gateway] mirrored certificate for %s to %s", domain, m.cfg.GatewayCertDir)
}

// install publishes a certificate to the serving cache.
//
// In-memory publication is deliberately separate from persistence: the cache
// is what the TLS handshake reads, and a database hiccup must not leave the
// gateway still serving the old certificate while holding a perfectly good new
// one.
func (m *CertManager) install(domain string, cert *tls.Certificate, info CertInfo) {
	m.mu.Lock()
	m.certs[storage.NormalizeDomain(domain)] = cert
	m.info[storage.NormalizeDomain(domain)] = info
	m.mu.Unlock()
	m.clearOnDemand(domain)
}

// provisionFallback guarantees a served domain has something to hand out.
func (m *CertManager) provisionFallback(ctx context.Context, domain string) {
	certPEM, keyPEM, notAfter, err := generateSelfSigned(domain, m.cfg.SelfSignedDir)
	if err != nil {
		log.Printf("[gateway] could not generate fallback certificate for %s: %v", domain, err)
		return
	}
	cert, leaf, err := parseCertificate(certPEM, keyPEM)
	if err != nil {
		return
	}
	if _, err := m.st.UpsertCertificate(ctx, storage.Certificate{
		NamespaceId: m.ns,
		Domain:      domain,
		Source:      storage.CertSourceSelfSigned,
		Issuer:      selfSignedSubject(domain),
		CertPEM:     certPEM,
		KeyPEM:      keyPEM,
		NotBefore:   leaf.NotBefore,
		NotAfter:    notAfter,
		Status:      storage.CertStatusActive,
	}); err != nil {
		log.Printf("[gateway] could not store fallback certificate for %s: %v", domain, err)
	}
	m.install(domain, cert, CertInfo{
		Domain:    domain,
		Source:    storage.CertSourceSelfSigned,
		Issuer:    selfSignedSubject(domain),
		NotBefore: leaf.NotBefore,
		NotAfter:  notAfter,
		Status:    storage.CertStatusActive,
	})
}

// clearOnDemand drops a minted fallback for a domain that now has a stored one.
func (m *CertManager) clearOnDemand(domain string) {
	m.onDemandMu.Lock()
	delete(m.onDemand, domain)
	m.onDemandMu.Unlock()
}

func (m *CertManager) recordAttempt(domain string) {
	m.failMu.Lock()
	defer m.failMu.Unlock()
	rec, ok := m.failures[domain]
	if !ok {
		rec = &failureRecord{}
		m.failures[domain] = rec
	}
	rec.next = time.Now().Add(m.backoffFor(rec.count))
}

func (m *CertManager) recordSuccess(domain string) {
	m.failMu.Lock()
	defer m.failMu.Unlock()
	delete(m.failures, domain)
}

// recordFailure lengthens the wait before the next attempt.
func (m *CertManager) recordFailure(domain string) {
	m.failMu.Lock()
	defer m.failMu.Unlock()
	rec, ok := m.failures[domain]
	if !ok {
		rec = &failureRecord{}
		m.failures[domain] = rec
	}
	rec.count++
	// Jitter, so a fleet of gateways that all failed at once does not retry
	// in lockstep and reproduce the same burst.
	jitter := time.Duration(rand.Int63n(int64(rec.count) * int64(time.Second)))
	rec.next = time.Now().Add(m.backoffFor(rec.count) + jitter)
}

func (m *CertManager) backoffFor(count int) time.Duration {
	d := acmeBackoffBase
	for i := 1; i < count && d < maxIssuanceBackoff; i++ {
		d *= 2
	}
	if d > maxIssuanceBackoff {
		d = maxIssuanceBackoff
	}
	return d
}

// backoffRemaining reports whether a domain is still inside its retry wait.
func (m *CertManager) backoffRemaining(domain string) (time.Duration, bool) {
	m.failMu.Lock()
	defer m.failMu.Unlock()
	rec, ok := m.failures[domain]
	if !ok {
		return 0, false
	}
	if !time.Now().Before(rec.next) {
		return 0, false
	}
	return time.Until(rec.next), true
}

// accountReady reports whether the ACME account has been established.
//
// Registration is attempted at most once per process. Retrying it on every
// reconcile cycle would put a pointless CA request on the wire every few
// seconds for the lifetime of the gateway, which is exactly the kind of
// ambient traffic that gets an operator rate-limited by a CA for no reason.
func (m *CertManager) accountReady() bool {
	m.accountMu.Lock()
	defer m.accountMu.Unlock()
	return m.accountOK
}

func (m *CertManager) markAccountReady() {
	m.accountMu.Lock()
	m.accountOK = true
	m.accountMu.Unlock()
}

// ACMEStatus is the console-facing view of why issuance is or is not happening.
type ACMEStatus struct {
	Enabled     bool   `json:"enabled"`
	Directory   string `json:"directory"`
	Email       string `json:"email"`
	DataDir     string `json:"dataDir"`
	ChallengeOK bool   `json:"challengeEndpoint"`
	Pending     int    `json:"pending"`
	BackingOff  int    `json:"backingOff"`
}

// Status reports ACME health for the console.
//
// BackingOff is the number that matters operationally: a non-zero value means
// issuance is deliberately paused somewhere, and each backed-off domain is a
// site whose certificate will never arrive on its own.
func (m *CertManager) Status(now time.Time) ACMEStatus {
	m.failMu.Lock()
	backingOff := 0
	for _, rec := range m.failures {
		if now.Before(rec.next) {
			backingOff++
		}
	}
	m.failMu.Unlock()

	m.challengeMu.RLock()
	pending := len(m.challenges)
	m.challengeMu.RUnlock()

	return ACMEStatus{
		Enabled:     m.issuer.enabled,
		Directory:   m.issuer.directory,
		Email:       m.issuer.email,
		DataDir:     m.issuer.dataDir,
		ChallengeOK: true,
		Pending:     pending,
		BackingOff:  backingOff,
	}
}

// DomainsNeedingCerts lists served domains with no certificate at all, so the
// console can show work that is outstanding rather than only problems that
// already happened.
func (m *CertManager) DomainsNeedingCerts(table *Table) []string {
	var out []string
	for _, d := range table.Domains {
		if _, ok := m.Info(d); !ok {
			out = append(out, d)
		}
	}
	return out
}

// StoreManual persists an operator-supplied certificate.
//
// Manual certificates deliberately bypass every automatic path: once someone
// uploads a certificate from an internal CA, the gateway must not keep asking a
// public CA for a replacement.
func (m *CertManager) StoreManual(ctx context.Context, domain, certPEM, keyPEM string) (CertInfo, error) {
	cert, leaf, err := parseCertificate(certPEM, keyPEM)
	if err != nil {
		return CertInfo{}, err
	}
	domain = storage.NormalizeDomain(domain)
	if err := leaf.VerifyHostname(domain); err != nil {
		// A certificate that does not cover the domain it is stored under
		// would be served to exactly the wrong traffic. Refuse it here,
		// where the operator can see the reason.
		return CertInfo{}, fmt.Errorf("certificate does not cover %s: %w", domain, err)
	}
	if _, err := m.st.UpsertCertificate(ctx, storage.Certificate{
		NamespaceId: m.ns,
		Domain:      domain,
		Source:      storage.CertSourceManual,
		Issuer:      issuerOf(leaf, ""),
		CertPEM:     certPEM,
		KeyPEM:      keyPEM,
		NotBefore:   leaf.NotBefore,
		NotAfter:    leaf.NotAfter,
		Status:      storage.CertStatusActive,
	}); err != nil {
		return CertInfo{}, err
	}
	info := CertInfo{
		Domain:    domain,
		Source:    storage.CertSourceManual,
		Issuer:    issuerOf(leaf, ""),
		NotBefore: leaf.NotBefore,
		NotAfter:  leaf.NotAfter,
		Status:    storage.CertStatusActive,
	}
	m.install(domain, cert, info)
	return info, nil
}

// Forget removes a domain from the cache and stops automatic work on it. It is
// what a certificate revocation calls.
func (m *CertManager) Forget(domain string) {
	domain = storage.NormalizeDomain(domain)
	m.mu.Lock()
	delete(m.certs, domain)
	delete(m.info, domain)
	m.mu.Unlock()
	m.clearOnDemand(domain)
	m.failMu.Lock()
	delete(m.failures, domain)
	m.failMu.Unlock()
}

// Reload re-reads certificates from the database without waiting for the next
// tick, so an operator who just uploaded a certificate sees it take effect
// immediately instead of after a refresh interval.
func (m *CertManager) Reload(ctx context.Context) error { return m.reload(ctx) }

// ActiveDomains returns every domain with a loaded certificate.
func (m *CertManager) ActiveDomains() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.certs))
	for d := range m.certs {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

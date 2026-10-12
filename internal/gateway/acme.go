package gateway

// Automatic certificate issuance over ACME (RFC 8555).
//
// # Why issuance is a default rather than a feature flag
//
// A gateway that needs a certificate installed by hand before it can serve a
// domain is a gateway operators will route around. Because the whole point of
// this subsystem is that a frontend works the moment its route is declared,
// issuance runs by default and the manual path is the fallback.
//
// # Why the eligibility precheck exists
//
// Public CAs enforce strict failure limits — five failed authorizations per
// account per hour on Let's Encrypt. A gateway that blindly attempts issuance
// for every declared domain would burn that budget on internal names it can
// never get a certificate for ("api.internal", "localhost", "10.0.0.7") and
// then be unable to issue for the one real public domain it has. So a domain
// must clear a precheck before a single request reaches the CA.
//
// # Why HTTP-01 and not DNS-01
//
// HTTP-01 needs no API credentials and works behind any load balancer, which
// makes it the only variant an operator can set up without provisioning a DNS
// provider account. It requires the gateway to answer on port 80 for the
// domain being issued, which is exactly what the gateway's HTTP listener does.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/acme"

	"github.com/halfking/goacos/internal/storage"
)

// acmeIssuer obtains certificates from one ACME directory.
type acmeIssuer struct {
	enabled   bool
	directory string
	email     string
	dataDir   string
	client    *acme.Client
}

// newACMEIssuer prepares the ACME client and its account key.
//
// The account key is created once and reused forever. Losing it means creating
// a new account, which means a fresh rate-limit bucket — and a fleet of
// gateway replicas that each generated their own would collectively exhaust
// limits that a single account would never approach. That is why it lives in a
// file that operators are told to back up, not in the database.
func newACMEIssuer(directory, email, dataDir string, timeout time.Duration) (*acmeIssuer, error) {
	a := &acmeIssuer{enabled: directory != "", directory: directory, email: email, dataDir: dataDir}
	if !a.enabled {
		return a, nil
	}
	key, err := loadOrCreateAccountKey(dataDir)
	if err != nil {
		return nil, fmt.Errorf("acme account key: %w", err)
	}
	a.client = &acme.Client{
		Key:          key,
		DirectoryURL: directory,
		UserAgent:    "goacos-gateway",
		HTTPClient:   &http.Client{Timeout: timeout},
	}
	return a, nil
}

func accountKeyPath(dir string) string { return filepath.Join(dir, "account.key") }

// loadOrCreateAccountKey reads the ACME account key, minting one on first use.
func loadOrCreateAccountKey(dir string) (*ecdsa.PrivateKey, error) {
	dir = writableDir(dir, "acme")
	if dir != "" {
		if raw, err := os.ReadFile(accountKeyPath(dir)); err == nil {
			if key, err := parseECKey(raw); err == nil {
				return key, nil
			}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	if dir == "" {
		return key, nil
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	// Written before first use so a crash mid-registration cannot leave a
	// half-initialised key that the next boot would silently replace.
	if err := os.WriteFile(accountKeyPath(dir),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, fmt.Errorf("write account key: %w", err)
	}
	return key, nil
}

func parseECKey(raw []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("account key is not PEM")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

// eligibleForACME reports whether a domain is worth asking a public CA about.
//
// This is the guard that keeps failure limits for internal names from being
// spent against the operator's real domains. It is deliberately conservative:
// a false negative costs an operator one manual certificate, a false positive
// costs rate limit that other domains need.
func eligibleForACME(domain string) bool {
	d := storage.NormalizeDomain(hostOnly(domain))
	if d == "" || strings.HasPrefix(d, "*.") {
		return false
	}
	// Wildcards are rejected: they cannot be validated over HTTP-01 alone, and
	// a gateway serving a wildcard certificate would need a different model
	// for which instance answers for a name it has never seen.
	if strings.HasPrefix(d, "*") {
		return false
	}
	if net.ParseIP(d) != nil {
		// A public CA will not issue for an IP address; asking wastes a slot.
		return false
	}
	lower := strings.ToLower(d)
	for _, suffix := range []string{
		".local", ".localhost", ".internal", ".intranet", ".lan", ".home",
		".test", ".example", ".invalid", ".svc", ".cluster.local",
	} {
		if strings.HasSuffix(lower, suffix) {
			return false
		}
	}
	if lower == "localhost" || !strings.Contains(lower, ".") {
		return false
	}
	return true
}

// resolvablePublicly reports whether the domain currently resolves to a public
// address.
//
// This is the second half of the eligibility guard. The ACME CA will resolve
// the name from its own network; if it points at a private address or at
// nothing, validation fails and the attempt is charged against the failure
// limit. Checking first turns "we will lose a slot" into "we simply do not
// try yet".
func resolvablePublicly(domain string) bool {
	ips, err := net.LookupIP(strings.TrimPrefix(strings.ToLower(domain), "*."))
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			return false
		}
	}
	return true
}

// hostOnly strips a port from an authority string.
func hostOnly(authority string) string {
	if h, _, err := net.SplitHostPort(authority); err == nil {
		return h
	}
	return authority
}

// issue obtains a certificate for one domain using HTTP-01.
//
// serveChallenge is called for each token that must answer on port 80. The
// caller owns keeping it available until validation finishes, which is why it
// is a callback and not a side effect of this function.
func (a *acmeIssuer) issue(ctx context.Context, domain string, serveChallenge func(token, keyAuth string)) (certPEM, keyPEM string, err error) {
	if !a.enabled || a.client == nil {
		return "", "", errors.New("acme is disabled")
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("generate key: %w", err)
	}

	order, err := a.client.AuthorizeOrder(ctx, acme.DomainIDs(domain))
	if err != nil {
		return "", "", fmt.Errorf("authorize order: %w", err)
	}

	// An order can come back already partly valid (a re-issued domain with an
	// unexpired authorization), so every authorization is inspected rather
	// than assumed to be pending.
	for _, authzURL := range order.AuthzURLs {
		authz, err := a.client.GetAuthorization(ctx, authzURL)
		if err != nil {
			return "", "", fmt.Errorf("get authorization: %w", err)
		}
		switch authz.Status {
		case acme.StatusValid:
			continue
		case acme.StatusPending:
		default:
			return "", "", fmt.Errorf("authorization for %s is %s", domain, authz.Status)
		}
		ch, err := http01Challenge(authz)
		if err != nil {
			return "", "", err
		}
		response, err := a.client.HTTP01ChallengeResponse(ch.Token)
		if err != nil {
			return "", "", fmt.Errorf("challenge response: %w", err)
		}
		// The challenge must stay answerable for the whole validation window,
		// so it is registered for the lifetime of this call and removed when
		// the function returns — success or failure.
		serveChallenge(ch.Token, response)
		defer func() { serveChallenge(ch.Token, "") }()
		if _, err := a.client.Accept(ctx, ch); err != nil {
			return "", "", fmt.Errorf("accept challenge: %w", err)
		}
	}

	order, err = a.client.WaitOrder(ctx, order.URI)
	if err != nil {
		return "", "", fmt.Errorf("wait for order: %w", err)
	}
	if order.Status != acme.StatusReady {
		return "", "", fmt.Errorf("order for %s is %s", domain, order.Status)
	}

	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: domain},
		DNSNames: []string{domain},
	}, priv)
	if err != nil {
		return "", "", fmt.Errorf("create csr: %w", err)
	}
	chains, _, err := a.client.CreateOrderCert(ctx, order.URI, csrDER, true)
	if err != nil {
		return "", "", fmt.Errorf("finalize order: %w", err)
	}
	if len(chains) == 0 {
		return "", "", errors.New("ca returned an empty certificate chain")
	}
	// The full chain is stored, not just the leaf: an intermediate is what
	// makes the certificate validate in a browser that does not happen to
	// cache it.
	var certBuf strings.Builder
	for _, der := range chains {
		certBuf.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return "", "", fmt.Errorf("marshal key: %w", err)
	}
	return certBuf.String(), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})), nil
}

// http01Challenge picks the HTTP-01 challenge out of an authorization.
func http01Challenge(authz *acme.Authorization) (*acme.Challenge, error) {
	for _, ch := range authz.Challenges {
		if ch.Type == "http-01" {
			return ch, nil
		}
	}
	return nil, fmt.Errorf("no http-01 challenge offered for %s", authz.Identifier.Value)
}

// registerAccount creates the ACME account, tolerating an existing one.
//
// Re-registering an account that already exists is not an error worth failing
// on: it means this account key was used before, which is the normal case on
// every boot after the first. Only an unrecognised failure aborts, because
// continuing past a real registration problem would produce a confusing
// failure at the first order instead.
func (a *acmeIssuer) registerAccount(ctx context.Context) error {
	if !a.enabled || a.client == nil {
		return errors.New("acme is disabled")
	}
	acct := &acme.Account{}
	if a.email != "" {
		acct.Contact = []string{"mailto:" + a.email}
	}
	_, err := a.client.Register(ctx, acct, acme.AcceptTOS)
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "already") || strings.Contains(msg, "accountDoesNotExist") {
		return nil
	}
	return fmt.Errorf("register acme account: %w", err)
}

// selfSignedSubject is reused by the console warning so the operator sees the
// same words in the certificate store and in the log.
func selfSignedSubject(domain string) string {
	return "goacos gateway (self-signed): " + domain
}

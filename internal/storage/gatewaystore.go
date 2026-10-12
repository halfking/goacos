package storage

// Gateway control-plane persistence: certificates and per-domain traffic policy.
//
// # Why certificates live in the database
//
// Because a certificate has to be auditable and revocable by the same person
// who declares a route. A certificate sitting in /etc/nginx on one host cannot
// answer "what is this domain serving with today" across a cluster, and
// revoking it means SSH plus a reload. Storing the PEM next to the routing
// policy makes the full answer one query, and makes revocation one DELETE.
//
// # Why the private key is stored at all
//
// Some deployments cannot reach an ACME CA at all (air-gapped, or an internal
// PKI behind a corporate proxy). Those still need TLS on the frontend. The key
// column exists so an operator can paste a certificate from an internal issuer
// and have the gateway serve it without a filesystem handoff. It is the same
// trust boundary as the database itself: whoever can read this table could
// already read the admin credentials.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Certificate sources. The value is surfaced in the console because "this
// domain is on a self-signed certificate" is the warning an operator actually
// needs to see — a browser will happily accept it and the outage it causes is
// invisible until a real user opens the site.
const (
	CertSourceManual     = "manual"
	CertSourceACME       = "acme"
	CertSourceSelfSigned = "self_signed"
	// CertSourceFile marks a certificate discovered in the shared certificate
	// directory rather than issued or uploaded through this server.
	//
	// It is a reporting value only. Such rows are never written to the table:
	// the file on disk is the authoritative copy, and storing a duplicate here
	// would recreate exactly the second source of truth this model exists to
	// eliminate.
	CertSourceFile = "file"
)

// Certificate lifecycle status.
const (
	CertStatusActive   = "active"
	CertStatusExpiring = "expiring"
	CertStatusPending  = "pending"
	CertStatusFailed   = "failed"
)

// DefaultRenewBeforeDays is how far ahead of expiry a certificate enters the
// renewal window. 21 days is well inside Let's Encrypt's 90-day lifetime and
// leaves room for several failed attempts against a flaky CA endpoint.
const DefaultRenewBeforeDays = 21

// Certificate is one domain's TLS material plus how it got here.
type Certificate struct {
	NamespaceId string
	Domain      string
	Source      string
	Issuer      string
	CertPEM     string
	KeyPEM      string
	NotBefore   time.Time
	NotAfter    time.Time
	// RenewBeforeDays is the renewal window for this certificate. Only
	// meaningful for ACME-issued ones; a manually uploaded certificate is
	// never auto-renewed, because nothing in this process can know what to
	// renew it with.
	RenewBeforeDays int
	AcmeOrderURL    string
	Status          string
	LastError       string
	GmtCreate       time.Time
	GmtModified     time.Time
}

const certCols = `namespace_id, domain, source, issuer, cert_pem, key_pem,
                  not_before, not_after, renew_before_days, acme_order_url,
                  status, COALESCE(last_error,''), gmt_create, gmt_modified`

func scanCertificate(row interface{ Scan(...any) error }) (*Certificate, error) {
	var c Certificate
	if err := row.Scan(&c.NamespaceId, &c.Domain, &c.Source, &c.Issuer, &c.CertPEM, &c.KeyPEM,
		&c.NotBefore, &c.NotAfter, &c.RenewBeforeDays, &c.AcmeOrderURL,
		&c.Status, &c.LastError, &c.GmtCreate, &c.GmtModified); err != nil {
		return nil, err
	}
	return &c, nil
}

// UpsertCertificate stores or replaces one domain's certificate and reports
// whether a new row was created.
//
// Replacement is the normal path for renewal, so callers must not assume a
// second call is a duplicate: an ACME renewal writes the same (namespace,
// domain) key with new material.
func (s *Store) UpsertCertificate(ctx context.Context, c Certificate) (bool, error) {
	if c.Source == "" {
		c.Source = CertSourceManual
	}
	if c.Status == "" {
		c.Status = CertStatusActive
	}
	if c.RenewBeforeDays <= 0 {
		c.RenewBeforeDays = DefaultRenewBeforeDays
	}
	if c.NotBefore.IsZero() {
		c.NotBefore = time.Now()
	}
	if c.NotAfter.IsZero() {
		c.NotAfter = time.Now().AddDate(10, 0, 0)
	}
	existing, err := s.GetCertificate(ctx, c.NamespaceId, c.Domain)
	if err != nil {
		return false, err
	}
	if existing != nil {
		_, err := s.Exec(ctx, `UPDATE gateway_certificate SET source=?, issuer=?, cert_pem=?, key_pem=?,
			not_before=?, not_after=?, renew_before_days=?, acme_order_url=?, status=?, last_error=?,
			gmt_modified=CURRENT_TIMESTAMP WHERE namespace_id=? AND domain=?`,
			c.Source, c.Issuer, c.CertPEM, c.KeyPEM, c.NotBefore, c.NotAfter,
			c.RenewBeforeDays, c.AcmeOrderURL, c.Status, c.LastError, c.NamespaceId, c.Domain)
		return false, err
	}
	_, err = s.Exec(ctx, `INSERT INTO gateway_certificate (namespace_id, domain, source, issuer, cert_pem,
		key_pem, not_before, not_after, renew_before_days, acme_order_url, status, last_error)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.NamespaceId, c.Domain, c.Source, c.Issuer, c.CertPEM, c.KeyPEM,
		c.NotBefore, c.NotAfter, c.RenewBeforeDays, c.AcmeOrderURL, c.Status, c.LastError)
	return true, err
}

// GetCertificate returns one domain's certificate or nil when absent.
// Absence is not an error: a domain with no certificate is a normal state the
// gateway handles by falling back rather than refusing to start.
func (s *Store) GetCertificate(ctx context.Context, ns, domain string) (*Certificate, error) {
	c, err := scanCertificate(s.QueryRow(ctx,
		"SELECT "+certCols+" FROM gateway_certificate WHERE namespace_id=? AND domain=?", ns, domain))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// ListCertificates returns every certificate in a namespace, domain ascending.
func (s *Store) ListCertificates(ctx context.Context, ns string) ([]Certificate, error) {
	rows, err := s.Query(ctx, "SELECT "+certCols+" FROM gateway_certificate WHERE namespace_id=? ORDER BY domain", ns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Certificate
	for rows.Next() {
		c, err := scanCertificate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// ListCertificatesExpiringBefore returns certificates whose not_after falls on
// or before the given instant. This is the renewal sweep's only query: it must
// return renewals and near-expiry warnings alike, because an operator seeing
// "3 days left" and not being able to fix it is the failure mode this exists to
// prevent.
func (s *Store) ListCertificatesExpiringBefore(ctx context.Context, ns string, before time.Time) ([]Certificate, error) {
	rows, err := s.Query(ctx, "SELECT "+certCols+
		" FROM gateway_certificate WHERE namespace_id=? AND not_after<=? ORDER BY not_after", ns, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Certificate
	for rows.Next() {
		c, err := scanCertificate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// SetCertificateStatus records an issuance outcome without touching the
// certificate material. Issuance fails far more often than it succeeds, and
// the failure has to survive a restart to be worth anything.
func (s *Store) SetCertificateStatus(ctx context.Context, ns, domain, status, lastErr string) error {
	_, err := s.Exec(ctx, `UPDATE gateway_certificate SET status=?, last_error=?,
		gmt_modified=CURRENT_TIMESTAMP WHERE namespace_id=? AND domain=?`,
		status, lastErr, ns, domain)
	return err
}

// DeleteCertificate removes a domain's certificate. The gateway notices on its
// next refresh and falls back, so revoking a compromised certificate is a
// single call with no reload step.
func (s *Store) DeleteCertificate(ctx context.Context, ns, domain string) error {
	_, err := s.Exec(ctx, "DELETE FROM gateway_certificate WHERE namespace_id=? AND domain=?", ns, domain)
	return err
}

// VHostPolicy is the per-domain traffic rule set. Zero values mean "inherit the
// gateway default", which is why the struct can be sparse: an operator who only
// wants HSTS on one domain should not have to restate timeouts.
type VHostPolicy struct {
	NamespaceId string
	Domain      string
	// ForceHTTPS makes the HTTP listener redirect rather than proxy. It
	// defaults to true, because serving a login page over plain HTTP while the
	// same domain also has a certificate is almost never intended.
	ForceHTTPS bool
	// HSTSMaxAge, when > 0, emits a Strict-Transport-Security header.
	HSTSMaxAge int
	// AllowIPs and DenyIPs are newline-separated CIDRs or bare addresses.
	// Deny is evaluated first, so a deny wins over an allow — the safe
	// direction when both name the same client.
	AllowIPs string
	DenyIPs  string
	// ClientMaxBodyBytes, when > 0, rejects larger request bodies with 413.
	ClientMaxBodyBytes int64
	ConnectTimeoutMS   int
	ReadTimeoutMS      int
	SendTimeoutMS      int
	// Websocket keeps hijacking available for this domain. Turning it off
	// matters because an upgrade-capable proxy is an open tunnel to whatever
	// is behind it.
	Websocket   bool
	Metadata    map[string]string
	GmtCreate   time.Time
	GmtModified time.Time
}

const vhostCols = `namespace_id, domain, force_https, hsts_max_age,
                  COALESCE(allow_ips,''), COALESCE(deny_ips,''), client_max_body_bytes,
                  connect_timeout_ms, read_timeout_ms, send_timeout_ms, websocket,
                  COALESCE(metadata,''), gmt_create, gmt_modified`

func scanVHost(row interface{ Scan(...any) error }) (*VHostPolicy, error) {
	var v VHostPolicy
	var meta string
	if err := row.Scan(&v.NamespaceId, &v.Domain, &v.ForceHTTPS, &v.HSTSMaxAge,
		&v.AllowIPs, &v.DenyIPs, &v.ClientMaxBodyBytes,
		&v.ConnectTimeoutMS, &v.ReadTimeoutMS, &v.SendTimeoutMS, &v.Websocket,
		&meta, &v.GmtCreate, &v.GmtModified); err != nil {
		return nil, err
	}
	v.Metadata = decodeMeta(meta)
	return &v, nil
}

// UpsertVHost stores or replaces one domain's traffic policy.
func (s *Store) UpsertVHost(ctx context.Context, v VHostPolicy) (bool, error) {
	existing, err := s.GetVHost(ctx, v.NamespaceId, v.Domain)
	if err != nil {
		return false, err
	}
	if existing != nil {
		_, err := s.Exec(ctx, `UPDATE gateway_vhost SET force_https=?, hsts_max_age=?, allow_ips=?, deny_ips=?,
			client_max_body_bytes=?, connect_timeout_ms=?, read_timeout_ms=?, send_timeout_ms=?,
			websocket=?, metadata=?, gmt_modified=CURRENT_TIMESTAMP
			WHERE namespace_id=? AND domain=?`,
			flagInt(v.ForceHTTPS), v.HSTSMaxAge, v.AllowIPs, v.DenyIPs, v.ClientMaxBodyBytes,
			v.ConnectTimeoutMS, v.ReadTimeoutMS, v.SendTimeoutMS,
			flagInt(v.Websocket), encodeMeta(v.Metadata), v.NamespaceId, v.Domain)
		return false, err
	}
	_, err = s.Exec(ctx, `INSERT INTO gateway_vhost (namespace_id, domain, force_https, hsts_max_age,
		allow_ips, deny_ips, client_max_body_bytes, connect_timeout_ms, read_timeout_ms, send_timeout_ms,
		websocket, metadata) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		v.NamespaceId, v.Domain, flagInt(v.ForceHTTPS), v.HSTSMaxAge, v.AllowIPs, v.DenyIPs,
		v.ClientMaxBodyBytes, v.ConnectTimeoutMS, v.ReadTimeoutMS, v.SendTimeoutMS,
		flagInt(v.Websocket), encodeMeta(v.Metadata))
	return true, err
}

// GetVHost returns one domain's policy or nil when the domain has none.
func (s *Store) GetVHost(ctx context.Context, ns, domain string) (*VHostPolicy, error) {
	v, err := scanVHost(s.QueryRow(ctx,
		"SELECT "+vhostCols+" FROM gateway_vhost WHERE namespace_id=? AND domain=?", ns, domain))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return v, nil
}

// ListVHosts returns every policy in a namespace, domain ascending.
func (s *Store) ListVHosts(ctx context.Context, ns string) ([]VHostPolicy, error) {
	rows, err := s.Query(ctx, "SELECT "+vhostCols+" FROM gateway_vhost WHERE namespace_id=? ORDER BY domain", ns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VHostPolicy
	for rows.Next() {
		v, err := scanVHost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

// DeleteVHost removes one domain's policy, returning it to the defaults.
func (s *Store) DeleteVHost(ctx context.Context, ns, domain string) error {
	_, err := s.Exec(ctx, "DELETE FROM gateway_vhost WHERE namespace_id=? AND domain=?", ns, domain)
	return err
}

// flagInt renders a bool for an integer column. Kept as a helper so every call
// site is explicit that these columns are integers in both engines.
func flagInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// NormalizeDomain lowercases and strips a port, so "Example.COM:443" and
// "example.com" resolve to the same certificate. Route and certificate lookups
// both go through this; a mismatch here is a silent 404 for the operator, which
// is the most expensive kind of bug in a TLS path.
func NormalizeDomain(d string) string {
	d = strings.TrimSpace(strings.ToLower(d))
	if i := strings.LastIndex(d, ":"); i > 0 && !strings.Contains(d[i+1:], ":") {
		if _, err := parseNumericPort(d[i+1:]); err == nil {
			d = d[:i]
		}
	}
	return strings.TrimSuffix(d, ".")
}

func parseNumericPort(s string) (int, error) {
	if s == "" {
		return 0, errors.New("empty port")
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("not a port")
		}
		n = n*10 + int(c-'0')
	}
	if n < 1 || n > 65535 {
		return 0, errors.New("port out of range")
	}
	return n, nil
}

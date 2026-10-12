-- Migration 003 — gateway control-plane tables (certificates + vhost policy).
--
-- Why this exists: the application model (migration 002) declares WHAT a
-- domain routes to. It deliberately says nothing about HOW that traffic is
-- carried — no certificate, no TLS policy, no timeout, no access rule. Those
-- are exactly the settings that used to live in a hand-maintained nginx.conf
-- on a single host, which is the second SSOT this project is eliminating.
--
-- Putting them here means: one place to audit what a domain accepts, one place
-- to revoke a certificate, and no "works on the box nobody remembers" state.
--
-- MySQL dialect. The PostgreSQL twin lives in 003_gateway.postgres.sql and must
-- stay structurally identical.
--
-- Flag columns are declared as integer rather than boolean on purpose: MySQL
-- has no true boolean and PostgreSQL returns a native bool that will not scan
-- into an int field. One integer type keeps one scan signature across engines.

CREATE TABLE IF NOT EXISTS gateway_certificate (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  namespace_id varchar(128) NOT NULL DEFAULT 'public',
  domain varchar(255) NOT NULL,
  -- source: manual (uploaded by an operator), acme (issued by an ACME CA),
  -- self_signed (locally generated fallback). The distinction is what lets the
  -- console say "this domain is running on a self-signed certificate", which is
  -- the single most important warning this subsystem can emit.
  source varchar(32) NOT NULL DEFAULT 'manual',
  issuer varchar(255) NOT NULL DEFAULT '',
  cert_pem mediumtext NOT NULL,
  key_pem mediumtext NOT NULL,
  not_before datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  not_after datetime NOT NULL DEFAULT '2099-12-31 23:59:59',
  renew_before_days int NOT NULL DEFAULT 21,
  acme_order_url varchar(512) NOT NULL DEFAULT '',
  status varchar(32) NOT NULL DEFAULT 'active',
  last_error text,
  gmt_create datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_gateway_cert (namespace_id, domain),
  KEY idx_gateway_cert_expiry (not_after),
  KEY idx_gateway_cert_source (source)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- Per-domain traffic policy. A row here overrides the gateway-wide defaults for
-- one domain; the absence of a row means "use the defaults", not "no policy".
CREATE TABLE IF NOT EXISTS gateway_vhost (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  namespace_id varchar(128) NOT NULL DEFAULT 'public',
  domain varchar(255) NOT NULL,
  force_https int NOT NULL DEFAULT 1,
  hsts_max_age int NOT NULL DEFAULT 0,
  allow_ips text NOT NULL,
  deny_ips text NOT NULL,
  client_max_body_bytes bigint NOT NULL DEFAULT 0,
  connect_timeout_ms int NOT NULL DEFAULT 3000,
  read_timeout_ms int NOT NULL DEFAULT 60000,
  send_timeout_ms int NOT NULL DEFAULT 60000,
  websocket int NOT NULL DEFAULT 1,
  metadata text,
  gmt_create datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_gateway_vhost (namespace_id, domain)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- Migration 003 — gateway control-plane tables (certificates + vhost policy).
-- PostgreSQL twin of 003_gateway.mysql.sql. Keep the two structurally identical.
--
-- Flag columns use smallint rather than boolean for the cross-engine reason
-- stated in the MySQL twin: one integer type means one Scan signature works
-- against both drivers.

CREATE TABLE IF NOT EXISTS gateway_certificate (
  id bigserial PRIMARY KEY,
  namespace_id varchar(128) NOT NULL DEFAULT 'public',
  domain varchar(255) NOT NULL,
  source varchar(32) NOT NULL DEFAULT 'manual',
  issuer varchar(255) NOT NULL DEFAULT '',
  cert_pem text NOT NULL,
  key_pem text NOT NULL,
  not_before timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  not_after timestamp NOT NULL DEFAULT '2099-12-31 23:59:59',
  renew_before_days int NOT NULL DEFAULT 21,
  acme_order_url varchar(512) NOT NULL DEFAULT '',
  status varchar(32) NOT NULL DEFAULT 'active',
  last_error text,
  gmt_create timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  CONSTRAINT uk_gateway_cert UNIQUE (namespace_id, domain)
);
CREATE INDEX IF NOT EXISTS idx_gateway_cert_expiry ON gateway_certificate (not_after);
CREATE INDEX IF NOT EXISTS idx_gateway_cert_source ON gateway_certificate (source);

CREATE TABLE IF NOT EXISTS gateway_vhost (
  id bigserial PRIMARY KEY,
  namespace_id varchar(128) NOT NULL DEFAULT 'public',
  domain varchar(255) NOT NULL,
  force_https smallint NOT NULL DEFAULT 1,
  hsts_max_age int NOT NULL DEFAULT 0,
  allow_ips text NOT NULL DEFAULT '',
  deny_ips text NOT NULL DEFAULT '',
  client_max_body_bytes bigint NOT NULL DEFAULT 0,
  connect_timeout_ms int NOT NULL DEFAULT 3000,
  read_timeout_ms int NOT NULL DEFAULT 60000,
  send_timeout_ms int NOT NULL DEFAULT 60000,
  websocket smallint NOT NULL DEFAULT 1,
  metadata text,
  gmt_create timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  CONSTRAINT uk_gateway_vhost UNIQUE (namespace_id, domain)
);
CREATE INDEX IF NOT EXISTS idx_gateway_vhost_domain ON gateway_vhost (domain);

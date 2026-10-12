-- Migration 002 — application / component / route model (goacos-specific).
-- PostgreSQL twin of 002_application.mysql.sql. Keep the two structurally
-- identical: application/migrations.go applies whichever matches the store's
-- dialect, and the storage tests assert both dialects carry the same tables.

CREATE TABLE IF NOT EXISTS application (
  id bigserial PRIMARY KEY,
  namespace_id varchar(128) NOT NULL DEFAULT 'public',
  name varchar(128) NOT NULL,
  display_name varchar(255) DEFAULT NULL,
  description text,
  env varchar(32) NOT NULL DEFAULT '',
  metadata text,
  gmt_create timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  CONSTRAINT uk_application UNIQUE (namespace_id, name)
);
CREATE INDEX IF NOT EXISTS idx_application_name ON application (name);

CREATE TABLE IF NOT EXISTS application_component (
  id bigserial PRIMARY KEY,
  namespace_id varchar(128) NOT NULL DEFAULT 'public',
  app_name varchar(128) NOT NULL,
  name varchar(128) NOT NULL,
  kind varchar(32) NOT NULL DEFAULT 'api',
  protocol varchar(32) NOT NULL DEFAULT 'http',
  service_name varchar(255) NOT NULL,
  group_name varchar(128) NOT NULL DEFAULT 'DEFAULT_GROUP',
  cluster_name varchar(64) NOT NULL DEFAULT '',
  health_path varchar(255) NOT NULL DEFAULT '',
  weight double precision NOT NULL DEFAULT 1,
  lb_algorithm varchar(32) NOT NULL DEFAULT 'round_robin',
  sticky varchar(32) NOT NULL DEFAULT '',
  metadata text,
  gmt_create timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  CONSTRAINT uk_component UNIQUE (namespace_id, app_name, name)
);
CREATE INDEX IF NOT EXISTS idx_component_service ON application_component (namespace_id, service_name);

CREATE TABLE IF NOT EXISTS route_binding (
  id bigserial PRIMARY KEY,
  namespace_id varchar(128) NOT NULL DEFAULT 'public',
  domain varchar(255) NOT NULL,
  path_prefix varchar(255) NOT NULL DEFAULT '/',
  app_name varchar(128) NOT NULL,
  component_name varchar(128) NOT NULL,
  upstream_name varchar(128) NOT NULL DEFAULT '',
  priority int NOT NULL DEFAULT 100,
  metadata text,
  gmt_create timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  CONSTRAINT uk_route UNIQUE (namespace_id, domain, path_prefix)
);
CREATE INDEX IF NOT EXISTS idx_route_domain ON route_binding (namespace_id, domain);

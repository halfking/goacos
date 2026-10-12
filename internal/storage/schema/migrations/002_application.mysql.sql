-- Migration 002 — application / component / route model (goacos-specific).
--
-- Why this exists: the naming model alone (namespace -> group -> service ->
-- instance) cannot express a complete application. A frontend site, its API and
-- its worker are three different services that belong to one product and are
-- reached through named domains. Without an application entity every gateway
-- has to keep its own hand-maintained "domain -> host:port" table, which is
-- exactly how the current two SSOTs drifted apart.
--
-- MySQL dialect. The PostgreSQL twin lives in 002_application.postgres.sql and
-- must stay structurally identical — application/migrations.go applies the file
-- matching the store's dialect.

CREATE TABLE IF NOT EXISTS application (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  namespace_id varchar(128) NOT NULL DEFAULT 'public',
  name varchar(128) NOT NULL,
  display_name varchar(255) DEFAULT NULL,
  description text,
  env varchar(32) NOT NULL DEFAULT '',
  metadata text,
  gmt_create datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_application (namespace_id, name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- A component is one addressable piece of an application: its frontend, its
-- API, a worker, an MCP endpoint. kind says what it IS, protocol says how it
-- is spoken, service_name is the naming service whose instances serve it.
CREATE TABLE IF NOT EXISTS application_component (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  namespace_id varchar(128) NOT NULL DEFAULT 'public',
  app_name varchar(128) NOT NULL,
  name varchar(128) NOT NULL,
  kind varchar(32) NOT NULL DEFAULT 'api',
  protocol varchar(32) NOT NULL DEFAULT 'http',
  service_name varchar(255) NOT NULL,
  group_name varchar(128) NOT NULL DEFAULT 'DEFAULT_GROUP',
  cluster_name varchar(64) NOT NULL DEFAULT '',
  health_path varchar(255) NOT NULL DEFAULT '',
  weight double NOT NULL DEFAULT 1,
  lb_algorithm varchar(32) NOT NULL DEFAULT 'round_robin',
  sticky varchar(32) NOT NULL DEFAULT '',
  metadata text,
  gmt_create datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_component (namespace_id, app_name, name),
  KEY idx_component_service (namespace_id, service_name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- A route binds an inbound domain (and path prefix) to one component. There is
-- deliberately no IP or port column: the instances come from the naming
-- registry at resolve time, which is the whole point of this model.
CREATE TABLE IF NOT EXISTS route_binding (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  namespace_id varchar(128) NOT NULL DEFAULT 'public',
  domain varchar(255) NOT NULL,
  path_prefix varchar(255) NOT NULL DEFAULT '/',
  app_name varchar(128) NOT NULL,
  component_name varchar(128) NOT NULL,
  upstream_name varchar(128) NOT NULL DEFAULT '',
  priority int NOT NULL DEFAULT 100,
  metadata text,
  gmt_create datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_route (namespace_id, domain, path_prefix),
  KEY idx_route_domain (namespace_id, domain)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

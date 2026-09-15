-- goacos — lightweight Nacos-compatible server
-- PostgreSQL schema (mirror of schema.sql for MySQL).
-- Config tables keep the official Apache Nacos table layout so data remains
-- interoperable with a real Nacos deployment backed by PostgreSQL-compatible
-- tooling. Naming tables are goacos-specific.

-- ------------------------------------------------------------------
-- Config center (Nacos-compatible)
-- ------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS config_info (
  id bigserial PRIMARY KEY,
  data_id varchar(255) NOT NULL,
  group_id varchar(128) NOT NULL,
  content text NOT NULL,
  md5 varchar(32) DEFAULT NULL,
  gmt_create timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  src_user text,
  src_ip varchar(50) DEFAULT NULL,
  app_name varchar(128) DEFAULT NULL,
  tenant_id varchar(128) NOT NULL DEFAULT '',
  c_desc varchar(255) DEFAULT NULL,
  c_use varchar(64) DEFAULT NULL,
  effect varchar(64) DEFAULT NULL,
  type varchar(64) DEFAULT NULL,
  c_schema text,
  encrypted_data_key varchar(1024) NOT NULL DEFAULT '',
  CONSTRAINT uk_configinfo_datagrouptenant UNIQUE (data_id, group_id, tenant_id)
);
CREATE INDEX IF NOT EXISTS idx_configinfo_group_id ON config_info (group_id);
CREATE INDEX IF NOT EXISTS idx_configinfo_gmt_create ON config_info (gmt_create);

CREATE TABLE IF NOT EXISTS his_config_info (
  id bigint NOT NULL,
  nid bigserial PRIMARY KEY,
  data_id varchar(255) NOT NULL,
  group_id varchar(128) NOT NULL,
  app_name varchar(128) DEFAULT NULL,
  content text NOT NULL,
  md5 varchar(32) DEFAULT NULL,
  gmt_create timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  src_user text,
  src_ip varchar(50) DEFAULT NULL,
  op_type varchar(10) NOT NULL,
  tenant_id varchar(128) NOT NULL DEFAULT '',
  encrypted_data_key varchar(1024) NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_his_gmt_create ON his_config_info (gmt_create);
CREATE INDEX IF NOT EXISTS idx_his_gmt_modified ON his_config_info (gmt_modified);
CREATE INDEX IF NOT EXISTS idx_his_did ON his_config_info (data_id, group_id, tenant_id);

CREATE TABLE IF NOT EXISTS config_info_beta (
  id bigserial PRIMARY KEY,
  data_id varchar(255) NOT NULL,
  group_id varchar(128) NOT NULL,
  app_name varchar(128) DEFAULT NULL,
  content text NOT NULL,
  beta_ips varchar(1024) DEFAULT NULL,
  md5 varchar(32) DEFAULT NULL,
  gmt_create timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  src_user text,
  src_ip varchar(50) DEFAULT NULL,
  tenant_id varchar(128) NOT NULL DEFAULT '',
  encrypted_data_key varchar(1024) NOT NULL DEFAULT '',
  CONSTRAINT uk_configinfobeta_datagrouptenant UNIQUE (data_id, group_id, tenant_id)
);

CREATE TABLE IF NOT EXISTS config_info_tag (
  id bigserial PRIMARY KEY,
  data_id varchar(255) NOT NULL,
  group_id varchar(128) NOT NULL,
  tenant_id varchar(128) NOT NULL DEFAULT '',
  tag_id varchar(128) NOT NULL,
  app_name varchar(128) DEFAULT NULL,
  content text NOT NULL,
  md5 varchar(32) DEFAULT NULL,
  gmt_create timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  src_user text,
  src_ip varchar(50) DEFAULT NULL,
  CONSTRAINT uk_configinfotag_datagrouptenanttag UNIQUE (data_id, group_id, tenant_id, tag_id)
);

CREATE TABLE IF NOT EXISTS config_tags_relation (
  id bigint NOT NULL,
  tag_name varchar(128) NOT NULL,
  tag_type varchar(64) DEFAULT NULL,
  data_id varchar(255) NOT NULL,
  group_id varchar(128) NOT NULL,
  tenant_id varchar(128) NOT NULL DEFAULT '',
  nid bigserial PRIMARY KEY,
  CONSTRAINT uk_configtagrelation_configidtag UNIQUE (id, tag_name, tag_type)
);
CREATE INDEX IF NOT EXISTS idx_configtags_tenant ON config_tags_relation (tenant_id);

CREATE TABLE IF NOT EXISTS config_info_aggr (
  id bigserial PRIMARY KEY,
  data_id varchar(255) NOT NULL,
  group_id varchar(128) NOT NULL,
  datum_id varchar(255) NOT NULL,
  content text NOT NULL,
  md5 varchar(32) DEFAULT NULL,
  gmt_create timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  src_user text,
  src_ip varchar(50) DEFAULT NULL,
  app_name varchar(128) DEFAULT NULL,
  tenant_id varchar(128) NOT NULL DEFAULT '',
  CONSTRAINT uk_configinfoaggr_datagrouptenantdatum UNIQUE (data_id, group_id, tenant_id, datum_id)
);

CREATE TABLE IF NOT EXISTS group_capacity (
  id bigserial PRIMARY KEY,
  group_id varchar(128) NOT NULL DEFAULT '',
  quota integer NOT NULL DEFAULT 0,
  "usage" integer NOT NULL DEFAULT 0,
  max_size integer NOT NULL DEFAULT 0,
  max_aggr_count integer NOT NULL DEFAULT 0,
  max_aggr_size integer NOT NULL DEFAULT 0,
  max_history_count integer NOT NULL DEFAULT 0,
  gmt_create timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  CONSTRAINT uk_groupcap_group_id UNIQUE (group_id)
);

CREATE TABLE IF NOT EXISTS tenant_capacity (
  id bigserial PRIMARY KEY,
  tenant_id varchar(128) NOT NULL DEFAULT '',
  quota integer NOT NULL DEFAULT 0,
  "usage" integer NOT NULL DEFAULT 0,
  max_size integer NOT NULL DEFAULT 0,
  max_aggr_count integer NOT NULL DEFAULT 0,
  max_aggr_size integer NOT NULL DEFAULT 0,
  max_history_count integer NOT NULL DEFAULT 0,
  gmt_create timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  CONSTRAINT uk_tenantcap_tenant_id UNIQUE (tenant_id)
);

CREATE TABLE IF NOT EXISTS tenant_info (
  id bigserial PRIMARY KEY,
  kp varchar(128) NOT NULL,
  tenant_id varchar(128) NOT NULL DEFAULT '',
  namespace_name varchar(128) DEFAULT NULL,
  namespace_desc varchar(255) DEFAULT NULL,
  gmt_create timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  CONSTRAINT uk_tenant_info_kptenantid UNIQUE (kp, tenant_id)
);
CREATE INDEX IF NOT EXISTS idx_tenant_info_tenant ON tenant_info (tenant_id);

-- ------------------------------------------------------------------
-- Auth (Nacos-compatible)
-- ------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS users (
  username varchar(50) NOT NULL PRIMARY KEY,
  password varchar(500) NOT NULL,
  enabled smallint NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS roles (
  username varchar(50) NOT NULL,
  role varchar(50) NOT NULL,
  CONSTRAINT roles_pkey PRIMARY KEY (username, role)
);

CREATE TABLE IF NOT EXISTS permissions (
  role varchar(50) NOT NULL,
  resource varchar(255) NOT NULL,
  action varchar(15) NOT NULL,
  CONSTRAINT permissions_pkey PRIMARY KEY (role, resource, action)
);

-- ------------------------------------------------------------------
-- Naming / service registry (goacos-specific, PostgreSQL-backed)
-- ------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS naming_service (
  id bigserial PRIMARY KEY,
  namespace_id varchar(128) NOT NULL DEFAULT '',
  group_name varchar(128) NOT NULL DEFAULT 'DEFAULT_GROUP',
  name varchar(255) NOT NULL,
  protect_threshold double precision NOT NULL DEFAULT 0,
  metadata text,
  app_name varchar(128) DEFAULT NULL,
  gmt_create timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  CONSTRAINT uk_service UNIQUE (namespace_id, group_name, name)
);

CREATE TABLE IF NOT EXISTS naming_instance (
  id bigserial PRIMARY KEY,
  namespace_id varchar(128) NOT NULL DEFAULT '',
  group_name varchar(128) NOT NULL DEFAULT 'DEFAULT_GROUP',
  service_name varchar(255) NOT NULL,
  cluster_name varchar(64) NOT NULL DEFAULT 'DEFAULT',
  ip varchar(64) NOT NULL,
  port integer NOT NULL,
  weight double precision NOT NULL DEFAULT 1,
  healthy smallint NOT NULL DEFAULT 1,
  enabled smallint NOT NULL DEFAULT 1,
  ephemeral smallint NOT NULL DEFAULT 1,
  metadata text,
  last_heartbeat_ms bigint NOT NULL DEFAULT 0,
  gmt_create timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  CONSTRAINT uk_instance UNIQUE (namespace_id, group_name, service_name, cluster_name, ip, port)
);
CREATE INDEX IF NOT EXISTS idx_instance_service ON naming_instance (namespace_id, group_name, service_name);

-- ------------------------------------------------------------------
-- goacos metadata
-- ------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS goacos_meta (
  k varchar(64) NOT NULL PRIMARY KEY,
  v varchar(255) NOT NULL
);

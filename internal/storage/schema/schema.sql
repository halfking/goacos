-- goacos — lightweight Nacos-compatible server
-- Embedded MySQL schema. Config tables mirror the official Apache Nacos
-- distribution schema (Apache-2.0) so data can interoperate / migrate with a
-- real Nacos deployment. Naming tables are goacos-specific (goacos stores
-- service registry data in MySQL instead of Nacos's in-memory Distro+Derby
-- model, which makes horizontal scaling trivial).

SET NAMES utf8mb4;
SET FOREIGN_KEY_CHECKS = 0;

-- ------------------------------------------------------------------
-- Config center (Nacos-compatible)
-- ------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS config_info (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  data_id varchar(255) NOT NULL,
  group_id varchar(128) NOT NULL,
  content longtext NOT NULL,
  md5 varchar(32) DEFAULT NULL,
  gmt_create datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
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
  PRIMARY KEY (id),
  UNIQUE KEY uk_configinfo_datagrouptenant (data_id, group_id, tenant_id),
  KEY idx_group_id (group_id),
  KEY idx_gmt_create (gmt_create)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE IF NOT EXISTS his_config_info (
  id bigint unsigned NOT NULL,
  nid bigint unsigned NOT NULL AUTO_INCREMENT,
  data_id varchar(255) NOT NULL,
  group_id varchar(128) NOT NULL,
  app_name varchar(128) DEFAULT NULL,
  content longtext NOT NULL,
  md5 varchar(32) DEFAULT NULL,
  gmt_create datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  src_user text,
  src_ip varchar(50) DEFAULT NULL,
  op_type char(10) NOT NULL,
  tenant_id varchar(128) NOT NULL DEFAULT '',
  encrypted_data_key varchar(1024) NOT NULL DEFAULT '',
  PRIMARY KEY (nid),
  KEY idx_gmt_create (gmt_create),
  KEY idx_gmt_modified (gmt_modified),
  KEY idx_did (data_id, group_id, tenant_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE IF NOT EXISTS config_info_beta (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  data_id varchar(255) NOT NULL,
  group_id varchar(128) NOT NULL,
  app_name varchar(128) DEFAULT NULL,
  content longtext NOT NULL,
  beta_ips varchar(1024) DEFAULT NULL,
  md5 varchar(32) DEFAULT NULL,
  gmt_create datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  src_user text,
  src_ip varchar(50) DEFAULT NULL,
  tenant_id varchar(128) NOT NULL DEFAULT '',
  encrypted_data_key varchar(1024) NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_configinfobeta_datagrouptenant (data_id, group_id, tenant_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE IF NOT EXISTS config_info_tag (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  data_id varchar(255) NOT NULL,
  group_id varchar(128) NOT NULL,
  tenant_id varchar(128) NOT NULL DEFAULT '',
  tag_id varchar(128) NOT NULL,
  app_name varchar(128) DEFAULT NULL,
  content longtext NOT NULL,
  md5 varchar(32) DEFAULT NULL,
  gmt_create datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  src_user text,
  src_ip varchar(50) DEFAULT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_configinfotag_datagrouptenanttag (data_id, group_id, tenant_id, tag_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE IF NOT EXISTS config_tags_relation (
  id bigint unsigned NOT NULL,
  tag_name varchar(128) NOT NULL,
  tag_type varchar(64) DEFAULT NULL,
  data_id varchar(255) NOT NULL,
  group_id varchar(128) NOT NULL,
  tenant_id varchar(128) NOT NULL DEFAULT '',
  nid bigint unsigned NOT NULL AUTO_INCREMENT,
  PRIMARY KEY (nid),
  UNIQUE KEY uk_configtagrelation_configidtag (id, tag_name, tag_type),
  KEY idx_tenant_id (tenant_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE IF NOT EXISTS config_info_aggr (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  data_id varchar(255) NOT NULL,
  group_id varchar(128) NOT NULL,
  datum_id varchar(255) NOT NULL,
  content longtext NOT NULL,
  md5 varchar(32) DEFAULT NULL,
  gmt_create datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  src_user text,
  src_ip varchar(50) DEFAULT NULL,
  app_name varchar(128) DEFAULT NULL,
  tenant_id varchar(128) NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_configinfoaggr_datagrouptenantdatum (data_id, group_id, tenant_id, datum_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE IF NOT EXISTS group_capacity (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  group_id varchar(128) NOT NULL DEFAULT '',
  quota int unsigned NOT NULL DEFAULT 0,
  `usage` int unsigned NOT NULL DEFAULT 0,
  max_size int unsigned NOT NULL DEFAULT 0,
  max_aggr_count int unsigned NOT NULL DEFAULT 0,
  max_aggr_size int unsigned NOT NULL DEFAULT 0,
  max_history_count int unsigned NOT NULL DEFAULT 0,
  gmt_create datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_group_id (group_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE IF NOT EXISTS tenant_capacity (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  tenant_id varchar(128) NOT NULL DEFAULT '',
  quota int unsigned NOT NULL DEFAULT 0,
  `usage` int unsigned NOT NULL DEFAULT 0,
  max_size int unsigned NOT NULL DEFAULT 0,
  max_aggr_count int unsigned NOT NULL DEFAULT 0,
  max_aggr_size int unsigned NOT NULL DEFAULT 0,
  max_history_count int unsigned NOT NULL DEFAULT 0,
  gmt_create datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_tenant_id (tenant_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE IF NOT EXISTS tenant_info (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  kp varchar(128) NOT NULL,
  tenant_id varchar(128) NOT NULL DEFAULT '',
  namespace_name varchar(128) DEFAULT NULL,
  namespace_desc varchar(255) DEFAULT NULL,
  gmt_create datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_tenant_info_kptenantid (kp, tenant_id),
  KEY idx_tenant_id_tenant (tenant_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- ------------------------------------------------------------------
-- Auth (Nacos-compatible)
-- ------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS users (
  username varchar(50) NOT NULL,
  password varchar(500) NOT NULL,
  enabled tinyint(1) NOT NULL DEFAULT 1,
  PRIMARY KEY (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE IF NOT EXISTS roles (
  username varchar(50) NOT NULL,
  role varchar(50) NOT NULL,
  PRIMARY KEY (username, role)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE IF NOT EXISTS permissions (
  role varchar(50) NOT NULL,
  resource varchar(255) NOT NULL,
  action varchar(15) NOT NULL,
  PRIMARY KEY (role, resource, action)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- ------------------------------------------------------------------
-- Naming / service registry (goacos-specific, MySQL-backed)
-- ------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS naming_service (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  namespace_id varchar(128) NOT NULL DEFAULT '',
  group_name varchar(128) NOT NULL DEFAULT 'DEFAULT_GROUP',
  name varchar(255) NOT NULL,
  protect_threshold double NOT NULL DEFAULT 0,
  metadata text,
  app_name varchar(128) DEFAULT NULL,
  gmt_create datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_service (namespace_id, group_name, name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE IF NOT EXISTS naming_instance (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  namespace_id varchar(128) NOT NULL DEFAULT '',
  group_name varchar(128) NOT NULL DEFAULT 'DEFAULT_GROUP',
  service_name varchar(255) NOT NULL,
  cluster_name varchar(64) NOT NULL DEFAULT 'DEFAULT',
  ip varchar(64) NOT NULL,
  port int unsigned NOT NULL,
  weight double NOT NULL DEFAULT 1,
  healthy tinyint(1) NOT NULL DEFAULT 1,
  enabled tinyint(1) NOT NULL DEFAULT 1,
  ephemeral tinyint(1) NOT NULL DEFAULT 1,
  metadata text,
  last_heartbeat_ms bigint NOT NULL DEFAULT 0,
  gmt_create datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  gmt_modified datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_instance (namespace_id, group_name, service_name, cluster_name, ip, port),
  KEY idx_service (namespace_id, group_name, service_name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- ------------------------------------------------------------------
-- goacos metadata
-- ------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS goacos_meta (
  k varchar(64) NOT NULL,
  v varchar(255) NOT NULL,
  PRIMARY KEY (k)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

SET FOREIGN_KEY_CHECKS = 1;

/**
 * goacos-registry 的类型声明。
 *
 * 刻意保持最小：只描述「服务接入方会用到的」表面。
 * 契约的权威描述在 ../CONTRACT.md 与 Go SDK，这里不重复——
 * 两处描述一旦分叉，类型声明就会变成新的、没人校验的真相来源。
 */

export interface RegistryMetadata {
  [key: string]: string;
}

export interface ConfigOptions {
  service_name: string;
  port: number;
  enabled?: boolean;
  server_addr?: string;
  namespace_id?: string;
  group_name?: string;
  cluster_name?: string;
  ip?: string;
  weight?: number;
  metadata?: RegistryMetadata;
  username?: string;
  password?: string;
  token?: string;
  heartbeat_interval?: number;
  request_timeout?: number;
}

export interface Instance {
  ip?: string;
  port?: number;
  healthy?: boolean;
  enabled?: boolean;
  weight?: number;
  clusterName?: string;
  serviceName?: string;
  metadata?: RegistryMetadata;
  [key: string]: unknown;
}

export declare class RegistryError extends Error {}
export declare class DisabledError extends RegistryError {}
export declare class AuthError extends RegistryError {}
export declare class StatusError extends RegistryError {
  status: number;
  constructor(method: string, path: string, status: number, body?: string);
}

export declare class Config {
  constructor(options: ConfigOptions);
}

export declare class RegistryClient {
  constructor(config: Config);
  readonly enabled: boolean;
  readonly registered: boolean;
  readonly heartbeatInterval: number;
  readonly baseUrl: string;
  start(): void;
  stop(timeout?: number): void;
  register(): void;
  beat(): void;
  deregister(): void;
  listInstances(serviceName: string, healthyOnly?: boolean): Promise<Instance[]>;
  listServices(pageNo?: number, pageSize?: number): Promise<string[]>;
  ping(): Promise<void>;
  advertiseIp(): string;
}
# goacos-registry（Node）

goacos 服务注册中心的 Node/TypeScript 客户端，零依赖，使用全局 `fetch`。

Go SDK 是 [`sdk/registry`](../registry)，本绑定与它实现**同一套线上契约**；
三语言共用的不变量见 [`../conformance/CONTRACT.md`](../conformance/CONTRACT.md)，
一致性由 `../conformance/run.py` 的契约检查守住。

## 安装

```sh
npm install goacos-registry
```

## 用法

```js
const { Config, RegistryClient } = require('goacos-registry');

const client = new RegistryClient(new Config({
  service_name: 'redclaw-web',
  port: 27094,
  enabled: true,
  server_addr: process.env.GOACOS_SERVER_ADDR,
  ip: '100.106.175.168',
  metadata: { app: 'redclaw', component: 'web', env: 'itestu' },
}));
client.start();
// ...
client.stop();   // 注销并退出
```

## 为什么零依赖

用 Node 内建的 `fetch`，不需要 `node-fetch` 之类的垫片。
平台的镜像依赖经常被锁死，一个需要拉第三方包的注册客户端
会让接入方**直接无法部署**。

## 要求

Node >= 18（需要全局 `fetch`）。

许可证：Apache-2.0（与 goacos 主仓一致）。
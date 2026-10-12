# goacos-registry（Python）

goacos 服务注册中心的 Python 客户端，零依赖（只用标准库）。

Go SDK 是 [`sdk/registry`](../registry)，本绑定与它实现**同一套线上契约**；
三语言共用的不变量见 [`../CONTRACT.md`](../CONTRACT.md)，
一致性由 `../conformance/run.py` 的契约检查守住。

## 安装

```sh
pip install goacos-registry
```

> **构建环境要求：pip ≥ 23 / setuptools ≥ 77。**
>
> 实测记录：在 **pip 21.2 + setuptools 58** 的环境里安装本包，
> 构建器会**静默忽略 `[project]` 表**，产出名为 `UNKNOWN-0.0.0` 的空壳包——
> 包名、版本、许可全部丢失，而且**不报错**。装完 `import` 才可能察觉。
>
> 这是旧 setuptools 的行为，不是本包的问题；`build-system.requires`
> 已声明 `setuptools>=77`（`license` / `license-files` 属 PEP 639，需 77+），
> 但过旧的 pip 可能不去满足它。先升级 pip 再装。
>
> 用现代工具链实测：`goacos-registry 1.2.0`、许可 `Apache-2.0`、LICENSE 已随包分发。

## 用法

```python
import os
from goacos_registry import Config, RegistryClient

client = RegistryClient(Config(
    service_name="casdoor",
    port=28000,
    enabled=True,
    server_addr=os.environ["GOACOS_SERVER_ADDR"],
    ip="100.106.175.168",
    metadata={"app": "redclaw", "component": "auth", "env": "itestu"},
))
client.start()          # 注册 + 心跳（后台线程）
...
client.stop()           # 注销并退出
```

## 为什么零依赖

平台的镜像依赖经常被锁死。一个需要拉第三方包的注册客户端
会让接入方**直接无法部署**——而注册是「服务上线前」就要做的事，
不能因为客户端依赖冲突而卡住。

## 行为约定

- `start()` 不会因为注册中心不可达而抛异常，也不会阻塞启动。
- 心跳失败由 SDK 自己退避重试；退避上限不超过注册中心的实例淘汰窗口，
  否则实例会在重试之前就被删除、永不回来。
- `stop()` 会注销实例；`SIGKILL` 场景下由注册中心兜底淘汰。

许可证：Apache-2.0（与 goacos 主仓一致）。
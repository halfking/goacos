feat(gateway): 把「数据面拨不通的实例」变成状态接口里看得见的计数

起因
----
端到端实测（2026-10-14，本机 goacos + 数据面）时撞见的：三个实例都显示
`healthy`，但其中两个从网关所在的网络命名空间**根本拨不通**，于是 100% 的
流量静默集中到第三个 —— 而状态接口里的一切都看起来完全正常。

```
acc.itestu.cn  healthy=3  instances=[127.0.0.1:14101, 127.0.0.1:14102, 192.168.31.19:4101]
counters: {"requests":6,"proxied":6,"noRoute":0,"pooledUpstreams":3}
```

`healthy` 来自注册中心：实例的注册器在心跳就算 healthy。这与「本进程能不能
拨通它」是两件事，而它们最容易出问题的场合恰好是同一个 —— 注册器跑在容器里，
通告了一个只有它自己那个网络命名空间能到达的地址。

后果：注册器永远健康、实例永远 healthy、请求照样成功（靠重试绕过去）、
`proxied` 一直在涨。**没有任何一个数字说明「你部署的三个实例里有两个用不了」。**
唯一的痕迹是一行日志：
`[gateway] upstream 127.0.0.1:14101 failed, trying another instance: connection refused`

改了什么
--------
1. `Gateway.upstreamFailed` 计数 + 在重试路径上自增（`proxy.go`）。
   这是「实例不可达」，不是「请求没发出去」—— 后者已有 `noRoute`。
2. `Counters()` 暴露为 `counters.upstreamFailed`。
3. 状态接口里 `domains[].healthy` 旁边加 `healthyIsFrom`，写明它来自
   注册中心心跳、不是数据面探测，并指明要和 `counters.upstreamFailed` 对看。

为什么不直接把 healthy 改成探针结果
----------------------------------
那要新增一条主动健康探测链路，代价和风险都大得多（探测频率、探测超时、
探测失败与真实拨不通的区分）。先让不一致**看得见**，比让一个字段语义含糊
要好 —— 现在两个数字对不上时，操作者能立刻发现「注册中心和数据面不是一回事」。

实测确认
--------
同场景、修复后的网关：

```
counters: {"requests":6,"proxied":6,"noRoute":0,"pooledUpstreams":3,"upstreamFailed":12}
acc.itestu.cn  healthy=3
  instances: ['127.0.0.1:14101','127.0.0.1:14102','192.168.31.19:4101']
  healthyIsFrom: registry (registrar heartbeat), not a data-plane probe
```

`upstreamFailed=12` = 6 次请求 × 每次先试两个不可达实例。修复前这里只有
`healthy=3 / proxied=6`，与「负载均衡一切正常」无法区分。

判据
----
`TestUpstreamFailureIsCountedSeparatelyFromSuccess`。

它一开始写成「发一次请求断言 `upstreamFailed>=1`」，**结果时好时坏** ——
原因是 `Lookup` 会按 权重→IP→端口 排序（`selectInstances`），而测试里两个
实例都在 `127.0.0.1` 上，tiebreak 落到了操作系统随机分配的端口上，于是
「死实例是否排在前面」是五五开。改成连发六次请求覆盖轮转，两种排序下都必然
碰到死实例。

变异核对：摘掉 `g.upstreamFailed.Add(1)` 后，正是这条测试报红；
还原后通过。
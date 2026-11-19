# 架构决策记录（ADR）

ADR 用来记录“为什么这样设计”，避免以后只看到代码不知道背景。

## 状态

- Proposed：提议中
- Accepted：已采纳
- Superseded：被新 ADR 取代
- Deprecated：不再推荐

## 命名

`NNNN-短标题.md`，例如 `0002-random-short-codes.md`。编号递增，不重复使用。

## 索引

| 编号 | 标题 | 状态 |
| --- | --- | --- |
| [0001](0001-api-versioning-and-route-namespace.md) | API 版本和路由命名空间 | Accepted |
| [0002](0002-random-short-codes.md) | 随机短码 | Accepted |
| [0003](0003-postgres-and-pool-config.md) | PostgreSQL 和连接池配置 | Accepted |
| [0004](0004-storage-package-layout.md) | 存储包的目录结构 | Accepted |
| [0005](0005-prometheus-before-tracing.md) | 先做 Prometheus 指标，追踪以后再说 | Accepted |
| [0006](0006-redis-cache-design.md) | Redis 读缓存的设计 | Accepted |
| [0007](0007-async-click-recording.md) | 异步批量记录点击 | Accepted |
| [0008](0008-cold-cache-read-path.md) | 冷缓存和长尾短码的读路径保护 | Accepted |
| [0009](0009-process-split-and-credentials.md) | 两个进程、一种租户身份 | Accepted |
| [0010](0010-app-level-referential-integrity.md) | 引用完整性放在应用层，不使用数据库外键 | Accepted |
| [0011](0011-link-crud-api.md) | 短链管理 API 的增删改查和排序 | Accepted |
| [0012](0012-click-analytics.md) | 点击统计：日聚合、客户端维度和统计接口 | Accepted |
| [0013](0013-pprof-on-loopback-listener.md) | 剖析端点放在单独的回环监听上 | Accepted |
| [0014](0014-per-tenant-rate-limit-and-quota.md) | 按租户限流和链接配额 | Accepted |
| [0015](0015-monthly-link-quota.md) | 链接配额按月计创建数，删除不返还 | Accepted |
| [0016](0016-link-states-and-takedown.md) | 链接状态和下架：过期、停用、封禁 | Accepted |
| [0017](0017-defer-click-write-optimization.md) | 点击写入暂不改，保留单写入者 | Accepted |

## 模板

见 [template.md](template.md)。

参考：https://adr.github.io/

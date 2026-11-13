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

## 模板

见 [template.md](template.md)。

参考：https://adr.github.io/

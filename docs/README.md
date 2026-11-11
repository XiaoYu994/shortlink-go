# 文档

README 只保留项目概览和入口，架构、接口、运行方式和演进记录放在这里。

## 目录

| 文档 | 内容 |
| --- | --- |
| [architecture.md](architecture.md) | 当前架构、目录结构、依赖方向、请求流程 |
| [api.md](api.md) | HTTP API 契约、请求/响应、错误格式 |
| [runbook.md](runbook.md) | 本地启动、创建短链、数据库操作、排错 |
| [roadmap.md](roadmap.md) | 学习路线和阶段进度 |
| [adr/README.md](adr/README.md) | 架构决策记录索引 |

## 约定

- 影响架构、存储、协议的决定先写 ADR，再改代码。
- 路线图变化更新 `roadmap.md`，不在 README 里堆细节。
- 运行步骤变化更新 `runbook.md`，保证新人能按文档跑起来。

## 参考

这些结构参考了常见开源项目的做法：

- ADR：https://adr.github.io/
- Kubernetes KEP：https://github.com/kubernetes/enhancements
- Rust RFC：https://github.com/rust-lang/rfcs
- Go design docs：https://go.googlesource.com/proposal/+/master/design
- Prometheus docs：https://github.com/prometheus/prometheus/tree/main/docs

项目还小，不需要一上来就做 KEP/RFC 流程；用 ADR + roadmap + runbook 足够，等协作规模变大再演进。

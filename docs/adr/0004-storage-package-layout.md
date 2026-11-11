# 0004. 存储包的目录结构

- 状态：Accepted
- 日期：2026-11-11

## 背景

最初 `MemoryStore` 放在 `internal/link`，PostgreSQL 实现在 `internal/postgres`。`link` 同时承担领域模型和存储实现，边界不清晰。

## 决策

- `internal/link` 只保留领域模型、校验和业务错误。
- `internal/memorystore` 放内存实现，只用于测试和本地开发。
- `internal/postgres` 放生产存储实现。
- `LinkStore` 接口定义在消费方 `internal/httpapi`。
- `main` 只负责组装，不引用内存实现。

## 后果

- 领域包不依赖存储和 HTTP。
- 内存实现不会进入 server 二进制。
- 新增存储实现（例如缓存层）时，有清晰的落点。
- 测试仍然可以用内存实现，不需要数据库。

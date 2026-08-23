# Agent-FS Community 文档

这套文档描述当前 `main` 分支实际实现的社区版。Agent-FS Community 是运行在开发机
loopback 网络上的单用户上下文服务：它把本地文件系统转换为可增量更新的 SQLite 语义索引，
再通过 CLI、HTTP 和 MCP 为 Agent 提供检索结果与有 Token 预算的上下文包。

## 阅读路线

### 使用者

1. [快速开始与功能指南](user-guide.md)：安装、索引目录、查询和日常使用。
2. [MCP 与 HTTP API](mcp-http-api.md)：Claude Code、Codex、OpenCC 和直接 API 调用。
3. [配置与部署](configuration-deployment.md)：命令行参数、环境变量、systemd 和多目录配置。
4. [运维与故障排查](operations-troubleshooting.md)：健康检查、日志、修复索引和常见错误。

### 设计者与贡献者

1. [总体架构设计](architecture.md)：组件、数据流、并发模型和设计取舍。
2. [特性设计](features.md)：监听、解析、Embedding、混合检索、上下文包和恢复语义。
3. [代码模块设计](modules.md)：源码文件职责、调用关系和扩展点。
4. [数据模型与崩溃恢复](data-model-recovery.md)：SQLite 表、事务边界和操作日志状态机。
5. [开发与测试](development-testing.md)：构建、测试、代码约束和贡献流程。
6. [性能与基准方法](performance-benchmarking.md)：应测指标、百万文件方法和结果声明规则。
7. [安全模型与社区版边界](security.md)：信任边界、威胁模型和安全部署要求。

## 能力状态

| 能力 | 当前状态 | 说明 |
|---|---|---|
| 完整扫描 | 已实现 | 先收集、解析和向量化，再以单个 SQLite 事务提交 |
| 文件监听与增量索引 | 已实现 | fsnotify、事件去抖、批量更新、失败重试 |
| 工程目录剪枝 | 已实现 | 扫描与 watcher 共用默认 basename globs，并支持 CLI 扩展/关闭 |
| 源码文件白名单 | 已实现 | 默认仅代码、Markdown、文本配置/锁文件；媒体、压缩包和二进制不入库 |
| BM25 + 向量 + 元数据混合检索 | 已实现 | 文件与 Chunk 两级召回，固定权重融合 |
| MCP/HTTP 服务 | 已实现 | loopback HTTP、两个 MCP tools、两个 REST API |
| Go AST Chunk | 已实现 | declaration/symbol/行号级 Chunk |
| PDF/Office parser | 已实现、默认不启用 | 用 `--include-file` 显式加入；PDF 依赖 `pdftotext` |
| 崩溃恢复操作日志 | 已实现 | `rename`、`remove` 持久状态机与启动恢复 |
| Claude Code/Codex/OpenCC | 已提供配置并有兼容记录 | 见 [兼容性记录](../COMPATIBILITY.md) |
| 认证、ACL、租户、审计 | 不包含 | 社区版仅面向受信任的本机单用户 |
| 百万文件 SLO | 尚未在本仓库发布可复现结果 | 目标与实测结果必须分开陈述 |
| Agent 工具调用/Token 降幅 | 尚未在本仓库发布 A/B 证据 | 见[性能与基准方法](performance-benchmarking.md) |

## 文档约定

- 所有路径示例都使用绝对路径，因为索引中的 `files.path` 是绝对路径。
- 默认服务地址是 `http://127.0.0.1:7337`，MCP endpoint 是 `/mcp`。
- “文件系统”始终是事实源；SQLite 是可删除、可重建的派生索引。
- 文档中标记为“当前实现”的内容应能在源码或测试中找到对应证据。
- 文档中标记为“建议”“目标”或“待验证”的内容不是发布承诺。

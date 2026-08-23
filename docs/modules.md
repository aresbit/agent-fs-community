# 代码模块设计

## 1. 仓库布局

```text
agent-fs-community/
├── cmd/agent-fs/main.go       # 可执行程序入口、signal context
├── internal/cli/cli.go        # CLI 参数、子命令、daemon 编排
├── store.go                   # Store 生命周期、SQLite 初始化
├── schema.sql                 # 社区版 schema v1、FTS 与触发器
├── scan.go                    # 完整扫描与事务提交
├── incremental.go             # watcher 事件的批量增量同步
├── watch.go                   # fsnotify 递归监听、去抖和重试
├── exclude.go                 # 默认/自定义 basename glob 排除策略
├── include.go                 # 源码/Markdown 文件白名单与 opt-in 规则
├── parser.go                  # 文本、Go AST、PDF、Office 提取与 Chunk
├── embed.go                   # 本地 hash 和 HTTP Embedding
├── hybrid.go                  # 双层混合召回、过滤与融合排序
├── context.go                 # Token 预算上下文包
├── query.go                   # 只读 SQL 与快捷查询、doctor
├── operations.go              # tag、rename、remove
├── recovery.go                # 持久操作日志与启动恢复
├── server.go                  # HTTP、MCP、协议兼容和安全头
├── scripts/                   # 安装、MCP 冒烟、安装器测试
├── compat/                    # 三类客户端配置模板
└── *_test.go                  # 单元、集成和恢复测试
```

`legacy/python/` 是早期实现的保留代码，不参与 Go binary 的构建，也不应作为当前设计依据。

## 2. 包边界

项目当前只有两个主要 Go package：

- `agentfs`：可复用库，负责存储、索引、检索、server 和文件操作。
- `agentfs/internal/cli`：进程适配层，解析命令行并组合 `Store`、watcher 与 HTTP server。

`cmd/agent-fs` 保持很薄，只创建 signal-aware context 并调用 `cli.Run`。这让库逻辑可在测试和未来其他
前端中直接复用。

## 3. Store 核心

[`store.go`](../store.go) 中的 `Store` 是主要聚合根：

```go
type Store struct {
    db           *sql.DB
    path         string
    contentBytes int
    extractBytes int
    maxRows      int
    embedder     Embedder
}
```

`Open` 负责路径规范化、创建 0700 父目录、打开 SQLite、设置单连接、应用 schema、恢复未完成操作，
最后把数据库文件 chmod 为 0600。社区 schema 只接受版本 0/1，并主动拒绝权限感知版或旧 Python
数据库，防止悄悄误读不同语义的数据。

[`exclude.go`](../exclude.go) 在 `Open` 时合并并验证默认/自定义 basename globs。扫描、增量路径和
watcher 注册都调用同一策略，避免“没有索引但仍占 watch”或“没有 watch 但完整扫描又写回”的分裂。
[`include.go`](../include.go) 则负责非目录条目的 allowlist：常用扩展名使用 O(1) map 查询，特殊无扩展名
文件使用 exact-name map，只有少量 glob 走 `filepath.Match`，避免百万文件树上逐项线性匹配大清单。

扩展原则：新增索引行为优先成为 `Store` 方法；新增配置通过 `Options` 注入；不要从库层读取全局环境
变量，环境变量解析应留在 CLI。

## 4. 索引流水线

[`scan.go`](../scan.go) 将索引拆成 `collect → describePath → commitScan`：

- `collect` 管理目录遍历、取消、忽略数据库 artifacts 和稳定排序。
- `describePath` 管理文件类型、解析、预览、Chunk 与向量。
- `commitScan` 管理数据库事务、父子关系、tags、FTS 与向量持久化。

[`incremental.go`](../incremental.go) 复用同一 `describePath`，再用批量 upsert、批量 Chunk/Embedding 写入和
stale 删除完成增量提交。新增 parser 或 embedder 时，两条流水线会自然获得一致行为。

## 5. Watcher

[`watch.go`](../watch.go) 的 `WatchOptions` 提供：

| 字段 | 用途 |
|---|---|
| `Root` | 必需的绝对或可规范化 root |
| `Debounce` | 事件成熟等待，非正值时默认 100ms |
| `Ready` | 初始扫描成功后的非阻塞通知 |
| `Errors` | 可恢复 watcher 错误的非阻塞通道 |
| `OnSynced` | 测试/基准记录从观察到提交的 freshness |

daemon 当前为每个 root 独立创建 fsnotify watcher。若未来需要百万目录规模，应评估共享 watcher、
分层轮询或 Linux fanotify，而不是只调高参数。

## 6. Parser 与 Chunk

[`parser.go`](../parser.go) 中 `extractDocument` 是统一入口，返回内部 `parsedDocument`。格式专用提取器
只负责得到有界文本；Chunk 策略随后统一执行。Go declaration 先按 AST 切分，过大的 declaration 再
按窗口切分。其余文本直接使用 1,200 rune / 200 rune overlap。

新增语言 AST 的推荐步骤：

1. 在 `languageForExtension` 登记语言。
2. 增加无副作用的 `xxxASTChunks(filename, source)`。
3. 在 `extractDocument` 中按扩展名调用，解析失败返回 `nil` 以触发窗口回退。
4. 为 symbol、行号、超大 declaration、语法错误回退增加测试。

不要让 parser 直接写数据库；这会破坏完整扫描的“先收集、后事务提交”边界。

## 7. Embedder

[`embed.go`](../embed.go) 的最小接口只有 `Model`、`Dimensions` 和 `Embed`。可选内部接口
`EmbedBatch` 会被索引流水线探测，并自动按 64 条分批。

新的实现必须：线程安全、固定维度、尊重 context 取消、返回稳定模型 ID，并对内容是否离开本机有
明确文档。数据库以模型 ID 隔离向量候选。

## 8. Search 与 Context

[`hybrid.go`](../hybrid.go) 把文件 ID 作为同一文件不同召回通道的聚合 key。Chunk 命中会把最相关
Chunk 的 symbol、范围和内容挂到文件级 `SearchHit` 上，因此一个文件在最终结果中最多出现一次。

[`context.go`](../context.go) 是检索结果的消费者，不复制召回逻辑。它优先读取命中 Chunk；没有 Chunk
时使用文件预览。未来精确 tokenizer、邻接 Chunk 扩展或去重策略应放在这一层。

## 9. Server

[`server.go`](../server.go) 使用标准库 `http.ServeMux`：

- `Handler` 注册 REST/MCP route 与安全中间件。
- `mcp` 负责 JSON-RPC、协议协商和 stateless headers 校验。
- `mcpTools` 是工具 schema 的单一来源。
- `callTool` 将 MCP 参数映射到库方法。

新增 MCP tool 时必须同步添加 schema、调用分发、正常/错误测试和文档。社区版 MCP 应保持只读；会
改文件的 tool 会显著扩大误操作和安全风险。

## 10. Operations 与 Recovery

[`operations.go`](../operations.go) 负责正常路径状态转换，
[`recovery.go`](../recovery.go) 根据外部可观察状态恢复。两者共享内部的 `renameRows`、`deleteRows` 等
幂等数据库步骤。修改状态机时必须覆盖每个 crash point，而不只是成功路径。

## 11. 依赖策略

运行时核心依赖只有 `fsnotify` 与 pure-Go `modernc.org/sqlite`。PDF 提取是可选外部命令。HTTP、MCP、
AST、XML、ZIP 与 CLI 尽量使用 Go 标准库。新增依赖前应说明它替代了什么复杂度、是否引入 CGO、
是否影响单 binary 部署和许可。

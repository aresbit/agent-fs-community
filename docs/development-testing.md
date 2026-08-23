# 开发与测试

## 1. 开发环境

```bash
go version
go mod download
make check
```

要求 Go 1.26+。核心 SQLite driver 是 pure Go，正常构建不要求系统 SQLite 或 CGO。PDF 功能测试/手工
验证需要 `pdftotext`，Office 与 Go AST 不需要额外 runtime。

## 2. 常用命令

| 命令 | 内容 |
|---|---|
| `make build` | `bin/agent-fs`，使用 `-trimpath` |
| `make test` | 全包普通测试 |
| `make race` | 全包 race detector |
| `make check` | gofmt、shell syntax、vet、build、race test |
| `make clean` | 清 Go test cache |

单测定位：

```bash
go test -run TestHybridSearchAndContextPack -v ./...
go test -run TestOpenRecoversInterruptedRemove -v ./...
go test -race -count=10 ./...
```

## 3. 测试分层

| 文件 | 重点 |
|---|---|
| `store_test.go` | 扫描、只读查询、snapshot rollback、并发读、schema 边界 |
| `incremental_test.go` | 批量 create/update/delete、Chunk/Embedding 替换 |
| `parser_test.go` | Go AST、DOCX、HTTP batch embedder |
| `server_test.go` | 混合检索、Context Pack、MCP 协议、真实 watcher 增删 |
| `operations_test.go` | rename/remove、危险路径、crash recovery |
| `exclude_test.go` | 默认/自定义排除、关闭默认值、watch 剪枝、旧索引清理 |
| `scripts/test-install.sh` | 隔离 HOME 下的一键安装与三客户端配置调用 |
| `scripts/smoke-mcp.sh` | 运行中 daemon initialize 冒烟 |

测试应使用 `t.TempDir()` 和独立数据库，避免读写开发者真实 workspace。涉及 crash recovery 的测试可
构造 journal 和文件系统中间态，但必须验证恢复后的文件与索引两侧。

## 4. 代码约束

- 所有路径进入存储层前经过 `normalizePath`，数据库保存绝对 clean path。
- 文件系统是事实源；不要在 SQLite 中引入无法从文件或明确用户元数据重建的隐式状态。
- 全量扫描失败必须保留旧 snapshot。
- FTS external-content 表必须通过同事务 trigger 与源表保持一致。
- `query` 的只读检查和 `maxRows` 不能绕过。
- daemon 必须保持 loopback-only；社区版不新增“临时”远程开关。
- MCP tools 默认只读；修改真实文件的能力留在显式 CLI/API。
- Parser 和 embedder 必须尊重 context 取消与内容边界。
- 扫描、增量同步和 watcher 必须共用 `exclude.go`，不能各自维护排除清单。
- 新增错误应保留操作上下文并使用 `%w` 包装底层原因。

## 5. 修改 schema

当前 schema v1 直接嵌入 binary。修改流程：

1. 明确是向后兼容的 `CREATE IF NOT EXISTS`，还是需要新 `user_version`。
2. 为旧版本打开、升级/拒绝路径写测试。
3. 更新 `schema.sql`、`Store.initialize` 版本判断和本文档。
4. 验证 FTS trigger、foreign keys 与 `doctor`。
5. 不要通过只改 `PRAGMA user_version` 假装迁移完成。

社区版目前没有通用 migration runner。破坏性 schema 变化更适合新 DB + rescan，直到真正需要保留不可
重建元数据时再设计迁移链。

## 6. 新增 MCP tool

1. 先在 `agentfs` package 提供可测试库方法。
2. 在 `mcpTools()` 添加准确 JSON Schema 和 annotations。
3. 在 `callTool` 解码参数并调用库方法。
4. 增加 tools/list、成功、未知字段、业务错误测试。
5. 更新 [MCP 与 HTTP API](mcp-http-api.md)和客户端提示。

不要让 tool schema 宣称代码未验证的范围；例如参数实际会 cap 时，文档和 description 都要写明。

## 7. 新增 parser

parser 输出应是有界、确定、无数据库副作用的 `parsedDocument/parsedChunk`。必须测试：

- 正常文本与 symbol/行号；
- 空文件；
- 语法损坏的回退；
- 超大输入的截断；
- 压缩/嵌套格式的资源上限；
- context 取消或外部进程超时。

如果采用外部 binary，要在错误中明确依赖名，并在文档中标为可选依赖。

## 8. Pull Request 验收

提交前至少：

```bash
make check
bash scripts/test-install.sh
```

对于 watcher、性能、恢复或协议变更，还应附针对场景的可复现命令和 before/after 数据。性能数据应
包含机器、文件树生成参数、冷/热状态、样本数和 percentile，不接受只给平均值。

## 9. 文档维护

新增 flag、环境变量、route、MCP tool、schema 表、parser 类型或安全边界时，必须更新 `docs/` 的相应
章节与 `docs/README.md` 能力状态。目标指标和当前实测结果要分开，避免把 roadmap 写成发布事实。

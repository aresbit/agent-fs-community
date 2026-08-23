# 快速开始与功能指南

## 1. 前置条件

- Linux 开发机；一键安装要求 systemd user service。
- Go 1.26 或更高版本。
- `curl` 用于安装后的健康检查。
- 索引 PDF 时额外安装 Poppler 的 `pdftotext`；不索引 PDF 可不安装。
- Claude Code、Codex、OpenCC 都是可选客户端，不影响 daemon 自身运行。

## 2. 一键安装

```bash
git clone https://github.com/aresbit/agent-fs-community.git
cd agent-fs-community
./scripts/install.sh /absolute/path/to/workspace
```

安装结果：

| 内容 | 默认位置 |
|---|---|
| binary | `~/.local/bin/agent-fs` |
| SQLite 索引 | `~/.local/share/agent-fs/community.db` |
| systemd unit | `~/.config/systemd/user/agent-fs.service` |
| MCP endpoint | `http://127.0.0.1:7337/mcp` |

安装器会尝试配置已安装的 Claude Code 与 Codex，并把 `agentfs` server 合并到
`~/.claude/settings.json` 供 OpenCC/Claude-compatible 工具发现。被修改的既有配置会先生成时间戳备份。

验证：

```bash
curl --fail http://127.0.0.1:7337/healthz
./scripts/smoke-mcp.sh
systemctl --user status agent-fs.service
```

## 3. 手动构建与运行

```bash
make build
bin/agent-fs --db "$HOME/.local/share/agent-fs/community.db" daemon \
  --root /absolute/path/to/code \
  --root /absolute/path/to/docs \
  --listen 127.0.0.1:7337
```

全局参数必须写在子命令前，例如：

```bash
bin/agent-fs --content-bytes 16384 --extract-bytes 4194304 scan /absolute/project
```

## 4. 推荐的 Agent 使用方式

在客户端项目规则或系统提示中加入：

```text
开始代码或文档任务前，先调用 agentfs.context_pack。
把用户的完整任务作为 query，token_budget 使用 4000-6000。
优先使用返回的 symbol、行号和内容；只有证据不足时再调用 agentfs.search，
不要先递归遍历或逐个读取整个仓库。
```

适合 `context_pack` 的 query：

- `修复 watcher 在编辑器原子替换文件时丢失更新的问题`
- `解释 operation_journal 的崩溃恢复状态机`
- `NewHTTPEmbedder dimensions 不匹配时在哪里报错`
- 直接粘贴编译错误、日志片段、函数名或用户故事。

`query` 越完整，混合检索越能同时利用领域词、精确 symbol 和路径线索。

## 5. MCP 工具

### `context_pack`

输入：

```json
{
  "query": "修复 PDF 解析超时后的错误处理",
  "token_budget": 5000,
  "limit": 12
}
```

输出包含查询、排序后的 items、估算 Token 和是否截断。每个 item 可包含路径、类型、score、symbol、
起止行和内容。

### `search`

输入：

```json
{
  "query": "recovery journal",
  "limit": 20,
  "path_prefix": "/absolute/project",
  "kinds": ["file"],
  "tags": ["backend"],
  "min_size": 1,
  "max_size": 1048576
}
```

所有筛选项可选，但 `query` 和元数据条件至少要有一种。多个 tags 是 AND 语义。详细协议见
[MCP 与 HTTP API](mcp-http-api.md)。

## 6. CLI 功能

以下示例假定：

```bash
export AGENTFS_DB="$HOME/.local/share/agent-fs/community.db"
```

### 初始化与扫描

```bash
agent-fs init
agent-fs scan --tag backend --tag active /absolute/project
```

完整扫描是事务性的。重复扫描会刷新已存在行并移除该 root 下已消失的行。

### 浏览和搜索

```bash
agent-fs ls /absolute/project
agent-fs find 'operation journal'
agent-fs big 100
agent-fs du /absolute/project
agent-fs by-tag backend
```

`big 100` 表示大于 100 MiB 的普通文件。所有输出均为格式化 JSON。

### 只读 SQL

```bash
agent-fs query 'SELECT kind, count(*) AS n FROM files GROUP BY kind ORDER BY n DESC'
agent-fs query "SELECT path, mime FROM files WHERE mime LIKE 'text/%' LIMIT 20"
```

只允许一条只读语句，结果最多 `--max-rows` 行。不要依赖未记录的内部列长期兼容；schema 见
[数据模型](data-model-recovery.md)。

### 标签

```bash
agent-fs tag /absolute/project/README.md important
agent-fs untag /absolute/project/README.md important
agent-fs by-tag important
```

标签写入索引而不是文件扩展属性；删除并重新创建同一路径后标签是否保留取决于扫描/增量 reconciliation。

### 可恢复重命名与删除

```bash
agent-fs rename /absolute/project/old.go new.go
agent-fs rm /absolute/project/generated.txt
agent-fs rm --recursive /absolute/project/obsolete-dir
```

这些命令会修改真实文件。目录非空时必须显式 `--recursive`。不能操作文件系统根、索引数据库本身，
也不能把根目录级别目标交给这些命令。

### 诊断与修复

```bash
agent-fs doctor
agent-fs rebuild-fts
agent-fs scan /absolute/project
```

先用 `doctor` 确定故障类型。`rebuild-fts` 只重建文件级 FTS；路径大量漂移或数据库 schema 不兼容时，
使用新数据库并重新扫描通常更清晰。

## 7. 索引多个目录

手动 daemon 可重复传入 `--root`。一键安装器当前只接收一个项目 root；要长期监听多个 root，编辑
user unit 的 `ExecStart`，增加多个 `--root`，再执行：

```bash
systemctl --user daemon-reload
systemctl --user restart agent-fs.service
```

root 可以嵌套，但会产生重复 watcher 与重复事件处理，通常应只配置最外层或一组互不重叠的目录。

## 8. 卸载

当前没有自动卸载脚本。手动停止并移除：

```bash
systemctl --user disable --now agent-fs.service
rm "$HOME/.config/systemd/user/agent-fs.service"
systemctl --user daemon-reload
rm "$HOME/.local/bin/agent-fs"
```

确认不再需要索引后，可删除 `~/.local/share/agent-fs/community.db` 及其 WAL/SHM。客户端中的 `agentfs`
MCP 配置需要分别通过客户端命令或配置文件移除。删除索引不会删除被索引文件。

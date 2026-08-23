# MCP 与 HTTP API

## 1. Endpoint 一览

默认 base URL：`http://127.0.0.1:7337`。

| Method | Path | 用途 |
|---|---|---|
| `GET` | `/healthz` | 进程健康和请求计数 |
| `POST` | `/v1/search` | REST 混合检索 |
| `POST` | `/v1/context` | REST 上下文包 |
| `POST` | `/mcp` | MCP Streamable HTTP / stateless JSON-RPC |

请求 body 默认最多 1 MiB。REST body 与 JSON-RPC envelope 拒绝未知字段；MCP tool 的 arguments
当前按 Go `json.Unmarshal` 解码，未知 arguments 字段会被忽略。服务无认证，只允许在受信任的本机
loopback 边界使用。

## 2. MCP 客户端配置

### Claude Code

```bash
claude mcp add --transport http --scope user agentfs http://127.0.0.1:7337/mcp
claude mcp get agentfs
```

一次性配置可使用 [`compat/claude-agentfs.json`](../compat/claude-agentfs.json)。工具名通常显示为
`mcp__agentfs__context_pack` 和 `mcp__agentfs__search`。

### Codex

```bash
codex mcp add agentfs --url http://127.0.0.1:7337/mcp
codex mcp get agentfs
```

也可以把 [`compat/codex-agentfs.toml`](../compat/codex-agentfs.toml) 合并进用户配置。

### OpenCC

把 [`compat/opencc-agentfs.json`](../compat/opencc-agentfs.json) 中的 `mcpServers.agentfs` 合并到
`~/.claude/settings.json`，在 OpenCC 中运行 `mcpfs_discover regenerate=true`，再通过 `mcpfs` 或
`mcpfs_exec` 调用工具。

已实测客户端版本与协议见 [`COMPATIBILITY.md`](../COMPATIBILITY.md)。客户端版本可能变化，升级后应
重新执行 `tools/list` 与实际 tool call 冒烟测试。

## 3. MCP 生命周期

### initialize

```bash
curl -sS http://127.0.0.1:7337/mcp \
  -H 'Accept: application/json, text/event-stream' \
  -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"curl","version":"1"}}}'
```

server 接受 `2025-06-18` 与 `2025-03-26` 的 initialize，并返回 tools capability。当前实现返回 JSON，
不创建服务端 session，也不要求 `Mcp-Session-Id`。

### initialized notification

`notifications/initialized` 和 `notifications/cancelled` 返回 HTTP 202，没有 JSON-RPC response body。

### tools/list

```bash
curl -sS http://127.0.0.1:7337/mcp \
  -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'
```

结果包含 `context_pack`、`search`，以及本地 cache 的 `ttlMs=60000` 提示。两个工具都标注 read-only、
idempotent、closed-world。

## 4. MCP Tools

### 4.1 `context_pack`

| 参数 | 类型 | 必需 | 默认/范围 |
|---|---|---:|---|
| `query` | string | 是 | 非空完整任务 |
| `token_budget` | integer | 否 | 默认 4000，最大 32000；非正值回退默认 |
| `limit` | integer | 否 | 默认 12；HybridSearch 最终最多 200 |

调用：

```bash
curl -sS http://127.0.0.1:7337/mcp \
  -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"context_pack","arguments":{"query":"watcher 如何处理高频原子替换","token_budget":5000,"limit":12}}}'
```

`result.structuredContent` 示例结构：

```json
{
  "query": "watcher 如何处理高频原子替换",
  "items": [
    {
      "path": "/workspace/watch.go",
      "kind": "file",
      "score": 0.72,
      "symbol": "Store.Watch",
      "start_line": 26,
      "end_line": 122,
      "content": "..."
    }
  ],
  "estimated_tokens": 2450,
  "truncated": false
}
```

MCP 同时把相同 JSON 序列化到 `result.content[0].text`，兼容只读取 text content 的客户端。

### 4.2 `search`

| 参数 | 类型 | 必需 | 语义 |
|---|---|---:|---|
| `query` | string | MCP schema 当前标为是 | 自然语言、symbol 或错误文本 |
| `limit` | integer | 否 | 默认 20，最大 200 |
| `path_prefix` | string | 否 | 路径前缀过滤，建议绝对路径 |
| `kinds` | string[] | 否 | `file`、`dir`、`symlink`、`other` |
| `tags` | string[] | 否 | 多标签全部满足 |
| `min_size` | integer | 否 | 最小 bytes，0 表示不限制 |
| `max_size` | integer | 否 | 最大 bytes，0 表示不限制 |

```bash
curl -sS http://127.0.0.1:7337/mcp \
  -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"search","arguments":{"query":"HTTP embedding retry","path_prefix":"/workspace","kinds":["file"],"limit":10}}}'
```

返回数组中的 hit 含 path、kind、size、mtime、MIME、snippet、Chunk symbol/行号、总分及三个分项分数。

## 5. Stateless headers

轻量 bridge 可先调用 `server/discover` 获取当前扩展协议 `2026-07-28`。使用该协议时请求需满足：

- `MCP-Protocol-Version: 2026-07-28`
- `Mcp-Method` 必须与 JSON-RPC `method` 相同
- `tools/call` 还需要 `Mcp-Name` 与 `params.name` 相同

标准 Claude Code/Codex 接入不需要手工设置这些 headers。服务还接受 MCP header 版本
`2025-06-18`、`2025-03-26`。

## 6. REST API

### `POST /v1/search`

body 与 `search` arguments 相同：

```bash
curl --fail-with-body -sS http://127.0.0.1:7337/v1/search \
  -H 'Content-Type: application/json' \
  --data '{"query":"operation journal","limit":5}'
```

成功返回 `{"hits":[...]}`。

### `POST /v1/context`

```bash
curl --fail-with-body -sS http://127.0.0.1:7337/v1/context \
  -H 'Content-Type: application/json' \
  --data '{"query":"解释增量索引事务","token_budget":4000}'
```

成功直接返回 ContextPack。

### `GET /healthz`

```json
{
  "status": "ok",
  "protocol": "2026-07-28",
  "requests": 12,
  "errors": 1
}
```

`requests` 统计 search/context/mcp route 的请求；`errors` 统计业务 API 或 JSON-RPC 错误。JSON decode
失败目前不会增加 errors 计数。

## 7. 错误语义

- REST decode 或业务错误：HTTP 400，body 为 `{"error":"..."}`。
- MCP 协议错误：HTTP 200，JSON-RPC `error`；常见 code 为 `-32600`、`-32601`、`-32602`、
  `-32000`。
- 不允许的 Origin：HTTP 403。
- body 超限通常表现为 JSON decode 失败；客户端应控制 query 长度，不要发送文件正文。

MCP `tools/call` 没有部分成功语义。客户端可以记录 JSON-RPC id 做关联，但社区版不保存请求审计。

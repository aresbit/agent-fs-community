# MCP 快速接入

## 1. 启动

```bash
./scripts/install.sh /absolute/workspace
curl -sS http://127.0.0.1:7337/healthz
./scripts/smoke-mcp.sh
```

## 2. Claude Code

一键命令：

```bash
claude mcp add --transport http --scope user agentfs http://127.0.0.1:7337/mcp
claude mcp get agentfs
```

单次运行、完全不修改配置：

```bash
claude -p --mcp-config=compat/claude-agentfs.json \
  --allowedTools mcp__agentfs__context_pack,mcp__agentfs__search \
  '调用 context_pack 找出 watcher 初始化逻辑'
```

## 3. Codex

```bash
codex mcp add agentfs --url http://127.0.0.1:7337/mcp
codex mcp get agentfs
```

或把 [`compat/codex-agentfs.toml`](compat/codex-agentfs.toml) 合并到 Codex 配置。

## 4. OpenCC

安装器把下面的配置合并到 `~/.claude/settings.json`，OpenCC 的 MCP-FS discovery 会自动
发现传统 HTTP MCP server：

```json
{
  "mcpServers": {
    "agentfs": {
      "type": "http",
      "url": "http://127.0.0.1:7337/mcp"
    }
  }
}
```

进入 OpenCC 后运行 `mcpfs_discover regenerate=true`，随后可通过 `mcpfs` 或
`mcpfs_exec` 调用 `context_pack` / `search`。

## 5. 直接调用 MCP

标准 Streamable HTTP initialize：

```bash
curl -sS http://127.0.0.1:7337/mcp \
  -H 'Accept: application/json, text/event-stream' \
  -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"curl","version":"1"}}}'
```

调用上下文包：

```bash
curl -sS http://127.0.0.1:7337/mcp \
  -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"context_pack","arguments":{"query":"崩溃恢复如何实现","token_budget":4000}}}'
```

## 6. 安全边界

社区版没有认证、租户和 ACL。CLI 会拒绝非 loopback `--listen`，但用户仍需避免 SSH
反向转发、公开反向代理或容器端口映射。跨机器和多用户环境应使用具备认证、授权和审计的
企业部署。

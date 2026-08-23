# 客户端兼容性

实测日期：2026-08-23。

| 客户端 | 实测版本 | initialize | tools/list | context_pack |
|---|---:|---:|---:|---:|
| OpenCC | 1.0.2，MCP SDK ^1.29.0 | stateless bridge | 通过 | 通过 |
| Claude Code | 2.1.237 | 2025-06-18 | 通过 | 通过 |
| Codex CLI | 0.149.0-alpha.4.1 | 2025-06-18 | 通过 | 通过 |

Agent-FS 同时接受 2025-06-18、2025-03-26 Streamable HTTP，以及供 curl/轻量 bridge
使用的 2026-07-28 stateless headers。社区版 endpoint 固定为本机 HTTP，不提供远程认证。

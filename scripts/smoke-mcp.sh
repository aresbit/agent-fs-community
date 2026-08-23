#!/usr/bin/env bash
set -euo pipefail

endpoint=${1:-http://127.0.0.1:7337/mcp}
response=$(curl --fail --silent "$endpoint" \
  -H 'Accept: application/json, text/event-stream' \
  -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"agent-fs-smoke","version":"1"}}}')

if [[ "$response" != *'"name":"agent-fs"'* || "$response" != *'"tools"'* ]]; then
  echo "agent-fs MCP smoke failed: $response" >&2
  exit 1
fi
echo "agent-fs MCP smoke passed: $endpoint"

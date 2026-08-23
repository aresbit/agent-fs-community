#!/usr/bin/env bash
set -euo pipefail

project_root=${1:-$PWD}
listen_address=${AGENTFS_LISTEN:-127.0.0.1:7337}
endpoint="http://${listen_address}/mcp"

if [[ ! -d "$project_root" ]]; then
  echo "agent-fs: root is not a directory: $project_root" >&2
  exit 2
fi
project_root=$(cd "$project_root" && pwd -P)
if [[ "$project_root" == *$'\n'* || ! "$listen_address" =~ ^(127\.0\.0\.1|localhost|\[::1\]):[0-9]+$ ]]; then
  echo "agent-fs: root contains a newline or AGENTFS_LISTEN is not a loopback host:port" >&2
  exit 2
fi
source_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
install_dir=${AGENTFS_INSTALL_DIR:-${HOME:?}/.local/bin}
config_dir=${XDG_CONFIG_HOME:-${HOME:?}/.config}/agent-fs
data_dir=${XDG_DATA_HOME:-${HOME:?}/.local/share}/agent-fs
unit_dir=${XDG_CONFIG_HOME:-${HOME:?}/.config}/systemd/user

command -v go >/dev/null 2>&1 || { echo "agent-fs: Go 1.26+ is required" >&2; exit 1; }
command -v systemctl >/dev/null 2>&1 || { echo "agent-fs: this installer requires systemd --user" >&2; exit 1; }
command -v curl >/dev/null 2>&1 || { echo "agent-fs: curl is required for the health check" >&2; exit 1; }

temporary_dir=$(mktemp -d)
trap 'rm -rf -- "$temporary_dir"' EXIT
mkdir -p "$install_dir" "$config_dir" "$data_dir" "$unit_dir"
(cd "$source_root" && go build -trimpath -o "$temporary_dir/agent-fs" ./cmd/agent-fs)
install -m 0755 "$temporary_dir/agent-fs" "$install_dir/agent-fs"

escaped_root=${project_root//\/\\}
escaped_root=${escaped_root//\"/\\\"}
escaped_root=${escaped_root//%/%%}
escaped_binary=${install_dir//%/%%}/agent-fs
escaped_database=${data_dir//%/%%}/community.db

unit_path="$unit_dir/agent-fs.service"
if [[ -f "$unit_path" ]]; then
  cp -p "$unit_path" "$unit_path.bak.$(date +%Y%m%d%H%M%S)"
fi
cat >"$unit_path" <<UNIT
[Unit]
Description=Agent-FS community local context daemon
After=default.target

[Service]
Type=simple
ExecStart="$escaped_binary" --db "$escaped_database" daemon --listen "$listen_address" --root "$escaped_root"
Restart=on-failure
RestartSec=2
NoNewPrivileges=true

[Install]
WantedBy=default.target
UNIT

# OpenCC discovers traditional MCP servers from ~/.claude/settings.json. The
# same entry is also understood by Claude-compatible tooling. Preserve a backup
# before changing an existing settings file.
claude_settings=${CLAUDE_CONFIG_DIR:-${HOME:?}/.claude}/settings.json
if command -v python3 >/dev/null 2>&1; then
  mkdir -p "$(dirname "$claude_settings")"
  if [[ -f "$claude_settings" ]]; then
    cp -p "$claude_settings" "$claude_settings.bak.$(date +%Y%m%d%H%M%S)"
  fi
  python3 - "$claude_settings" "$endpoint" <<'PY'
import json, os, sys, tempfile
path, endpoint = sys.argv[1:]
try:
    with open(path, encoding="utf-8") as source:
        settings = json.load(source)
except FileNotFoundError:
    settings = {}
servers = settings.setdefault("mcpServers", {})
servers["agentfs"] = {"type": "http", "url": endpoint}
directory = os.path.dirname(path)
fd, temporary = tempfile.mkstemp(prefix="agentfs-settings-", dir=directory, text=True)
try:
    with os.fdopen(fd, "w", encoding="utf-8") as output:
        json.dump(settings, output, ensure_ascii=False, indent=2)
        output.write("\n")
    os.replace(temporary, path)
finally:
    if os.path.exists(temporary):
        os.unlink(temporary)
PY
else
  echo "agent-fs: python3 missing; OpenCC config template: $source_root/compat/opencc-agentfs.json" >&2
fi

if command -v claude >/dev/null 2>&1; then
  if ! claude mcp get agentfs >/dev/null 2>&1; then
    claude mcp add --transport http --scope user agentfs "$endpoint" >/dev/null
  fi
else
  echo "agent-fs: Claude Code not installed; use $source_root/compat/claude-agentfs.json"
fi

if command -v codex >/dev/null 2>&1; then
  if ! codex mcp get agentfs >/dev/null 2>&1; then
    codex mcp add agentfs --url "$endpoint" >/dev/null
  fi
else
  echo "agent-fs: Codex not installed; merge $source_root/compat/codex-agentfs.toml"
fi

systemctl --user daemon-reload
systemctl --user enable --now agent-fs.service

for _ in {1..100}; do
  if curl --fail --silent "http://${listen_address}/healthz" >/dev/null 2>&1; then
    echo "agent-fs installed: root=$project_root mcp=$endpoint"
    echo "status: systemctl --user status agent-fs.service"
    echo "logs:   journalctl --user -u agent-fs.service -f"
    exit 0
  fi
  sleep 0.1
done

echo "agent-fs: daemon did not become healthy" >&2
systemctl --user status agent-fs.service --no-pager >&2 || true
exit 1

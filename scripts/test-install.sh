#!/usr/bin/env bash
set -euo pipefail

source_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
sandbox=$(mktemp -d)
go_module_cache=$(go env GOMODCACHE)
go_build_cache=$(go env GOCACHE)
trap 'chmod -R u+w "$sandbox" 2>/dev/null || true; rm -rf -- "$sandbox"' EXIT
mkdir -p "$sandbox/home" "$sandbox/bin" "$sandbox/workspace"
export AGENTFS_TEST_LOG="$sandbox/client-calls.log"

for command_name in claude codex; do
  cat >"$sandbox/bin/$command_name" <<'STUB'
#!/usr/bin/env bash
printf '%s %s\n' "$(basename "$0")" "$*" >>"$AGENTFS_TEST_LOG"
if [[ "${1:-}" == mcp && "${2:-}" == get ]]; then exit 1; fi
exit 0
STUB
  chmod 0755 "$sandbox/bin/$command_name"
done

cat >"$sandbox/bin/systemctl" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
cat >"$sandbox/bin/curl" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod 0755 "$sandbox/bin/systemctl" "$sandbox/bin/curl"

HOME="$sandbox/home" \
XDG_CONFIG_HOME="$sandbox/home/.config" \
XDG_DATA_HOME="$sandbox/home/.local/share" \
AGENTFS_INSTALL_DIR="$sandbox/home/.local/bin" \
GOMODCACHE="$go_module_cache" \
GOCACHE="$go_build_cache" \
PATH="$sandbox/bin:$PATH" \
  "$source_root/scripts/install.sh" "$sandbox/workspace"

test -x "$sandbox/home/.local/bin/agent-fs"
test -f "$sandbox/home/.config/systemd/user/agent-fs.service"
test -f "$sandbox/home/.claude/settings.json"
grep -q '127.0.0.1:7337/mcp' "$sandbox/home/.claude/settings.json"
grep -q '^claude mcp add ' "$AGENTFS_TEST_LOG"
grep -q '^codex mcp add ' "$AGENTFS_TEST_LOG"
echo "agent-fs installer test passed"

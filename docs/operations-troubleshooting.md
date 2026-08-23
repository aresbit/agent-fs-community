# 运维与故障排查

## 1. 日常检查

```bash
systemctl --user status agent-fs.service
curl --fail-with-body -sS http://127.0.0.1:7337/healthz
./scripts/smoke-mcp.sh
agent-fs --db "$HOME/.local/share/agent-fs/community.db" doctor
```

建议把“进程健康”“MCP 可调用”“索引一致”分开检查：

- `/healthz` 只证明 server 能响应。
- `smoke-mcp.sh` 证明 initialize 通路可用。
- 实际 `context_pack` 证明索引中有可检索内容。
- `doctor` 证明数据库/FTS/外键/路径基础一致性。

## 2. 日志

```bash
journalctl --user -u agent-fs.service -f
journalctl --user -u agent-fs.service --since '30 minutes ago' --no-pager
systemctl --user show agent-fs.service -p ExecMainStatus -p NRestarts
```

watcher 的可恢复同步错误以 `agent-fs watcher:` 开头；致命 server、初始扫描或 watcher 错误会让 daemon
退出，由 unit 的 `Restart=on-failure` 在 2 秒后重启。

## 3. 常见问题

### daemon 启动但搜不到内容

1. 检查 unit 的 `--root` 是否是预期的绝对目录。
2. 初始完整扫描可能仍在运行，查看 journal。
3. 执行 `agent-fs query 'SELECT path,entry_count FROM scan_roots'`。
4. 直接执行一次 `agent-fs scan /absolute/root`，观察具体 parser/read 错误。
5. 确认 MCP 客户端连接的是同一端口和数据库对应 daemon。

### PDF 导致初始扫描失败

典型错误是 `pdftotext is required`。安装 Poppler 后重启，或把 PDF 移出当前 root。当前实现遇到单个
不可解析 PDF 会让完整扫描失败，不会静默跳过。

```bash
command -v pdftotext
pdftotext -v
systemctl --user restart agent-fs.service
```

### 真实 Embedding 无法启动

检查 URL、模型和维度是否同时存在。endpoint 返回的每个向量长度必须严格等于配置维度。

```bash
systemctl --user show-environment | rg AGENTFS_EMBEDDING
journalctl --user -u agent-fs.service -n 100 --no-pager
```

如果 key 来自 `EnvironmentFile`，不要把文件内容打印进共享日志。先用 provider 自己的健康接口验证，
再执行小目录 `scan`。

### 修改文件后检索结果没有更新

1. 等待至少一个 debounce 周期。
2. 查看 watcher 是否报告 `no space left on device` 或 `too many open files`。
3. 用 `stat` 和只读 SQL 对比 `mtime_ns`。
4. 对目标路径执行完整 root scan 以恢复一致性。
5. 高频编辑器 rename/replace 竞态会重试；持续失败通常意味着 watch limit 或读取错误。

### inotify watch 达到上限

Linux 每个被监听目录通常需要一个 watch：

```bash
cat /proc/sys/fs/inotify/max_user_watches
cat /proc/sys/fs/inotify/max_user_instances
find /absolute/root -type d | wc -l
```

临时调整示例：

```bash
sudo sysctl fs.inotify.max_user_watches=1048576
sudo sysctl fs.inotify.max_user_instances=1024
```

永久设置应由机器管理员写入 `/etc/sysctl.d/`。提高限制会增加内核内存使用；默认规则已经剪枝
`node_modules`、Bazel、语言缓存与常见构建输出。可用 `--exclude GLOB` 追加项目特有目录；同时应
避免嵌套重复 roots。

### `address already in use`

```bash
ss -ltnp 'sport = :7337'
systemctl --user status agent-fs.service
```

停止重复进程或选择另一个 loopback 端口，并同步更新 MCP 客户端 URL。

### `origin not allowed`

CLI/MCP SDK 通常不发送 Origin。浏览器应用会发送，应在 daemon 上精确添加：

```bash
agent-fs daemon --root /absolute/root --allow-origin http://127.0.0.1:3000
```

不要用通配字符串；当前实现是 exact match。

### schema incompatible

社区版有意拒绝企业权限 schema 和旧 Python schema。保留旧库作为备份，使用新路径：

```bash
agent-fs --db "$HOME/.local/share/agent-fs/community-v1.db" scan /absolute/root
```

不要手工改 `user_version` 绕过检查。

### `doctor` 发现 missing paths

这通常表示 daemon 停止期间文件被删除，或 watcher 曾丢事件。先完整扫描对应 root；再运行 `doctor`。
若 `quick_check`/FTS/外键失败，停止 daemon，保留故障数据库供诊断，然后用新数据库完整重建。

## 4. 重建策略

| 症状 | 优先动作 |
|---|---|
| 仅 files FTS 不一致 | `rebuild-fts` 后 `doctor` |
| 路径 stale/missing | 完整 `scan ROOT` |
| Embedding 模型切换 | 新 DB 或完整重扫所有 roots |
| schema 不兼容 | 新 DB，不做原地强迁移 |
| SQLite quick_check 失败 | 停服务、保留证据、新 DB 重建 |
| recovery 报状态歧义 | 不要删除 tombstone；人工核对文件与 journal |

## 5. 备份和恢复

索引可重建，通常备份价值低于 workspace 本身。确需备份时：

```bash
systemctl --user stop agent-fs.service
cp "$HOME/.local/share/agent-fs/community.db" /safe/backup/community.db
systemctl --user start agent-fs.service
```

不要只在运行时复制主 DB 而忽略 WAL。恢复索引后执行 `doctor` 和完整扫描，确保它重新对齐当前文件
系统。数据库包含文件预览、Chunk 和向量，应按私有代码数据同等级保护。

## 6. 安全事件

如果端口曾被代理到不可信网络：

1. 立即停止代理和 daemon。
2. 假设所有已索引文本已可能泄露。
3. 轮换索引中可能出现的 secret，而不只是 Embedding API key。
4. 检查 shell、代理、SSH 和容器日志；社区版没有请求审计可用于完整追溯。
5. 按 [`SECURITY.md`](../SECURITY.md) 向维护者私下报告产品漏洞。

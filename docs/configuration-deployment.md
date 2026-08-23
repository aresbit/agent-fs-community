# 配置与部署

## 1. 全局 CLI 配置

全局参数必须位于 COMMAND 前。

| 参数 | 默认值 | 环境变量/说明 |
|---|---:|---|
| `--db PATH` | `~/.agent-fs/fs.db` | `AGENTFS_DB` 可覆盖默认路径 |
| `--content-bytes N` | 8192 | 每个文件写入 `content_head` 的文本预览上限 |
| `--extract-bytes N` | 2097152 | 每个文件最大提取文本 bytes |
| `--max-rows N` | 1000 | 一次只读 SQL/快捷查询最大返回行数 |
| `--embedding-url URL` | 空 | `AGENTFS_EMBEDDING_URL`；空表示本地 hash |
| `--embedding-model NAME` | 空 | `AGENTFS_EMBEDDING_MODEL` |
| `--embedding-dimensions N` | 0 | `AGENTFS_EMBEDDING_DIMENSIONS` |
| `--embedding-key-env NAME` | `AGENTFS_EMBEDDING_KEY` | 指定存放 API key 的环境变量名 |
| `--exclude GLOB` | 可重复 | 在默认集合外追加 basename glob |
| `--no-default-excludes` | false | 完全关闭内置工程目录排除 |

`--embedding-url` 非空时，model 和正 dimensions 也必须存在。URL 可以给 base URL 或完整
`/v1/embeddings`；代码会自动补全后者。

## 2. daemon 参数

```text
agent-fs [全局参数] daemon --root ROOT [--root ROOT...] [daemon 参数]
```

| 参数 | 默认值 | 说明 |
|---|---:|---|
| `--root PATH` | 无 | 至少一个，可重复；监听并索引该目录树 |
| `--listen HOST:PORT` | `127.0.0.1:7337` | 仅接受 localhost 或 loopback IP |
| `--debounce DURATION` | `100ms` | Go duration，如 `250ms`、`1s` |
| `--allow-origin ORIGIN` | 空 | 可重复；只影响带浏览器 Origin header 的请求 |

`serve` 是 `daemon` 的别名。`0.0.0.0`、真实网卡 IP 和非 loopback hostname 会被 CLI 拒绝。

## 3. 目录排除

零配置默认排除以下 basename globs，规则应用于 root 下任意层级，并在目录入口直接剪枝：

```text
.git .hg .svn .repo .jj
.cache .ccache .sccache .bazel-cache bazel-* buck-out
node_modules .yarn .pnpm-store
__pycache__ .mypy_cache .pytest_cache .ruff_cache .tox .venv venv
.gradle .next .nuxt .pants.d .turbo .nx
build dist out target coverage htmlcov
DerivedData Pods
```

`bazel-*` 同时排除常见的 `bazel-bin`、`bazel-out`、`bazel-testlogs` 和 `bazel-WORKSPACE`。规则只
匹配单个 basename，不允许 `/` 或 `\\`，Linux 下区分大小写。显式配置的 root 本身始终允许，即使
它的 basename 与默认规则相同；排除从 root 的子项开始。

追加项目规则时，全局 flag 必须写在 COMMAND 前：

```bash
agent-fs --exclude 'generated-*' --exclude vendor daemon --root /absolute/project
```

如果项目确实需要检索名为 `build`、`target` 或 `Pods` 的源码目录，可关闭全部默认值并显式重建所需
集合：

```bash
agent-fs --no-default-excludes --exclude .git --exclude .cache daemon --root /absolute/project
```

变更排除配置后应执行完整 `scan ROOT` 或重启 daemon；初始扫描会把数据库中旧的排除子树作为 stale
数据删除。排除是索引/性能策略，不是访问控制，daemon 进程仍以当前 OS 用户身份运行。

## 4. 本地默认 Embedding

不设置任何 embedding 参数时使用 256 维 feature hash：

```bash
agent-fs --db /absolute/index.db daemon --root /absolute/project
```

这是隐私和部署最简单的模式。它能利用 token 相似性，但不等同于训练过的语义模型。

## 5. OpenAI-compatible Embedding

```bash
export AGENTFS_EMBEDDING_URL=http://127.0.0.1:8000
export AGENTFS_EMBEDDING_MODEL=BAAI/bge-m3
export AGENTFS_EMBEDDING_DIMENSIONS=1024
export AGENTFS_EMBEDDING_KEY='replace-with-provider-key'

agent-fs --db /absolute/bge-m3.db daemon --root /absolute/project
```

如果本地服务不需要认证，可以不设置 key。远程 provider 会收到索引文本和查询文本；使用前应检查
隐私、地域、保留和费用策略。不要把 key 写进 systemd unit 的命令行或提交到仓库。

systemd 推荐通过仅用户可读的 EnvironmentFile：

```ini
[Service]
EnvironmentFile=%h/.config/agent-fs/embedding.env
ExecStart=%h/.local/bin/agent-fs --db %h/.local/share/agent-fs/community.db --embedding-url http://127.0.0.1:8000 --embedding-model BAAI/bge-m3 --embedding-dimensions 1024 daemon --root /absolute/project
```

```bash
chmod 600 "$HOME/.config/agent-fs/embedding.env"
systemctl --user daemon-reload
systemctl --user restart agent-fs.service
```

注意：systemd 不会继承你当前 shell 临时 export 的变量。

## 6. 一键安装器配置

```bash
AGENTFS_LISTEN=127.0.0.1:7444 \
AGENTFS_INSTALL_DIR="$HOME/bin" \
XDG_CONFIG_HOME="$HOME/.config" \
XDG_DATA_HOME="$HOME/.local/share" \
./scripts/install.sh /absolute/project
```

支持的安装时变量：

| 变量 | 用途 |
|---|---|
| `AGENTFS_LISTEN` | loopback 监听地址 |
| `AGENTFS_INSTALL_DIR` | binary 安装目录 |
| `XDG_CONFIG_HOME` | Agent-FS 与 systemd user 配置基目录 |
| `XDG_DATA_HOME` | 数据库基目录 |
| `CLAUDE_CONFIG_DIR` | Claude/OpenCC settings 所在目录 |

安装器目前把一个位置参数写成一个 `--root`，也不会自动把 embedding 参数写进 unit。

## 7. 多 root systemd 配置

编辑 `~/.config/systemd/user/agent-fs.service`：

```ini
[Service]
ExecStart=
ExecStart=%h/.local/bin/agent-fs --db %h/.local/share/agent-fs/community.db daemon --listen 127.0.0.1:7337 --root /absolute/code --root /absolute/docs
```

对 systemd drop-in，第一行空 `ExecStart=` 用于清除原值。保存后：

```bash
systemctl --user daemon-reload
systemctl --user restart agent-fs.service
journalctl --user -u agent-fs.service -n 100 --no-pager
```

## 8. 容器与远程部署

社区版不建议容器发布端口或远程部署。即使进程只绑定容器内 `127.0.0.1`，sidecar、host network、
代理或 SSH tunnel 也可能改变可达边界。社区版没有认证/授权来补偿这种暴露。

如只想在 devcontainer 内给同一容器中的 Agent 使用，应同时满足：

- daemon 与 Agent 在同一受信任容器/用户边界；
- 不做 `-p 7337:7337`；
- 数据库和 workspace volume 只对该用户开放；
- MCP 客户端连接容器内 loopback。

## 9. 数据库路径与权限

`Open` 会创建 0700 的数据库父目录并把数据库文件设为 0600。SQLite WAL/SHM 是运行时 artifacts；
备份时应先停止 daemon，或使用 SQLite 一致性备份工具。不要把数据库放在被索引 root 内，虽然代码
会排除数据库及其 WAL/SHM/journal，分离存放仍更易运维。

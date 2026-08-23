# Agent-FS Community

Agent-FS Community 是开发机上的统一 Agent 上下文服务。它持续监听本地代码和文档，建立
BM25 + embedding + 元数据混合索引，并通过 MCP/HTTP 同时提供给 Claude Code、Codex、
OpenCC 和其他 Agent。

社区版专注单用户、单机开发体验：没有租户、ACL、审计、远程监听或企业控制面。daemon
强制绑定 loopback；不要把端口通过公网、容器端口映射或不可信反向代理暴露出去。

## 一键安装

要求 Linux、systemd user service 和 Go 1.26+：

```bash
git clone https://github.com/aresbit/agent-fs-community.git
cd agent-fs-community
./scripts/install.sh /absolute/path/to/your/workspace
```

安装器会：

1. 编译并安装到 `~/.local/bin/agent-fs`；
2. 创建并启动 `~/.config/systemd/user/agent-fs.service`；
3. 将索引保存在 `~/.local/share/agent-fs/community.db`；
4. 配置 Claude Code 和 Codex（客户端存在时）；
5. 写入 OpenCC 可发现的 `~/.claude/settings.json` MCP 配置；
6. 等待 `/healthz` 通过后才返回成功。

已有 systemd/Claude 配置会先生成带时间戳的备份。查看运行状态：

```bash
systemctl --user status agent-fs.service
journalctl --user -u agent-fs.service -f
./scripts/smoke-mcp.sh
```

## 手动运行

```bash
go build -o bin/agent-fs ./cmd/agent-fs
bin/agent-fs --db ~/.local/share/agent-fs/community.db daemon \
  --root /absolute/code --root /absolute/docs \
  --listen 127.0.0.1:7337
```

daemon 启动时扫描每个 root，随后通过 fsnotify 增量同步。MCP endpoint：
`http://127.0.0.1:7337/mcp`。

## Agent 工具

- `context_pack`：首选工具。一次调用完成检索、排序、代码/文档 chunk 读取和 Token 预算裁剪。
- `search`：返回相关路径、symbol、行号、snippet 和混合分数。

推荐提示词：

```text
开始任务前先调用 agentfs.context_pack，把完整任务作为 query，token_budget 设为 4000-6000。
只有返回证据不完整时才调用 search；不要先递归读取整个仓库。
```

## 客户端配置

安装脚本会自动配置；手动配置模板在 [`compat/`](compat/)：

- Claude Code：[`compat/claude-agentfs.json`](compat/claude-agentfs.json)
- Codex：[`compat/codex-agentfs.toml`](compat/codex-agentfs.toml)
- OpenCC：[`compat/opencc-agentfs.json`](compat/opencc-agentfs.json)

具体命令和验证步骤见 [`MCP_QUICKSTART.md`](MCP_QUICKSTART.md)。

## 支持的上下文

- Go：标准库 AST declaration chunk，保留 symbol 与行号；
- 其他代码/文本：有重叠的有界窗口 chunk；
- PDF：通过 Poppler `pdftotext` 抽取；
- DOCX/PPTX/XLSX：通过 zip+xml 有界解析；
- embedding：默认离线 feature hash，也可连接 OpenAI-compatible `/v1/embeddings`。

真实 embedding 示例：

```bash
export AGENTFS_EMBEDDING_URL=http://127.0.0.1:8000
export AGENTFS_EMBEDDING_MODEL=BAAI/bge-m3
export AGENTFS_EMBEDDING_DIMENSIONS=1024
export AGENTFS_EMBEDDING_KEY=provider-secret
```

## 数据与恢复

真实文件系统是事实源，SQLite 是可重建语义索引。扫描和增量更新在事务中提交；rename 和
remove 使用持久 operation journal，在进程崩溃后由下次 `Open` 幂等恢复。

## 开发

```bash
make check
```

`make check` 执行 gofmt 检查、vet、build 和全包 race test。

## 完整文档

完整设计与使用文档从 [`docs/README.md`](docs/README.md) 开始，包括：

- [总体架构设计](docs/architecture.md)与[特性设计](docs/features.md)；
- [代码模块设计](docs/modules.md)与[数据模型/崩溃恢复](docs/data-model-recovery.md)；
- [快速开始](docs/user-guide.md)、[MCP/HTTP API](docs/mcp-http-api.md)和[配置部署](docs/configuration-deployment.md)；
- [运维排障](docs/operations-troubleshooting.md)、[开发测试](docs/development-testing.md)、
  [性能基准方法](docs/performance-benchmarking.md)与[安全模型](docs/security.md)。

## 社区版边界

本仓库有意不包含：多租户、文件权限策略、审计日志、远程暴露、集中控制台和企业 SLA。
社区 daemon 只适合受信任的本机用户和开发目录。

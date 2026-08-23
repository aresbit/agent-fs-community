# 性能与基准方法

## 1. 当前声明

当前仓库没有提交可复现的百万文件 benchmark harness、原始结果数据或 Claude Code/Codex/OpenCC 的
真实任务 A/B 报告。因此不能仅根据实现声称：

- 百万文件增量 freshness P95 小于 1 秒；
- 查询 P95 小于 200ms；
- Agent 文件工具调用减少至少 70%；
- 上下文 Token 减少至少 40%。

这些是合理的产品验收目标，但在发布数字前必须按本文方法获得可复现证据。

## 2. 指标定义

### 索引指标

| 指标 | 定义 |
|---|---|
| 初始扫描吞吐 | 成功提交的路径数 / 从 Scan 开始到 commit 的 wall time |
| 初始扫描峰值 RSS | 扫描期间进程 resident set 峰值 |
| 数据库大小 | DB + WAL + SHM 的稳定总 bytes |
| 增量 freshness | 文件变更被 watcher 观察到到 `SyncPaths` commit 完成的时长 |
| 丢失率 | 变更 workload 结束后与权威树 diff 仍不一致的路径比例 |
| 重启恢复时间 | 进程启动到 journal 全部进入终态并可正确查询的时长 |

### 查询指标

| 指标 | 定义 |
|---|---|
| P50/P95/P99 latency | client 发出请求到完整 response body 收到 |
| QPS | 固定并发下成功查询数 / 时间 |
| Recall@K | 有标注相关文件/Chunk 中进入前 K 的比例 |
| MRR | 首个相关结果 reciprocal rank 的均值 |
| Context precision | Context Pack 中与任务实际相关的 Token 比例 |

### Agent 指标

| 指标 | 定义 |
|---|---|
| 任务成功率 | 盲审测试/验证命令通过的任务数比例 |
| 输入/输出 Token | 客户端或模型 API 的实际 tokenizer/accounting 数据 |
| wall latency | 从任务提交到最终答案/补丁完成 |
| 工具调用数 | 每个任务全部 tool call；另列文件相关 call |
| 无效读取 | 未被最终修复或解释使用的文件读取次数/bytes |

减少率统一使用 `(baseline - agentfs) / baseline × 100%`，同时报告绝对值和 bootstrap 置信区间。

## 3. 百万文件树

文件树必须可由固定 seed 重建，并覆盖真实分布而不是一百万个空文件：

- 目录数、深度、每目录 fan-out；
- 小文本、代码、二进制、PDF、DOCX/PPTX/XLSX 的比例；
- 文件大小分布与总 bytes；
- 重复内容、长路径、Unicode 名称、符号链接；
- Go symbol 数和普通窗口 Chunk 数；
- 数据库置于 root 外。

至少保存 manifest：`path, kind, size, content_hash, mtime_ns, expected_terms`。生成器、seed 和 manifest
checksum 必须与结果一起发布。

结果必须同时报告原始目录数、默认/自定义排除规则、剪枝后的 watch 数与索引条目数。否则无法判断
性能提升来自检索实现还是仅仅改变了输入集合。还要记录默认文件 allowlist、`--include-file` 和
`--all-files` 状态，并分别报告被接受/跳过的文件数与 bytes。

## 4. inotify 与高频变更测试

测试前记录：

```bash
uname -a
go version
cat /proc/sys/fs/inotify/max_user_watches
cat /proc/sys/fs/inotify/max_user_instances
ulimit -n
```

workload 至少包括：

1. 单文件连续 write；
2. 编辑器 temp + rename 原子替换；
3. 批量创建/删除目录；
4. 每秒 100/1,000/10,000 路径 burst；
5. 超过 debounce 的持续变更；
6. watcher 启动扫描期间发生变更；
7. 达到 inotify limit 的失败行为；
8. workload 后完整 scan 与 manifest diff。

`WatchOptions.OnSynced` 可记录单路径 freshness，但进程外基准还应以查询可见时间复核，避免只测 callback。

## 5. 崩溃恢复测试

对 `rename` 与 `remove` 的每个状态转换点注入 `SIGKILL`：intent 写入后、filesystem rename 后、DB commit
后、tombstone 清理前、journal 终态前。每个 case 重启至少 100 次随机化执行，并验证：

- old/new/stage 只出现允许组合；
- 索引与事实源一致；
- journal 最终为 done/rolled_back；
- `doctor` 通过；
- 重启恢复耗时 percentile。

## 6. 查询基准矩阵

把 query 分成：精确 symbol、自然语言功能、错误文本、路径/标签过滤、中文、混合中英文、无结果。
分别测：

- 默认 hash 与真实 Embedding；
- cold page cache 与 warm cache；
- 单并发与 4/16 并发；
- REST search、MCP search、context_pack；
- 10、20、100、200 limit；
- 10 万、100 万路径规模。

向量候选只查询同 sign-LSH bucket，因此质量 benchmark 必须同时报告 Recall@K，不能只报告 latency。

## 7. 真实 Agent A/B

选择固定且可自动验收的代码任务，至少覆盖 bugfix、跨文件功能、重构、测试补充、配置/文档问题。
每个任务从相同 commit、相同模型、相同推理参数和干净会话开始。

对比组：

- A：Agent 原生文件工具；
- B：官方 Filesystem MCP；
- C：Agent-FS，要求先 `context_pack`。

执行顺序随机化，每个 cell 重复至少 5 次。禁止把答案或相关文件路径写进某组专属 prompt。最终按任务
测试结果盲判成功，并记录 Token、延迟、所有 tool calls。Claude Code、Codex、OpenCC 分开报告，
不要把客户端差异混成一个总体均值。

## 8. 结果发布模板

```text
版本/commit:
机器/OS/文件系统:
Go 版本:
数据库与 Embedding 配置:
文件树 generator/seed/checksum:
冷/热状态与并发:
样本数:

初始扫描: duration, paths/s, peak RSS, DB bytes
增量: P50/P95/P99, max, lost events
查询: P50/P95/P99, QPS, Recall@10, MRR
Agent A/B: success, tokens, latency, calls（绝对值、减少率、CI）
原始数据位置:
已知异常/失败:
```

## 9. 防止误导

- 不用小树外推百万文件结果。
- 不用平均值代替 P95/P99。
- 不把估算 Token 当 API 实际 Token。
- 不只保留成功 run。
- 不在不同模型/硬件/任务上比较相对下降。
- 不把“本地 hash 向量存在”描述成已达到真实 Embedding 质量。

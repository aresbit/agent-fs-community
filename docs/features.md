# 特性设计

本文按“用户问题 → 设计 → 当前约束”说明主要特性。实现细节可继续查看
[代码模块设计](modules.md)和[数据模型](data-model-recovery.md)。

## 1. 事务性完整扫描

用户问题：第一次启动或索引与文件系统偏离时，需要建立一致快照，同时不能因中途读取失败而清空
原有索引。

设计：`Scan` 先递归收集目录树，对每个条目执行 stat、内容提取、Chunk 和向量生成；全部成功后再
进入 SQLite 事务。条目按深度和路径排序，以确保父目录在子条目前存在。事务提交时更新 root 的
现有行、删除 stale 行，并写入 `scan_roots` 统计。

约束：扫描时无法读取的非瞬时错误会让整次扫描失败；这保证旧快照保留，但意味着权限或损坏文档
需要先处理。符号链接本身被索引，不会递归跟随目标。

## 2. 文件监听与增量索引

用户问题：完整重扫成本高，Agent 又需要看到刚保存的代码。

设计：每个 root 使用一个 fsnotify watcher，递归监听所有目录。事件先按绝对路径合并，再经过默认
100ms 去抖；成熟路径通过 `SyncPaths` 批量写入。目录删除会删除索引中的整个子树，新建目录会被
递归加入 watch。同步失败的事件批次会重新排队。

扫描与 watcher 共享 basename glob 排除策略，并在进入目录前剪枝，而不是先递归再丢弃结果。默认
覆盖 VCS、Bazel、Node/Python 依赖缓存和常见构建输出；`--exclude` 可追加，
`--no-default-excludes` 可完全关闭。完整扫描会事务性清理旧数据库中已变为排除项的子树。

约束：Linux 上每个目录通常消耗一个 inotify watch，超大目录树需要提高内核限制。监听提供最终
一致性，不承诺在所有硬件和负载下固定小于 1 秒。

## 3. 文件与 Chunk 双层索引

用户问题：只返回整文件会注入大量无关 Token，只索引文件名又找不到具体实现。

设计：每个路径都有 `files` 行和文件级 Embedding；有文本的文件还会生成 `chunks` 行及 Chunk
Embedding。文件级文本预览默认最多 8 KiB，完整提取上限默认 2 MiB。检索时同时召回文件和 Chunk，
命中 Chunk 时保留 symbol、起止行号和精确内容。

约束：Chunk 不是完整源文件；需要上下文外的 import、类型或调用者时，Agent 可能需要再次搜索。

## 4. 混合检索

用户问题：精确符号适合词法搜索，自然语言问题适合向量检索，目录、类型、大小和标签又属于结构化
过滤；单一路径无法兼顾。

设计：

- 词法召回：FTS5 对文件和 Chunk 建候选，候选集内计算 BM25 排名。
- 向量召回：Embedding 以 float32 BLOB 保存，前 8 个向量维度的符号形成 bucket；先按模型和
  bucket 缩小候选，再算 cosine。
- 元数据：支持 `path_prefix`、`kinds`、`tags`、`min_size`、`max_size`。
- 排序：`score = 0.52 × lexical reciprocal rank + 0.38 × cosine + 0.10 × freshness`。
- 边界：默认返回 20 条、最多 200 条；候选数至少 256，随请求 limit 放大。

当前权重是代码常量，不是运行时配置。sign-LSH 是轻量候选策略，不应描述成完整向量数据库或保证
固定召回率的 ANN 实现。

## 5. Context Pack

用户问题：即使检索结果相关，Agent 逐条 search/read 仍会增加工具调用、往返延迟和重复 Token。

设计：`context_pack` 把完整任务作为一次查询，调用混合检索后直接加载命中 Chunk 或文件预览，
按得分排序并在预算内返回。默认预算 4,000，最大 32,000，默认最多 12 个结果。返回
`estimated_tokens` 和 `truncated`，让 Agent 判断是否需要补充检索。

Token 估算采用约 4 UTF-8 bytes/token，不是具体模型 tokenizer 的精确计数。减少 40% Token 或
70% 文件工具调用是需要真实 Agent A/B 基准验证的产品目标，不是仅由此算法即可证明的结果。

## 6. 文档与代码解析

| 类型 | 当前行为 | 依赖/限制 |
|---|---|---|
| Go | 标准库 parser 生成 declaration Chunk，保留函数、方法、type/var/const/import symbol 与行号 | 语法解析失败时回退窗口 Chunk |
| Python/JS/TS/Rust/C/C++/Java/Kotlin/Swift/Markdown/SQL/Shell | UTF-8 文本提取后生成 1,200 rune 窗口、200 rune 重叠 | 没有语言 AST |
| PDF | `pdftotext -layout -nopgbrk` 提取 | 必须安装 Poppler；30 秒超时 |
| DOCX | 读取正文、页眉和页脚 XML | 不还原复杂排版、批注或图片 OCR |
| PPTX | 按 slide XML 提取文本 | 不解析图片、讲者备注和视觉布局 |
| XLSX | shared strings 与 worksheet XML | 返回单元格文本，不重建公式语义和表结构 |
| 二进制/非 UTF-8 | 只保留文件元数据，不索引正文 | MIME 仍由内容探测生成 |

Office 解析会限制单个 XML entry 的膨胀比例和总提取字节数，降低压缩包异常膨胀风险。

## 7. Embedding

默认 `HashEmbedder(256)` 使用本地 feature hashing，对 token 哈希、带符号投影并归一化。它无网络、
确定性且不泄露内容，适合零配置启动。

设置 endpoint、model 和 dimensions 后使用 OpenAI-compatible `/v1/embeddings`。实现支持最多 64 条
文本一批、45 秒 HTTP 超时、最多 3 次重试；429 和 5xx 可重试。API key 只从用户指定的环境变量
读取，不写入数据库。

切换模型时，检索只读取与当前 `Embedder.Model()` 相同的向量。建议换模型后重建新数据库或完整
重扫，避免旧模型向量使部分文件暂时只依赖词法召回。

## 8. 只读 SQL 与快捷查询

CLI 提供 `query`、`ls`、`find`、`big`、`du`、`by-tag`。任意 SQL 只接受单条只读
`SELECT`、`WITH`、`EXPLAIN` 或白名单 PRAGMA，并通过只读 SQLite 连接执行；返回行数由
`--max-rows` 限制。它适合本地分析索引，不是通用数据库管理入口。

## 9. 可恢复文件操作

`rename` 和 `rm` 会真实修改文件系统，因此使用 `operation_journal` 记录意图与阶段。删除先把目标
原子 rename 为同目录隐藏 tombstone，再删除数据库行，最后清理 tombstone。进程重启时 `Open`
会检查文件系统和索引的组合状态，幂等完成或回滚操作。详见[数据模型与崩溃恢复](data-model-recovery.md)。

## 10. 健康与诊断

- `/healthz`：进程健康、MCP 协议标识、累计请求数和错误数。
- `doctor`：FTS integrity-check、SQLite quick_check、外键、files/FTS 行数及路径存在性。
- `rebuild-fts`：根据外部内容表重建文件 FTS。

`/healthz` 不是索引 freshness 或完整扫描完成信号。生产级可观测性（直方图、trace、审计）不在当前
社区版内。

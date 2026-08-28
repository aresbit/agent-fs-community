# 概念共现图（Concept Graph）—— agent-fs 从「文件/符号检索」到「概念导航」

> 状态：提案（未实现）。对应 `/home/pc/yysdl/agent-fs/` 下 Python 原型的 Go 重写。
> 目标读者：agent-fs 维护者。前置阅读：`docs/architecture.md`、`schema.sql`、`tokenize.go`。

## 1. 问题与动机

agent-fs 现在能回答三类问题：

| 问题 | 现有能力 | 数据基础 |
|---|---|---|
| 「这个文件讲什么」 | `search` / `context_pack` | `files_fts` + `chunks_fts` + embedding |
| 「谁定义/调用了这个符号」 | symbol graph | `symbols` + `symbol_refs` |
| 「这个概念和什么相关」 | **没有** | — |

第三类缺口在知识库场景最痛。一个 662 门课程、8500 章的知识库，用户想从「卡尔曼滤波」发散到「协方差 / EKF / 粒子滤波 / 贝叶斯估计」，再跳到讲它们的课程——现有系统只能做「卡尔曼滤波出现在哪些文件」（全文命中），做不到「卡尔曼滤波和哪些概念同现、强度如何」。

Python 原型 `knowledge_graph.py` 验证了这个需求：`kg_query.py 卡尔曼滤波` → 协方差/高斯/EKF/非线性。但它有两个不能合入 agent-fs 的硬伤：

1. **分词用 jieba**，与 agent-fs 的 `analyzeTerms`（bigram）是两套词表，结果不一致。
2. **Python + 独立 SQLite**，游离在 agent-fs 的 `Store` 事务、增量同步、`operation_journal` 崩溃恢复之外。

本提案把「概念共现」作为 agent-fs 的**原生第三层关系**（文件 → 符号 → 概念），用 Go + 已有分词器重写。

## 2. 为什么是「共现」，而不是「向量相似度」

agent-fs 已经有 embedding（`embeddings` / `chunk_embeddings`，sign-LSH 分桶）。一个自然的问题：为什么还要共现图，直接查「卡尔曼滤波的向量最近邻」不就行了？

两者是**互补的两种相关**，不是重复：

| | 向量相似度 | 概念共现 |
|---|---|---|
| 相关定义 | 语义相近（向量夹角小） | 统计同现（常一起出现） |
| 依赖 | 模型质量、维度、归一化 | 无模型，纯统计 |
| 可解释性 | 弱（黑盒分数） | 强（「共现 7 次」可查证） |
| 导航 | 只能给「最像的 top-k」 | 可沿边遍历：概念→概念→课程 |
| 成本 | 每次查询要编码 query | 建图一次，查询纯 SQL 走边 |

关键：共现图回答的是「**知识结构**」问题（哪些概念属于同一主题簇），向量回答的是「**语义**」问题（哪些内容意思接近）。知识库里「傅里叶变换」和「卷积定理」共现，不是因为它们向量像，而是因为它们在同一章反复被一起讲。共现图把这个「一起被讲」的事实显式化，且无需模型、可解释、可沿边导航。

二者在 `HybridSearch` 里可以融合：共现强度作为一个信号，与 BM25 / 向量分数一起进 `reciprocalRank`。

## 3. 核心设计决策

### D1：概念 = Markdown 标题 + 高频术语（复用 `analyzeTerms`）

概念词表两层：

1. **结构化概念**：Markdown `h1/h2/h3` 标题。章节标题天然是概念（「分支预测」「缓存一致性」），`_norm` 去掉编号前缀（`## 3.1 分支预测` → `分支预测`）。
2. **术语概念**：正文用 **gse 整词分词**（`gse.NewEmbed` 内嵌词典 + `CutStop` 去停用词）后做词频统计，取出现 ≥2 次的整词（去 markdown/LaTeX/标点噪声）。整词分词才能拿到「数据冒险」「协方差」这类真概念。

> **为什么否决 bigram（教训）**：最初设计复用 `analyzeTerms`（bigram）提取概念，理由是「不引入分词依赖」。实测发现 bigram 把「数据冒险」切成「数据/据冒/冒险」，概念节点碎片化严重（`related 冒险` 返回「险检/据冒/制冒」），且碎片 bigram 和真词一样高频，词频阈值过滤不掉。根因：**bigram 是检索粒度（中缀匹配），不是概念粒度（整词）**。最终改用 gse（纯 Go、go:embed 内嵌词典，无 CGO、无外部文件，符合 agent-fs 自包含约束；gojieba 依赖 cppjieba 的 C++ 与外部词典，故弃）。

与 Python 版对齐：Python 用 `jieba.analyse.extract_tags`（TF-IDF），Go 版用 `gse.CutStop`（整词分词）+ 词频。粒度一致（整词），不引入 TF-IDF 的 IDF 词典（对单个 chunk，词频已是可用的 TF）。

> 信息隐藏：调用方只见 `concept name`，不关心它是标题还是 bigram。概念来源（heading/term）是 `concepts.kind` 字段，查询时透明。

### D2：共现边按「chunk」聚合，权重 = 频次截断 + 关联度量（两段式）

Python 版用裸共现次数做边权，有两个缺陷，分别对应信息检索里两个已知问题
（Stanford CS276 ch08 查询扩展、ch11 特征选择）：

1. **高频泛化概念**（「系统」「函数」「方法」）和所有东西都共现，会把无关概念拉成邻居 —— 需要「关联度量」归一化。
2. **罕见词对偶然共现**（两个冷门词恰好出现一次）会被当成强关联 —— 这是**互信息 MI 的已知缺陷（偏向罕见词）**，需要「频次截断」先过滤。

所以正确度量是两段式，而非裸 PMI：

1. **频次截断**：`co_count >= θ`（θ 起步取 2）才保留边，过滤偶然共现的罕见词对。
2. **关联度量**：在截断之上用 PMI 归一化，惩罚「和谁都共现」的泛化词：

```
PMI(a,b) = log( P(a,b) / (P(a)·P(b)) )
```

P(a,b) 是同现概率（同一 chunk），P(a)、P(b) 是各自出现概率。最终排序键
`score = co_count · PMI(a,b)`（截断保证下限，PMI 做归一化），比单独用 co_count
或单独用 PMI 都稳。

共现的**统计单元**选 chunk 而不是文件：一个大文件（比如 8000 行的 index.md）里两个概念同现，不代表它们相关；同一 chunk（语义上紧邻的片段）里同现才相关。agent-fs 已有 `chunks` 表，直接复用。

> S1 当前只落地了第 1 段（`co_count` 频率排序，等价频次截断），第 2 段的 PMI
> 归一化留到 S2 的 `Related` 查询时派生（`concepts.doc_count` 已物化，P(a)/P(b)
> 可直接算）。

### D3：边物化存储，共现随 `chunks` 事务一致更新

两个选择：

- **查询时算**：`Related(a)` 临时 join `chunks`，O(全表)。
- **物化边表**：`concept_edges` 存 `(src, dst, weight)`，查询 O(1) 走索引。

选**物化**。理由：共现图是「建图一次、查询多次」的读多写少负载，与 embedding 的「算一次、存下来、查时走 bucket」同构。物化边表让 `Related` 变成纯索引查询，和 `symbols`/`symbol_refs` 的「预计算关系」一致。

代价是增量一致性：`chunks` 增删改时，涉及的共现边要同步更新。复用 `scan.go` 的 `replaceChunks` 事务边界——共现边重算与 chunk 替换放在**同一个 `sql.Tx`**，靠 `operation_journal` 的崩溃恢复保证不丢。

### D4：概念图是 `Store` 的子模块，不新增顶层服务

概念图不引入新进程、新端口、新存储文件。它作为 `Store` 的第三个关系子模块，与 symbol graph 平级：

```
Store
├── 文件检索（files_fts + chunks_fts + embeddings）
├── 符号图（symbols + symbol_refs）        ← 已有
└── 概念图（concepts + concept_edges）      ← 新增
```

对外新增两个窄接口，其余全部藏在 `Store` 内部（深模块）：

```go
// Related 返回与 name 共现强度最高的概念，按 PMI 降序。
func (s *Store) Related(ctx context.Context, name string, limit int) ([]Concept, error)

// ConceptOccurrences 返回 name 出现的文件/chunk（复用 files_fts/chunks_fts 命中）。
func (s *Store) ConceptOccurrences(ctx context.Context, name string, limit int) ([]SearchHit, error)
```

## 4. 数据模型（schema v3）

```sql
-- 概念节点。name 归一化后唯一；kind 区分标题词 vs 术语词；doc_count 供 PMI 算 P(·)。
CREATE TABLE IF NOT EXISTS concepts (
  id         INTEGER PRIMARY KEY,
  name       TEXT    NOT NULL UNIQUE,
  kind       TEXT    NOT NULL CHECK (kind IN ('heading','term')),
  doc_count  INTEGER NOT NULL DEFAULT 0,
  created_at_ns INTEGER NOT NULL
);

-- 概念→chunk 出现关系，统计单元是 chunk。
CREATE TABLE IF NOT EXISTS concept_occurrences (
  concept_id INTEGER NOT NULL REFERENCES concepts(id) ON DELETE CASCADE,
  chunk_id   INTEGER NOT NULL REFERENCES chunks(id)  ON DELETE CASCADE,
  PRIMARY KEY (concept_id, chunk_id)
);
CREATE INDEX IF NOT EXISTS idx_concept_occ_chunk ON concept_occurrences(chunk_id);

-- 物化共现边。weight = PMI，无向边存成 (min,max) 使 (src,dst) 唯一。
CREATE TABLE IF NOT EXISTS concept_edges (
  src    INTEGER NOT NULL REFERENCES concepts(id) ON DELETE CASCADE,
  dst    INTEGER NOT NULL REFERENCES concepts(id) ON DELETE CASCADE,
  weight REAL    NOT NULL,
  PRIMARY KEY (src, dst)
);
CREATE INDEX IF NOT EXISTS idx_concept_edges_dst ON concept_edges(dst, weight DESC);
```

`concept_edges` 存无向边为 `(min(id), max(id))`，查询 `Related(a)` 时 `WHERE src=? OR dst=?`，两条索引覆盖。这与 `symbol_refs` 的有向边（caller→callee）不同，因为概念共现本身无方向。

`doc_count` 在 `replaceChunks` 里随 chunk 增删维护，PMI 在提交前重算——不引入独立的「重建」命令，靠既有 `RebuildFTS` / `Check` 的路径覆盖。

## 5. 概念提取流程（集成进 scan）

在 `scan.go` 的 `replaceChunks` 之后、事务提交之前插入一步 `indexConcepts(tx, fileID, chunks)`：

1. 对每个 chunk 的原文（`chunks.content`）跑标题正则 `^#{1,3}\s` 提取 heading 概念。
2. 对正文跑 `analyzeTerms`，词频 ≥ 阈值的 term 收为 term 概念（停用词表复用 `tokenize.go` 的过滤思路，加一份知识库领域停用词）。
3. 概念 → chunk 写入 `concept_occurrences`；同 chunk 内的概念两两共现，累加进 `concept_edges` 的 PMI。

时间复杂度：每个 chunk O(k²) 两两配对，k = chunk 内概念数（通常 < 20），可控。大文件被 parser 切成多 chunk，天然限制了单个 chunk 的概念数。

## 6. 与 HybridSearch 的整合

概念图不是替代 `HybridSearch`，而是给它加一路信号 + 一个新的入口：

- **新命令 `related`**：`agent-fs related 卡尔曼滤波` → 按 PMI 降序列相关概念，每条附「出处文件」跳转。这是纯概念图查询，不走 embedding。
- **HybridSearch 融合**：共现强度作为一个候选信号进 `reciprocalRank`。当一个查询词命中概念 A，A 的高共现概念 B 所在 chunk 获得加权。这一步是增量增强，先做命令，再谈融合，避免一步跨太大。

## 7. 实现路径

分三个可独立合并的步骤：

1. **S1：概念提取 + 建图**（`concepts`/`concept_occurrences`/`concept_edges` 三张表 + `indexConcepts` 集成进 `replaceChunks`）。迁移 v2→v3，backfill 已有 chunk。
2. **S2：`Related` / `ConceptOccurrences` 接口 + `related` 命令**。纯读接口，用 `tokenize.go` 的 `analyzeTerms` 做查询侧概念归一化（查「卡尔曼滤波」→ 切成 bigram → 匹配 heading 节点「卡尔曼滤波」优先）。
3. **S3：HybridSearch 融合共现信号**。把共现强度并入 `reciprocalRank`，评估对检索质量的影响（需要 benchmark 佐证才合并）。

## 8. 验收标准

- S1：对 opc2 知识库 scan 后，`concepts` 节点数 > 10000，`concept_edges` 边数 > 10 万，`Check` 通过（表结构与数据一致）。
- S2：`related 卡尔曼滤波` 返回协方差/高斯/EKF 等，且 PMI 排名明显优于「卡尔曼滤波」的裸共现次数排名（泛化词被压下去）。
- S3：有 benchmark 证明融合共现信号后检索质量提升（NDCG / 命中率），否则不合并。
- 全链路：`go test ./...`、`go vet ./...`、`gofmt -l` 通过，与现有 schema 迁移、增量同步、崩溃恢复路径兼容。

## 9. 不做的事（划清边界）

- **不做**通用知识图谱/本体推理（entity 消歧、关系抽取）。只做「共现」这一种可查证的统计关系。
- **不做**自动概念聚类/主题建模。共现图是原始关系，聚类是下游消费方的事。
- **不引入** jieba 或任何 Go 之外的分词器。唯一分词入口是 `analyzeTerms`。
- **不改变** 现有 `files_fts`/`chunks_fts`/`symbols` 的任何语义，纯增量三张表 + 一个命令。

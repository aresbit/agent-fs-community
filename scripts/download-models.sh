#!/usr/bin/env bash
set -euo pipefail

# 下载 agent-fs 的本地语义模型。这些是大文件（~180MB），不进 git，由本脚本按
# SHA256 校验后拉取：
#   - bi-encoder   all-MiniLM-L6-v2 的 model.onnx（构建时 go:embed，必需）
#   - cross-encoder ms-marco-MiniLM-L-6-v2 的 model.onnx（重排时按需加载，可选）
#   - tokenizer.json  BERT WordPiece 词表（bi-encoder 与 cross-encoder 共用）
#
# 幂等：目标已存在且校验通过就跳过。校验失败或缺失才重新下载。

source_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)

command -v curl >/dev/null 2>&1 || { echo "agent-fs: curl is required" >&2; exit 1; }
command -v sha256sum >/dev/null 2>&1 || { echo "agent-fs: sha256sum is required" >&2; exit 1; }

# fetch <dest> <sha256> <url>...
# 按顺序尝试每个源，下载到临时文件并校验 SHA256，通过后原子移动到目标。
fetch() {
  local dest=$1 sha=$2
  shift 2
  if [[ -f "$dest" ]] && echo "$sha  $dest" | sha256sum --check --status 2>/dev/null; then
    echo "agent-fs: skip $(basename "$dest") (already present)"
    return 0
  fi
  mkdir -p "$(dirname "$dest")"
  local temporary
  temporary=$(mktemp)
  for url in "$@"; do
    echo "agent-fs: downloading $url"
    if curl --fail --location --retry 3 --output "$temporary" "$url"; then
      if echo "$sha  $temporary" | sha256sum --check --status 2>/dev/null; then
        mv "$temporary" "$dest"
        echo "agent-fs: verified $(basename "$dest") ($(du -h "$dest" | cut -f1))"
        return 0
      fi
      echo "agent-fs: checksum mismatch for $url, trying next source" >&2
    else
      echo "agent-fs: download failed for $url, trying next source" >&2
    fi
  done
  rm -f "$temporary"
  echo "agent-fs: failed to fetch $dest" >&2
  return 1
}

# bi-encoder：必须从 raw.githubusercontent 拉，因为 Go module proxy 只分发 Git LFS
# 指针（133 字节），而 HuggingFace 的 onnx/model.onnx 是 last_hidden_state 输出
# （无 pooling），与本库期望的 sentence_embedding 输出不匹配。raw 链接会跟随 LFS
# 重定向，返回库作者提交的原始模型（SHA256 精确匹配）。
fetch \
  "$source_root/third_party/all-minilm-l6-v2-go/all_minilm_l6_v2/model.onnx" \
  "994a58868f7abacacbf2192aa0aae8f56da8c4505dbde2740c861b24426ede6b" \
  "https://raw.githubusercontent.com/clems4ever/all-minilm-l6-v2-go/main/all_minilm_l6_v2/model.onnx" \
  "https://github.com/clems4ever/all-minilm-l6-v2-go/raw/main/all_minilm_l6_v2/model.onnx"

# cross-encoder：国内优先 hf-mirror，回退 huggingface.co。
fetch \
  "$source_root/models/cross-encoder-msmarco-MiniLM-L6-v2.onnx" \
  "5d3e70fd0c9ff14b9b5169a51e957b7a9c74897afd0a35ce4bd318150c1d4d4a" \
  "https://hf-mirror.com/cross-encoder/ms-marco-MiniLM-L-6-v2/resolve/main/onnx/model.onnx" \
  "https://huggingface.co/cross-encoder/ms-marco-MiniLM-L-6-v2/resolve/main/onnx/model.onnx"

# tokenizer：rerank.go 在包根目录 go:embed 一份（与 third_party 内的同源同内容）。
fetch \
  "$source_root/tokenizer.json" \
  "da0e79933b9ed51798a3ae27893d3c5fa4a201126cef75586296df9b4d2c62a0" \
  "https://raw.githubusercontent.com/clems4ever/all-minilm-l6-v2-go/main/all_minilm_l6_v2/tokenizer.json" \
  "https://github.com/clems4ever/all-minilm-l6-v2-go/raw/main/all_minilm_l6_v2/tokenizer.json"

cat >&2 <<'NOTE'

agent-fs: 模型就绪。另需系统级 ONNX Runtime 运行库（dlopen 加载，非模型）：
  libonnxruntime.so（>= 1.21）。放到 /usr/local/lib 并 ldconfig，或设
  AGENTFS_EMBEDDING_RUNTIME 指向其绝对路径。未安装时 embedding 回退到 hash、
  rerank 自动禁用，不影响索引功能。
NOTE

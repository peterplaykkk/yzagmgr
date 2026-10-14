#!/usr/bin/env bash
# scripts/verify/backup-real-config.sh —— 真实配置归档（给回退路径用的安全网）。
#
# 用途（doc/acceptance-steps.md §8.0）：真实替换 v1 之前打一份可逐字节还原的包：
#   scripts/verify/backup-real-config.sh              # → /tmp/real-config-backup-<ts>.tar.gz
#   scripts/verify/restore-real-config.sh <包路径>     # 回退（见 restore 脚本）
#
# 做法与「备份可用」的证明（不是声称，是当场验）：
#   1. 计算保护面每个文件的 sha256，复制进 staging 目录并写 MANIFEST.tsv；
#   2. 打 tar.gz；
#   3. **把包解开到临时目录，逐文件重算 sha256 与 MANIFEST 比对** —— 只有这一步通过，
#      才打印「备份可用」。不做第 3 步的备份脚本等于把回退押在未验证的 tar 上。
#
# 用法：
#   backup-real-config.sh [--full] [--out DIR]
#     --full   保护面扩大为 v1 数据根与 Claude 应用目录整树（数 GiB，慢）
#     --out    输出目录（默认 /tmp）
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib.sh
. "$HERE/lib.sh"
# shellcheck source=./real-config-lib.sh
. "$HERE/real-config-lib.sh"

OUT_DIR="/tmp"
while (($# > 0)); do
  case "$1" in
    --full) REAL_CONFIG_FULL=1 ;;
    --out) shift; OUT_DIR="${1:-}" ;;
    *) die "未知参数 $1（见脚本头部用法）" ;;
  esac
  shift
done

HOME_DIR="${HOME:?HOME 未设置}"
[[ -d "$OUT_DIR" ]] || die "输出目录不存在: $OUT_DIR"

STAMP="$(date +%Y%m%dT%H%M%S)"
ARCHIVE="$OUT_DIR/real-config-backup-${STAMP}.tar.gz"
STAGE="$(mktemp -d "${TMPDIR:-/tmp}/yzagmgr-backup-stage.XXXXXX")"
VERIFY_DIR="$(mktemp -d "${TMPDIR:-/tmp}/yzagmgr-backup-verify.XXXXXX")"
trap 'rm -rf "$STAGE" "$VERIFY_DIR"' EXIT

echo "scope: $(real_config_scope_label)"
echo "staging: 复制保护面文件..."

MANIFEST="$STAGE/MANIFEST.tsv"
: >"$MANIFEST"
present=0
absent=0
while IFS= read -r abs; do
  rel="${abs#"$HOME_DIR"/}"
  if [[ -f "$abs" ]]; then
    dest="$STAGE/files/$rel"
    mkdir -p "$(dirname "$dest")"
    cp -p "$abs" "$dest"
    printf 'present\t%s\t%s\t%s\n' "$(sha256_file "$abs")" "$(stat -f '%z' "$abs")" "$rel" >>"$MANIFEST"
    present=$((present + 1))
  else
    printf 'absent\t-\t-\t%s\n' "$rel" >>"$MANIFEST"
    absent=$((absent + 1))
  fi
done < <(real_config_paths "$HOME_DIR")
LC_ALL=C sort -o "$MANIFEST" "$MANIFEST"

echo "tar: 打包 $present 个文件（缺席登记 $absent 条）..."
tar -czf "$ARCHIVE" -C "$STAGE" .

echo "verify: 解开包并逐文件复核 sha256（这一步才让「备份可用」成立）..."
tar -xzf "$ARCHIVE" -C "$VERIFY_DIR"
bad=0
while IFS=$'\t' read -r status hash size rel; do
  [[ "$status" == "present" ]] || continue
  extracted="$VERIFY_DIR/files/$rel"
  if [[ ! -f "$extracted" ]]; then
    echo "  FAIL 缺文件: $rel" >&2
    bad=$((bad + 1))
    continue
  fi
  actual="$(sha256_file "$extracted")"
  if [[ "$actual" != "$hash" ]]; then
    echo "  FAIL 字节不符: ${rel}（$actual != ${hash}）" >&2
    bad=$((bad + 1))
  fi
done <"$VERIFY_DIR/MANIFEST.tsv"

if ((bad > 0)); then
  echo "备份不可用：$bad 个文件复核失败；请勿据此进入真实替换验收。" >&2
  exit 1
fi

echo "备份可用：$present 个文件逐字节复核通过（缺席登记 $absent 条）"
echo "archive: $ARCHIVE  $(stat -f '%z' "$ARCHIVE") B"
echo "回退命令: scripts/verify/restore-real-config.sh $ARCHIVE"

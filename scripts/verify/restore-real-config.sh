#!/usr/bin/env bash
# scripts/verify/restore-real-config.sh —— 从备份包回退真实配置（回退路径的最后一环）。
#
# 用途（doc/acceptance-steps.md §8.6）：
#   scripts/verify/restore-real-config.sh /tmp/real-config-backup-<ts>.tar.gz
#   scripts/verify/snapshot-real-config.sh | diff /tmp/real-before.txt -   # 期望无输出
#
# 行为（顺序即安全设计）：
#   1. **先给当前状态留证**：快照 + 调用 backup-real-config.sh 打一份「回退前的回退包」；
#   2. 解包并读 MANIFEST.tsv（备份包自带的逐文件 sha256）；
#   3. present 条目：复制回 $HOME 并逐个核对 sha256；
#   4. absent 条目：包记录「当时不存在」而如今存在 ⇒ **移入隔离仓**（不是 rm，
#      避免误删用户文件；隔离仓路径会打印出来），使盘上回到备份时的形态；
#   5. 复核：以 MANIFEST 为准逐条重算当前盘上 sha256，全部相等才判 PASS。
#
# 用法：restore-real-config.sh <tarball> [--full]
#   --full  与备份时同口径（在快照/回退包中扩大保护面）
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib.sh
. "$HERE/lib.sh"
# shellcheck source=./real-config-lib.sh
. "$HERE/real-config-lib.sh"

ARCHIVE=""
while (($# > 0)); do
  case "$1" in
    --full) REAL_CONFIG_FULL=1 ;;
    -*) die "未知参数 $1（见脚本头部用法）" ;;
    *) ARCHIVE="$1" ;;
  esac
  shift
done
[[ -n "$ARCHIVE" ]] || die "用法: restore-real-config.sh /tmp/real-config-backup-<ts>.tar.gz"
[[ -f "$ARCHIVE" ]] || die "备份包不存在: $ARCHIVE"

HOME_DIR="${HOME:?HOME 未设置}"
STAMP="$(date +%Y%m%dT%H%M%S)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/yzagmgr-restore.XXXXXX")"
QUARANTINE="${TMPDIR:-/tmp}/real-config-quarantine-${STAMP}"
trap 'rm -rf "$WORK"' EXIT

# ── 1. 回退前留证（强制，不可跳过）──────────────────────────────────────────
echo "== 回退前留证 =="
BEFORE_SNAPSHOT="${TMPDIR:-/tmp}/real-config-prerestore-${STAMP}.txt"
if ((REAL_CONFIG_FULL == 1)); then
  "$HERE/snapshot-real-config.sh" --full >"$BEFORE_SNAPSHOT"
else
  "$HERE/snapshot-real-config.sh" >"$BEFORE_SNAPSHOT"
fi
echo "current snapshot: $BEFORE_SNAPSHOT"
echo "回退前再打一份包（失败即中止，不带病往前）:"
if ((REAL_CONFIG_FULL == 1)); then
  "$HERE/backup-real-config.sh" --out "${TMPDIR:-/tmp}" --full
else
  "$HERE/backup-real-config.sh" --out "${TMPDIR:-/tmp}"
fi

# ── 2. 解包 ────────────────────────────────────────────────────────────────
tar -xzf "$ARCHIVE" -C "$WORK"
MANIFEST="$WORK/MANIFEST.tsv"
[[ -f "$MANIFEST" ]] || die "备份包内没有 MANIFEST.tsv（不是本套脚本产出的包？拒绝在无清单的情况下写真实 ${HOME}）"

restored=0 failed=0 removed=0

# ── 3/4. 逐条回写 ──────────────────────────────────────────────────────────
mkdir -p "$QUARANTINE"
while IFS=$'\t' read -r status hash size rel; do
  target="$HOME_DIR/$rel"
  if [[ "$status" == "present" ]]; then
    if ! mkdir -p "$(dirname "$target")"; then
      echo "FAIL 建目录失败: $(dirname "$target")" >&2
      failed=$((failed + 1))
      continue
    fi
    if ! cp -p "$WORK/files/$rel" "$target"; then
      echo "FAIL 写回失败: $rel" >&2
      failed=$((failed + 1))
      continue
    fi
    actual="$(sha256_file "$target")"
    if [[ "$actual" != "$hash" ]]; then
      echo "FAIL 写回后字节不符: ${rel}（$actual != ${hash}）" >&2
      failed=$((failed + 1))
      continue
    fi
    restored=$((restored + 1))
  elif [[ "$status" == "absent" ]]; then
    if [[ -e "$target" ]]; then
      # 备份时不存在、现在存在 ⇒ 移走（隔离仓可人工取回），使盘上回到备份形态。
      mkdir -p "$QUARANTINE/$(dirname "$rel")"
      mv "$target" "$QUARANTINE/$rel"
      echo "moved-to-quarantine: $rel"
      removed=$((removed + 1))
    fi
  fi
done <"$MANIFEST"

# ── 5. 复核（以 MANIFEST 为唯一判据）──────────────────────────────────────
echo "== 复核（以 MANIFEST 为唯一判据）=="
mismatch=0
total=0
while IFS=$'\t' read -r status hash size rel; do
  total=$((total + 1))
  target="$HOME_DIR/$rel"
  if [[ "$status" == "present" ]]; then
    if [[ ! -f "$target" ]]; then
      echo "FAIL 复核：缺失 $rel" >&2
      mismatch=$((mismatch + 1))
      continue
    fi
    actual="$(sha256_file "$target")"
    [[ "$actual" == "$hash" ]] || { echo "FAIL 复核：字节不符 $rel" >&2; mismatch=$((mismatch + 1)); }
  else
    [[ -e "$target" ]] && { echo "FAIL 复核：本应不存在却存在 $rel" >&2; mismatch=$((mismatch + 1)); }
  fi
done <"$MANIFEST"

echo "restored=$restored removed=$removed failed=$failed checked=$total mismatch=$mismatch"
((removed > 0)) && echo "隔离仓（可人工取回被移走的文件）: $QUARANTINE"

if ((failed > 0 || mismatch > 0)); then
  echo "verdict: FAIL —— 回退未达逐字节还原，请人工处理（回退前留证: ${BEFORE_SNAPSHOT}）" >&2
  exit 1
fi
echo "verdict: PASS —— 盘上已逐字节回到备份时形态"
echo "建议收尾核对: scripts/verify/snapshot-real-config.sh | diff $BEFORE_SNAPSHOT - （不要求为空：回退本身就会改动真实配置）"

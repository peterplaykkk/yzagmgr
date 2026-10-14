#!/usr/bin/env bash
# scripts/verify/snapshot-real-config.sh —— 真实配置快照（**只读**，唯一动作是计算 sha256）。
#
# 用途（doc/acceptance-steps.md §2.2 / §2.3 / §8.0）：
#   scripts/verify/snapshot-real-config.sh > /tmp/real-config-before.txt
#   ... 跑验收 ...
#   scripts/verify/snapshot-real-config.sh > /tmp/real-config-after.txt
#   diff /tmp/real-config-before.txt /tmp/real-config-after.txt   # 期望无输出（真实配置零触碰）
#
# 用法：
#   snapshot-real-config.sh                 # 默认保护面（见 real-config-lib.sh 头部）
#   snapshot-real-config.sh --full          # 追加 v1 数据根与 Claude 应用目录整树
#   snapshot-real-config.sh --diff FILE     # 与既有快照比对（等价 diff，退出码 0/1）
#
# 本脚本**不改**任何东西；它自身就是「保护用户真实配置」的第一道网。
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib.sh
. "$HERE/lib.sh"
# shellcheck source=./real-config-lib.sh
. "$HERE/real-config-lib.sh"

DIFF_AGAINST=""
while (($# > 0)); do
  case "$1" in
    --full) REAL_CONFIG_FULL=1 ;;
    --diff) shift; DIFF_AGAINST="${1:-}" ;;
    *) die "未知参数 $1（见脚本头部用法）" ;;
  esac
  shift
done

HOME_DIR="${HOME:?HOME 未设置}"
[[ -n "${YZAGMGR_HOME:-}" ]] && printf '# 注：环境里 YZAGMGR_HOME=%s 存在，但本脚本按**真实 $HOME** 取配置文件（保护面不随隔离根移动）\n' "$YZAGMGR_HOME" >&2

if [[ -n "$DIFF_AGAINST" ]]; then
  [[ -f "$DIFF_AGAINST" ]] || die "比对基线不存在: $DIFF_AGAINST"
  tmp="$(mktemp -t yzagmgr-snapshot.XXXXXX)"
  real_config_snapshot "$HOME_DIR" >"$tmp"
  if diff -u "$DIFF_AGAINST" "$tmp"; then
    echo "snapshot-diff: pass（与 $DIFF_AGAINST 逐字节一致）"
    rm -f "$tmp"
    exit 0
  fi
  echo "snapshot-diff: FAIL（上面是差异；真实配置已被改动）" >&2
  rm -f "$tmp"
  exit 1
fi

real_config_snapshot "$HOME_DIR"

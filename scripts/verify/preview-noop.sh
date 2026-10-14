#!/usr/bin/env bash
# scripts/verify/preview-noop.sh —— 预览零净副作用（UC-003 A2 的强化版）。
#
# 验收手册: doc/acceptance-steps.md §3.2。
#
# 前置（未就绪即 BLOCKED，退出码 2）：
#   - bin/yzagmgr；种子策略 + 令牌（见 lib.sh 的 seed_test_policy）——理由同 tier-matrix.sh：
#     门禁不联网，origin 用 example.com 保留域（形参语义已随旧中继删除停用）。
#
# 判读（v2 相对 v1 的强化点）：
#   - v1 的 preview 是「真写 → 读回 → 还原」，因此 mtime/inode 必变（recipe §F-T1），
#     只允许用「最终字节」判零副作用。v2 的 preview **完全不触盘**，所以本脚本
#     同时断言 sha256 与 mtime_ns 不变：sha256 是**主判据**，mtime 是更强的第二条
#     （未来若退化成 v1 的写-还原实现，mtime 断言会立刻红）。
#
# 证据形态（recipe §E）：绝对路径 + 前后 sha256 + 字节长度 + 计划条目数；
#   preview 还须自证「计划基于盘上写前字节」：files[].sha256Before == 盘上当前 sha256。
#
# 用法: preview-noop.sh
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib.sh
. "$HERE/lib.sh"

verify_init preview-noop
seed_test_policy
choose_isolated_ports
start_isolated_daemon
require_own_daemon

# 空字节 sha256：缺席文件的写前指纹（§F-T7 —— 不是 null，也不是空串）。
EMPTY_SHA="e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

# 九个走 provider_preview 通道的家族；workbuddy 走独立入口（单独处理）。
FAMILIES="claude claude-desktop codex gemini grokbuild opencode openclaw hermes pi"

# 受管卡 id 取自 API，不硬编码（UC-018 A5/A10）。
managed_card() {
  api GET "/tools/$1/profiles" | jget - 'next((p["id"] for p in d["data"]["items"] if p.get("kind")=="managed"), "")'
}

# 该族的受管路径（取自 API，不硬编码；只含文件，契约 §4.1）。
managed_paths() {
  api GET "/tools/$1" | jget - '"|".join(p["path"] for p in d["data"]["paths"])'
}

# meta_of <path> —— 从 file_meta 输出里取字段。
meta_pick() { printf '%s' "$2" | tr ' ' '\n' | awk -F= -v k="$1" '$1==k{print substr($0, length(k)+2)}'; }

for fam in $FAMILIES; do
  pid="$(managed_card "$fam")"
  if [[ -z "$pid" ]]; then
    record_fail "${fam} 取不到受管卡（/tools/${fam}/profiles 无 managed 项）"
    continue
  fi
  paths_raw="$(managed_paths "$fam")"
  if [[ -z "$paths_raw" ]]; then
    record_fail "${fam} /tools/${fam} 未给出任何受管路径（契约 §4.1 要求 paths 只含文件）"
    continue
  fi
  IFS='|' read -r -a fam_paths <<<"$paths_raw"

  # 前置快照：逐文件 sha256 / 字节数 / mtime，并记下「是否存在」。
  declare -a before_meta=()
  for p in "${fam_paths[@]}"; do
    before_meta+=("$(file_meta "$p")")
  done

  pv="$(api POST "/tools/${fam}/preview" "{\"profileId\":\"${pid}\"}" || true)"
  if ! printf '%s' "$pv" | grep -q '"ok":true'; then
    record_fail "${fam} preview 未成功（响应: ${pv:0:200}）"
    continue
  fi

  plan_paths="$(jget "$pv" '"|".join(f["path"] for f in d["data"]["files"])')"
  plan_n="$(jget "$pv" 'len(d["data"]["files"])')"
  op="$(jget "$pv" 'd["data"]["operation"]')"
  if [[ "$op" != "apply" ]]; then
    record_fail "${fam} preview.operation=${op}，期望 apply"
  fi

  # 计划必须覆盖该族全部受管文件（family 契约第 2 条），且逐条都在受管清单内。
  ok_plan=1
  for p in "${fam_paths[@]}"; do
    printf '%s' "$plan_paths" | grep -qF -- "$p" || ok_plan=0
  done
  if (( ok_plan == 1 )); then
    record_pass "${fam} 计划覆盖全部受管文件（${plan_n} 条）路径=${paths_raw}"
  else
    record_fail "${fam} 计划未覆盖全部受管文件：计划=${plan_paths} 受管=${paths_raw}"
  fi

  # 主判据 + 强化判据：逐文件 sha256 与 mtime_ns 双不变。
  for idx in "${!fam_paths[@]}"; do
    p="${fam_paths[$idx]}"
    before="${before_meta[$idx]}"
    after="$(file_meta "$p")"
    b_sha="$(meta_pick sha256 "$before")"; a_sha="$(meta_pick sha256 "$after")"
    b_len="$(meta_pick size "$before")"; a_len="$(meta_pick size "$after")"
    b_mt="$(meta_pick mtime_ns "$before")"; a_mt="$(meta_pick mtime_ns "$after")"
    b_ex="$(meta_pick exists "$before")"; a_ex="$(meta_pick exists "$after")"

    if [[ "$b_sha" != "$a_sha" || "$b_len" != "$a_len" ]]; then
      record_fail "${fam} preview 改了字节：${p} sha256 ${b_sha:0:12}→${a_sha:0:12} 字节 ${b_len}→${a_len}"
    elif [[ "$b_mt" != "$a_mt" ]]; then
      record_fail "${fam} preview 未改字节但动了 mtime（退化回「写-还原」实现）：${p} mtime_ns ${b_mt}→${a_mt}"
    elif [[ "$b_ex" != "$a_ex" ]]; then
      record_fail "${fam} preview 改变了文件存在性：${p} exists ${b_ex}→${a_ex}"
    else
      record_pass "${fam} 预览零净副作用 文件=${p} exists=${b_ex} sha256=${b_sha:0:12} 字节=${b_len} mtime_ns 不变"
    fi
  done

  # 计划的写前指纹必须等于盘上当前字节（缺席文件记空字节 sha256，§F-T7）。
  # 逐条比对整串而不是逐路径抓值：路径里可能含 `=` 之类字符，拼串比较没有歧义。
  # 注意 `printf '%s\n'`：行尾少一个换行会让 `while read` 丢掉最后一条（实测踩到）。
  plan_fp="$(jget "$pv" '"|".join(f["path"]+":"+f["sha256Before"] for f in d["data"]["files"])')"
  want_fp=""
  while IFS= read -r p; do
    [[ -z "$p" ]] && continue
    if [[ -f "$p" ]]; then s="$(sha256_file "$p")"; else s="$EMPTY_SHA"; fi
    want_fp="${want_fp}${p}:${s}|"
  done < <(printf '%s\n' "$plan_paths" | tr '|' '\n')
  want_fp="${want_fp%|}"
  if [[ -n "$plan_fp" && "$plan_fp" == "$want_fp" ]]; then
    record_pass "${fam} files[].sha256Before == 盘上写前 sha256（缺席按空字节 ${EMPTY_SHA:0:8}）"
  else
    record_fail "${fam} 计划的写前指纹与盘上不符：计划=${plan_fp} 盘上=${want_fp}"
  fi
done

# ── workbuddy：独立入口（不走工具通道）──────────────────────────────────────
# `/tools/workbuddy/preview` 返回 not_supported 是**正确**行为（UC-009：独立写链），
# 这里把它当断言断言掉，再用 /workbuddy/preview 的真入口做同样的零副作用判定。
wb_tool="$(api POST /tools/workbuddy/preview '{"profileId":"yzagmgr-x"}' || true)"
if [[ "$(api_error_code "$wb_tool")" == "not_supported" ]]; then
  record_pass "workbuddy /tools/workbuddy/preview 返回 not_supported（独立入口，契约 UC-009）"
else
  record_fail "workbuddy 工具通道 preview 未按契约返回 not_supported：${wb_tool:0:200}"
fi

wbfile="$RUN/.workbuddy/models.json"
wb_before="$(file_meta "$wbfile")"
wb_pv="$(api POST /workbuddy/preview '{"op":"reset"}' || true)"
wb_after="$(file_meta "$wbfile")"
if ! printf '%s' "$wb_pv" | grep -q '"ok":true'; then
  record_fail "workbuddy preview(op=reset) 未成功（响应: ${wb_pv:0:200}）"
elif [[ "$(meta_pick sha256 "$wb_before")" == "$(meta_pick sha256 "$wb_after")" \
     && "$(meta_pick mtime_ns "$wb_before")" == "$(meta_pick mtime_ns "$wb_after")" \
     && "$(meta_pick exists "$wb_before")" == "$(meta_pick exists "$wb_after")" ]]; then
  record_pass "workbuddy 预览零净副作用 文件=${wbfile} $(meta_pick exists "$wb_after") sha256=$(meta_pick sha256 "$wb_after" | cut -c1-12) mtime_ns 不变"
else
  record_fail "workbuddy preview 动了盘：${wbfile} ${wb_before} → ${wb_after}"
fi

# 预览不得留下任何「计划中的文件」（v2 的预览在内存里算目标字节）。
leftover=""
for fam in $FAMILIES; do
  paths_raw="$(managed_paths "$fam")"
  IFS='|' read -r -a fam_paths <<<"$paths_raw"
  for p in "${fam_paths[@]}"; do
    [[ -e "$p" ]] && leftover="${leftover}${p} "
  done
done
# 隔离根是全新的：除 workbuddy 之外不应有任何 live 文件被建出来。
if [[ -z "$leftover" ]]; then
  record_pass "九个家族的受管文件在「只预览不应用」后一个都不存在于盘上（零残留写入）"
else
  record_fail "预览后盘上出现受管文件（预览触盘）：${leftover}"
fi

verify_finish

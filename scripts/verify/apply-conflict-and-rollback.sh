#!/usr/bin/env bash
# scripts/verify/apply-conflict-and-rollback.sh —— 外改拒绝与失败按文件回滚。
#
# 验收手册: doc/acceptance-steps.md §3.4；陷阱原文 recipe §F-T7/T8，
#   「失败按文件回滚」独立成组见 recipe §G 断言组⑦。
#
# 前置（未就绪即 BLOCKED，退出码 2）：
#   - bin/yzagmgr；种子策略 + 令牌；macOS 的 `chflags`（注入「末条不可写」用）。
#
# 两条互不重叠的断言：
#   ① 外改拒绝（乐观并发）：preview 之后手改目标文件 → apply 必得
#      error.code=config_changed、盘上**一个字节都不写**，且操作日志按 requestId
#      记到 result=failed + errorCode=config_changed。
#   ② 失败按文件回滚：多文件计划（claude-desktop 四文件）里把**计划末条**置为
#      不可写（chflags uchg → 写入的 rename 得 EPERM）→ 事务不提交，
#      已写文件**逆序**恢复 pre-write 字节、未注入路径最终 sha256 == apply 前、
#      error.code=config_invalid、details.failedPath 指向注入的那条。
#
# 判读顺序刻意是「先断言事实，再断言枚举」：`files[].status` 的取值集合是契约硬约束
#   （§4.3：失败侧 ⊆ {restored, restore_failed, unchanged}），本脚本按契约判；实现若
#   把回滚前的 written 一并留在结果里，这里就会红——**不得为了变绿放宽**，见收尾报告。
#
# 用法: apply-conflict-and-rollback.sh
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib.sh
. "$HERE/lib.sh"

verify_init apply-conflict
seed_test_policy
choose_isolated_ports
start_isolated_daemon
require_own_daemon

require_cmd chflags "macOS 文件不可写标志（注入 EPERM 用）"

# 注入的 uchg 标志必须在任何退出路径上清掉，否则隔离根都删不掉。
INJECTED=()
clear_injected() {
  local p
  for p in "${INJECTED[@]:-}"; do
    [[ -n "$p" && -e "$p" ]] && chflags nouchg "$p" 2>/dev/null || true
  done
}
trap 'clear_injected; _verify_cleanup' EXIT

# bytes_sig <path...> —— 逐文件 sha256 拼接（逐行读，路径含空格也不切词）。
bytes_sig() {
  local out="" p
  while IFS= read -r p; do
    [[ -z "$p" ]] && continue
    if [[ -f "$p" ]]; then out="${out}${p}=$(sha256_file "$p")|"; else out="${out}${p}=<absent>|"; fi
  done < <(printf '%s\n' "$@" | LC_ALL=C sort)
  printf '%s' "${out%|}"
}

managed_card() { api GET "/tools/$1/profiles" | jget - 'next((p["id"] for p in d["data"]["items"] if p.get("kind")=="managed"), "")'; }
fam_paths() { api GET "/tools/$1" | jget - '"|".join(p["path"] for p in d["data"]["paths"])'; }

# apply_preview <fam> <previewId> —— 返回 apply 响应体（成功与失败都返回，由调用方判定）。
do_apply() { api POST "/tools/$1/apply" "{\"previewId\":\"$2\"}" || true; }

# op_log_for <requestId> —— 操作日志里该 requestId 的那一条。
op_log_for() { api GET "/operations?requestId=$1" | jget - 'json.dumps(d["data"]["items"][0], ensure_ascii=False, sort_keys=True) if d["data"]["items"] else ""'; }

# ══ ① 外改拒绝（乐观并发指纹复核）══════════════════════════════════════════
FAM1=claude
pid1="$(managed_card "$FAM1")"
[[ -n "$pid1" ]] || die "取不到 ${FAM1} 的受管卡（/tools/${FAM1}/profiles 无 managed 项）"
read -r -a p1 <<<"$(fam_paths "$FAM1" | tr '|' ' ')"
target1="${p1[0]}"

# 让目标文件先存在（否则「外改」会变成「新建」，测不到指纹复核）。
mkdir -p "$(dirname "$target1")"
printf '{"env":{"USER_OWN":"keep-me"}}\n' >"$target1"

pv1="$(api POST "/tools/${FAM1}/preview" "{\"profileId\":\"${pid1}\"}")"
pvid1="$(jget "$pv1" 'd["data"]["previewId"]')"
[[ -n "$pvid1" ]] || die "preview 未返回 previewId：${pv1:0:200}"

# 外部手改（模拟用户/另一个进程在预览之后改了同一个文件）。
printf '{"env":{"USER_OWN":"externally-changed"}}\n' >"$target1"
extern_sha="$(sha256_file "$target1")"

ap1="$(do_apply "$FAM1" "$pvid1")"
code1="$(api_error_code "$ap1")"
if [[ "$code1" == "config_changed" ]]; then
  record_pass "${FAM1} 外改后 apply 返回 config_changed（路径=${target1}）"
else
  record_fail "${FAM1} 外改后 apply 的 error.code=${code1:-<无>}，期望 config_changed（响应: ${ap1:0:240}）"
fi

det_path="$(jget "$ap1" 'str((d.get("error") or {}).get("details",{}).get("path",""))')"
if [[ "$det_path" == "$target1" ]]; then
  record_pass "${FAM1} details.path 精确指向被外改的文件（${det_path}）"
else
  record_fail "${FAM1} details.path=${det_path:-<无>}，期望 ${target1}"
fi

now_sha="$(sha256_file "$target1")"
if [[ "$now_sha" == "$extern_sha" ]]; then
  record_pass "${FAM1} 拒绝后盘上未被写：sha256=$(printf '%s' "$now_sha" | cut -c1-12)（== 外改后的字节）"
else
  record_fail "${FAM1} 拒绝后盘上被改：期望 ${extern_sha:0:12} 实际 ${now_sha:0:12}"
fi

if printf '%s' "$ap1" | grep -q '"ok":false' && ! printf '%s' "$ap1" | grep -q '"data"'; then
  record_pass "${FAM1} 失败信封形状正确（ok:false 且不带 data）"
else
  record_fail "${FAM1} 失败信封形状不符（失败响应不得带 data）：${ap1:0:200}"
fi

req1="$(jget "$ap1" 'd.get("requestId","")')"
log1="$(op_log_for "$req1")"
if [[ "$(jget "$log1" 'str(d.get("result",""))')" == "failed" && "$(jget "$log1" 'str(d.get("errorCode",""))')" == "config_changed" ]]; then
  record_pass "${FAM1} 操作日志按 requestId=${req1} 记到 result=failed + errorCode=config_changed"
else
  record_fail "${FAM1} 操作日志未如实记录失败：requestId=${req1} 记录=${log1:0:240}"
fi

# ══ ② 失败按文件回滚（多文件计划末条不可写）═══════════════════════════════
FAM2=claude-desktop
pid2="$(managed_card "$FAM2")"
[[ -n "$pid2" ]] || die "取不到 ${FAM2} 的受管卡"

# 先取计划形状（这一步的 preview 只为拿路径清单，其租约会被下面的正式预览取代）。
pv_shape="$(api POST "/tools/${FAM2}/preview" "{\"profileId\":\"${pid2}\"}")"
plan_list="$(jget "$pv_shape" '"|".join(f["path"] for f in d["data"]["files"])')"
IFS='|' read -r -a plan2 <<<"$plan_list"
if ((${#plan2[@]} < 2)); then
  die "${FAM2} 的计划只有 ${#plan2[@]} 个文件，构造不出「多文件计划」（计划=${plan_list}）"
fi
last2="${plan2[$((${#plan2[@]} - 1))]}"
if [[ -e "$last2" ]]; then
  die "注入点 ${last2} 已存在（隔离根本该是全新的），请检查夹具污染"
fi

# 夹具必须建在**正式预览之前**：预览把「写前字节」记成指纹，若预置发生在预览之后，
# apply 会在指纹复核那一步就以 config_changed 中止，测到的是 ① 而不是 ②（实测踩到）。
mkdir -p "$(dirname "$last2")"
printf '{"mcpServers":{}}\n' >"$last2"
pre_sig="$(bytes_sig "${plan2[@]}")"

pv2="$(api POST "/tools/${FAM2}/preview" "{\"profileId\":\"${pid2}\"}")"
pvid2="$(jget "$pv2" 'd["data"]["previewId"]')"
[[ -n "$pvid2" ]] || die "preview 未返回 previewId：${pv2:0:200}"

# 注入「末条不可写」：uchg 让写入那一步的 rename 得 EPERM（不改字节，故不触发指纹复核）。
chflags uchg "$last2"
INJECTED+=("$last2")
if [[ "$(stat -f %Sf "$last2" 2>/dev/null || true)" == *uchg* ]]; then
  record_pass "${FAM2} 注入末条不可写成功：chflags uchg ${last2}（计划顺序末条）"
else
  # 注入不成立 ⇒ 前置不成立，不得把「没测到」记成 pass。
  die "chflags uchg 未生效于 ${last2}，无法构造「末条不可写」场景"
fi

ap2="$(do_apply "$FAM2" "$pvid2")"
code2="$(api_error_code "$ap2")"
if [[ "$code2" == "config_invalid" ]]; then
  record_pass "${FAM2} 末条写失败后事务以 config_invalid 失败（不是冒充成功）"
else
  record_fail "${FAM2} 末条写失败后 error.code=${code2:-<无>}，期望 config_invalid（响应: ${ap2:0:240}）"
fi

failed_path="$(jget "$ap2" 'str((d.get("error") or {}).get("details",{}).get("failedPath",""))')"
if [[ "$failed_path" == "$last2" ]]; then
  record_pass "${FAM2} details.failedPath 指向注入的那一条（${last2}）"
else
  record_fail "${FAM2} details.failedPath=${failed_path:-<无>}，期望 ${last2}"
fi

# 注意不要套 str()：jget 对标量 bool 输出小写 true/false，str(True) 会变成 "True"（实测踩到）。
restored_all="$(jget "$ap2" '(d.get("error") or {}).get("details",{}).get("restoredAll","")')"
if [[ "$restored_all" == "true" ]]; then
  record_pass "${FAM2} details.restoredAll=true（回滚未报 restore_failed）"
else
  record_fail "${FAM2} details.restoredAll=${restored_all:-<无>}，期望 true"
fi

# 逐文件状态集合：契约 §4.3 规定失败侧 ⊆ {restored, restore_failed, unchanged}。
statuses="$(jget "$ap2" '"|".join(f["status"] for f in ((d.get("error") or {}).get("details",{}) or {}).get("files",[]))')"
bad_status=""
IFS='|' read -r -a st_arr <<<"$statuses"
for st in "${st_arr[@]}"; do
  case "$st" in
    restored|restore_failed|unchanged) ;;
    "") ;;
    *) bad_status="${bad_status}${st} " ;;
  esac
done
if [[ -z "$bad_status" && -n "$statuses" ]]; then
  record_pass "${FAM2} error.details.files[].status 全落在 {restored,restore_failed,unchanged} 内（实际: ${statuses}）"
else
  record_fail "${FAM2} error.details.files[].status 越界（越界值: ${bad_status:-<无条目>}；实际: ${statuses:-<空>}）——契约 §4.3 失败侧只允许 restored/restore_failed/unchanged"
fi

# files[] 形状：按 lease（= 预览计划）顺序、每路径恰好一条。
# （旧实现会把「写入阶段的 written」与「回滚阶段的 restored」并排返回、同一路径出现两次且
#   状态互相矛盾；现已改为只报回滚终态。这里同时守住「顺序 + 唯一性」两条。）
files_seq="$(jget "$ap2" '"|".join(f["path"] for f in ((d.get("error") or {}).get("details",{}) or {}).get("files",[]))')"
files_n="$(jget "$ap2" 'len(((d.get("error") or {}).get("details",{}) or {}).get("files",[]))')"
uniq_n="$(jget "$ap2" 'len({f["path"] for f in ((d.get("error") or {}).get("details",{}) or {}).get("files",[])})')"
if [[ "$files_seq" == "$plan_list" && "$uniq_n" == "$files_n" && "$files_n" == "${#plan2[@]}" ]]; then
  record_pass "${FAM2} error.details.files[] 按计划顺序、每路径恰一条（${files_n} 条 == 计划 ${#plan2[@]} 条）"
else
  record_fail "${FAM2} files[] 形状不符：条数=${files_n}（计划 ${#plan2[@]}）唯一路径数=${uniq_n} 顺序=${files_seq}"
fi

# 逆序恢复：契约把「恢复次序」显式暴露为 details.recoveryOrder ——
# 改成每路径一条之后，它无法再从 files[] 反推，故必须直接核对。
# 期望 = 「计划顺序去掉失败路径」的逆序（失败的那条从未写入，不参与恢复）。
recovery_seq="$(jget "$ap2" '"|".join(((d.get("error") or {}).get("details",{}) or {}).get("recoveryOrder",[]))')"
want_rev=""
for ((i = ${#plan2[@]} - 1; i >= 0; i--)); do
  [[ "${plan2[$i]}" == "$last2" ]] && continue
  want_rev="${want_rev}${plan2[$i]}|"
done
want_rev="${want_rev%|}"
if [[ -n "$recovery_seq" && "$recovery_seq" == "$want_rev" ]]; then
  record_pass "${FAM2} details.recoveryOrder == 写入集合的逆序（写序=${plan_list} 恢复序=${recovery_seq}）"
else
  record_fail "${FAM2} recoveryOrder 不是写入逆序：实际=${recovery_seq:-<无>} 期望=${want_rev:-<无>}（写序=${plan_list}）"
fi

# 未注入路径最终字节 == apply 前（末条本身保持我们预置的夹具字节）。
post_sig="$(bytes_sig "${plan2[@]}")"
if [[ "$post_sig" == "$pre_sig" ]]; then
  record_pass "${FAM2} 回滚后全部受管路径逐字节 == apply 前（$(printf '%s' "$pre_sig" | shasum -a 256 | cut -c1-12)）"
else
  record_fail "${FAM2} 回滚后盘上不等于 apply 前：前=${pre_sig} 后=${post_sig}"
fi

req2="$(jget "$ap2" 'd.get("requestId","")')"
log2="$(op_log_for "$req2")"
if [[ "$(jget "$log2" 'str(d.get("result",""))')" == "failed" && "$(jget "$log2" 'str(d.get("errorCode",""))')" == "config_invalid" ]]; then
  record_pass "${FAM2} 操作日志按 requestId=${req2} 记到 result=failed + errorCode=config_invalid"
else
  record_fail "${FAM2} 操作日志未如实记录失败：requestId=${req2} 记录=${log2:0:240}"
fi

verify_finish

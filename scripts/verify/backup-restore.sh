#!/usr/bin/env bash
# scripts/verify/backup-restore.sh —— 备份与恢复语义（初始快照不朽 / 删除语义 / 幂等 / 保留上限）。
#
# 验收手册: doc/acceptance-steps.md §3.5；契约 §4.3（恢复与 delete 语义）+ §5（保留策略）。
#
# 前置（未就绪即 BLOCKED，退出码 2）：bin/yzagmgr；种子策略 + 令牌。
#
# 判读要点：
#   - **初始快照**（接管前字节，reason=initial）是「恢复删除语义」的唯一依据：
#     它记 existed=false 的路径，恢复时会被删除（files[].status=deleted）。
#   - **恢复幂等**的判据是「事务 status=unchanged + 盘上逐字节不变」，
#     而不是「没报错」。
#   - **保留上限**同时看两侧：默认 keep=50（GET /backups/settings）、
#     超限后最旧的普通备份被淘汰、而初始快照**永不淘汰**（把它降到 keep=3 也仍在）。
#
# 用法: backup-restore.sh
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib.sh
. "$HERE/lib.sh"

verify_init backup-restore
seed_test_policy
choose_isolated_ports
start_isolated_daemon
require_own_daemon

FAM=pi   # 两个受管文件：models.json（用户夹具先存在）+ settings.json（接管时新建）

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

# backups_json <fam> <limit> —— 备份列表原文。
backups_json() { api GET "/tools/$1/backups?limit=$2"; }
# backup_count <fam> <limit> <extra-python-filter> —— 计数。
backup_count() { backups_json "$1" "$2" | jget - "sum(1 for b in d['data']['items'] if $3)"; }

apply_card() {
  local fam="$1" pid="$2" pv pvid ap
  pv="$(api POST "/tools/${fam}/preview" "{\"profileId\":\"${pid}\"}")"
  pvid="$(jget "$pv" 'd["data"]["previewId"]')"
  [[ -n "$pvid" ]] || die "${fam} preview 未返回 previewId：${pv:0:200}"
  ap="$(api POST "/tools/${fam}/apply" "{\"previewId\":\"${pvid}\"}")"
  printf '%s' "$ap" | grep -q '"ok":true' || die "${fam} apply 失败：${ap:0:240}"
  printf '%s' "$ap"
}

restore_backup() {
  local fam="$1" bk="$2" pv pvid ap
  pv="$(api POST "/tools/${fam}/restore/preview" "{\"backupId\":\"${bk}\"}")"
  pvid="$(jget "$pv" 'd["data"]["previewId"]')"
  [[ -n "$pvid" ]] || die "${fam} restore/preview 未返回 previewId：${pv:0:200}"
  ap="$(api POST "/tools/${fam}/restore/apply" "{\"previewId\":\"${pvid}\"}")"
  printf '%s' "$ap" | grep -q '"ok":true' || die "${fam} restore/apply 失败：${ap:0:240}"
  printf '%s' "$ap"
}

mpid="$(managed_card "$FAM")"
[[ -n "$mpid" ]] || die "取不到 ${FAM} 的受管卡"
paths_raw="$(fam_paths "$FAM")"
IFS='|' read -r -a plist <<<"$paths_raw"
user_file="${plist[0]}"
new_file="${plist[1]}"
[[ -n "$user_file" && -n "$new_file" ]] || die "${FAM} 受管路径不足两个：${paths_raw}"

# ── 夹具：用户自有内容先存在于 user_file，new_file 缺席 ────────────────────
mkdir -p "$(dirname "$user_file")"
printf '{\n  "providers": {\n    "user-owned": {\n      "models": [ { "id": "user-model", "baseUrl": "https://user.example/v1" } ]\n    }\n  }\n}\n' >"$user_file"
user_sha="$(sha256_file "$user_file")"
[[ -e "$new_file" ]] && die "夹具前置不成立：${new_file} 本该缺席（隔离根本该是全新的）"

# ── 接管（第一次 apply 会顺带记录初始快照）────────────────────────────────
ap="$(apply_card "$FAM" "$mpid")"
if [[ -f "$new_file" ]]; then
  record_pass "${FAM} 接管后新建了 ${new_file}（sha256=$(sha256_file "$new_file" | cut -c1-12)）"
else
  record_fail "${FAM} 接管后 ${new_file} 不存在（受管写入未覆盖该路径）"
fi

init_id="$(backups_json "$FAM" 50 | jget - 'next((b["backupId"] for b in d["data"]["items"] if b.get("initial")), "")')"
[[ -n "$init_id" ]] || die "备份列表里没有初始快照（initial:true）"
init_dump="$(backups_json "$FAM" 50 | jget - 'json.dumps(next(b for b in d["data"]["items"] if b.get("initial")), ensure_ascii=False, sort_keys=True)')"
init_reason="$(jget "$init_dump" 'd.get("reason","")')"
init_user_existed="$(jget "$init_dump" "next((f['existed'] for f in d['files'] if f['path']=='''$user_file'''), 'kein')")"
init_new_existed="$(jget "$init_dump" "next((f['existed'] for f in d['files'] if f['path']=='''$new_file'''), 'kein')")"
if [[ "$init_reason" == "initial" && "$init_user_existed" == "true" && "$init_new_existed" == "false" ]]; then
  record_pass "${FAM} 初始快照记录接管前字节：reason=initial backupId=${init_id}（用户文件 existed=true / 接管新建 existed=false）"
else
  record_fail "${FAM} 初始快照的形状不符：reason=${init_reason} 用户文件existed=${init_user_existed} 新文件existed=${init_new_existed}（${init_dump:0:240}）"
fi

# 契约 §4.1：createdByUs 由初始快照推出（接管时建的路径恢复后将被删除）。
cb_user="$(api GET "/tools/$FAM" | jget - "next((p['createdByUs'] for p in d['data']['paths'] if p['path']=='''$user_file'''), 'kein')")"
cb_new="$(api GET "/tools/$FAM" | jget - "next((p['createdByUs'] for p in d['data']['paths'] if p['path']=='''$new_file'''), 'kein')")"
if [[ "$cb_user" == "false" && "$cb_new" == "true" ]]; then
  record_pass "${FAM} /tools/${FAM} 的 createdByUs 与初始快照一致（用户文件 false / 接管新建 true）"
else
  record_fail "${FAM} createdByUs 不符：用户文件=${cb_user} 接管新建=${cb_new}"
fi

# ── 恢复：预览形状 + 应用后的字节与删除语义 ───────────────────────────────
rpv="$(api POST "/tools/${FAM}/restore/preview" "{\"backupId\":\"${init_id}\"}")"
rpvid="$(jget "$rpv" 'd["data"]["previewId"]')"
[[ -n "$rpvid" ]] || die "restore/preview 未返回 previewId：${rpv:0:240}"
rp_op="$(jget "$rpv" 'd["data"]["operation"]')"
del_flag="$(jget "$rpv" "next((f['delete'] for f in d['data']['files'] if f['path']=='''$new_file'''), 'kein')")"
warn_txt="$(jget "$rpv" "next((f.get('warning','') for f in d['data']['files'] if f['path']=='''$new_file'''), '')")"
if [[ "$rp_op" == "restore" && "$del_flag" == "true" ]]; then
  record_pass "${FAM} 恢复预览标记了删除：operation=restore delete=true 路径=${new_file} warning=${warn_txt:-<无>}"
else
  record_fail "${FAM} 恢复预览未标出删除语义：operation=${rp_op} delete=${del_flag}（响应: ${rpv:0:240}）"
fi

rap="$(api POST "/tools/${FAM}/restore/apply" "{\"previewId\":\"${rpvid}\"}")"
st_user="$(jget "$rap" "next((f['status'] for f in d['data']['files'] if f['path']=='''$user_file'''), 'kein')")"
st_new="$(jget "$rap" "next((f['status'] for f in d['data']['files'] if f['path']=='''$new_file'''), 'kein')")"
if [[ "$st_user" == "written" && "$st_new" == "deleted" ]]; then
  record_pass "${FAM} 恢复逐文件状态正确（用户文件 written / 接管新建 deleted）"
else
  record_fail "${FAM} 恢复逐文件状态不符：用户文件=${st_user} 接管新建=${st_new}（响应: ${rap:0:240}）"
fi

now_user_sha="$(sha256_file "$user_file")"
if [[ "$now_user_sha" == "$user_sha" ]]; then
  record_pass "${FAM} 恢复写回原字节：${user_file} sha256=$(printf '%s' "$user_sha" | cut -c1-12)（== 接管前）"
else
  record_fail "${FAM} 恢复未写回原字节：接管前=${user_sha:0:12} 恢复后=${now_user_sha:0:12}"
fi
if [[ -e "$new_file" ]]; then
  record_fail "${FAM} 恢复未删除接管时创建的路径：${new_file} 仍在（字节 ${now_user_sha:0:8}…）"
else
  record_pass "${FAM} 恢复删除了接管时创建的路径：${new_file} 已不在盘上"
fi

# ── 恢复幂等：再恢复一次必须是无操作 ──────────────────────────────────────
after_restore_sig="$(bytes_sig "${plist[@]}")"
rap2="$(restore_backup "$FAM" "$init_id")"
st2="$(jget "$rap2" 'd["data"]["status"]')"
sig2="$(bytes_sig "${plist[@]}")"
if [[ "$st2" == "unchanged" && "$sig2" == "$after_restore_sig" ]]; then
  record_pass "${FAM} 恢复幂等：二次恢复 status=unchanged 且盘上逐字节不变（$(printf '%s' "$sig2" | shasum -a 256 | cut -c1-12)）"
else
  record_fail "${FAM} 二次恢复不是无操作：status=${st2} 盘上 前=${after_restore_sig} 后=${sig2}"
fi

# ── 无效备份编号：必须响亮失败，不得当成功 ────────────────────────────────
bad="$(api POST "/tools/${FAM}/restore/preview" '{"backupId":"bk-does-not-exist"}' || true)"
bad_code="$(api_error_code "$bad")"
if [[ "$(jget "$bad" 'd.get("ok")')" == "false" && "$bad_code" =~ ^(backup_unreadable|not_found)$ ]]; then
  record_pass "${FAM} 未知 backupId 被拒：error.code=${bad_code}"
else
  record_fail "${FAM} 未知 backupId 未按契约被拒：${bad:0:200}"
fi

# ── 保留策略 ──────────────────────────────────────────────────────────────
keep0="$(api GET /backups/settings | jget - 'd["data"]["keep"]')"
if [[ "$keep0" == "50" ]]; then
  record_pass "保留上限默认值 keep=50（GET /backups/settings）"
else
  record_fail "保留上限默认值=${keep0}，期望 50"
fi

# 造出超过上限的普通备份：每次 apply 记一条（同态 apply 也记，见 recipe §F-T6）。
before_total="$(backup_count "$FAM" 200 'True')"
before_normal="$(backup_count "$FAM" 200 'not b.get("initial")')"
oldest_id="$(backups_json "$FAM" 200 | jget - 'min((b["backupId"] for b in d["data"]["items"] if not b.get("initial")), default="")')"
target_normal=$((keep0))
for ((i = 0; i < 52; i++)); do
  apply_card "$FAM" "$mpid" >/dev/null
done
after_total="$(backup_count "$FAM" 200 'True')"
after_normal="$(backup_count "$FAM" 200 'not b.get("initial")')"
if [[ "$after_normal" == "$target_normal" ]]; then
  record_pass "${FAM} 普通备份按 keep=${keep0} 封顶：普通 ${before_normal}→${after_normal}（+初始 1 条 ⇒ 合计 ${after_total}）"
else
  record_fail "${FAM} 普通备份数=${after_normal}，期望 ${target_normal}（合计 ${after_total}，造数前 ${before_total}）"
fi
init_still="$(backups_json "$FAM" 200 | jget - 'str(any(b.get("initial") for b in d["data"]["items"]))')"
if [[ "$init_still" == "True" && -n "$oldest_id" ]]; then
  gone="$(backups_json "$FAM" 200 | jget - "str(any(b['backupId']=='$oldest_id' for b in d['data']['items']))")"
  if [[ "$gone" == "False" ]]; then
    record_pass "${FAM} 淘汰按最旧优先（超限后 ${oldest_id} 已不在列表）且初始快照仍在列表里（永不淘汰）"
  else
    record_fail "${FAM} 超限后最旧的普通备份 ${oldest_id} 仍在列表（淘汰未生效）"
  fi
else
  record_fail "${FAM} 超限后初始快照丢失（initial_still=${init_still}）——初始快照必须永不淘汰"
fi

# 把 keep 压到 3：初始快照必须活下来（比「不淘汰」更强的断言）。
api PUT /backups/settings '{"keep":3}' >/dev/null
keep3="$(api GET /backups/settings | jget - 'd["data"]["keep"]')"
n3="$(backup_count "$FAM" 200 'not b.get("initial")')"
i3="$(backups_json "$FAM" 200 | jget - 'str(any(b.get("initial") for b in d["data"]["items"]))')"
if [[ "$keep3" == "3" && "$n3" == "3" && "$i3" == "True" ]]; then
  record_pass "PUT keep=3 立即生效（普通备份收敛到 3 条）且初始快照仍在（${FAM}）"
else
  record_fail "PUT keep=3 未按契约生效：keep=${keep3} 普通=${n3} 初始仍在=${i3}"
fi

# 越界值必须被拒（契约 §5：下限 1、上限 1000）。
lo="$(api PUT /backups/settings '{"keep":0}' || true)"
hi="$(api PUT /backups/settings '{"keep":1001}' || true)"
if [[ "$(api_error_code "$lo")" == "invalid_request" && "$(api_error_code "$hi")" == "invalid_request" ]]; then
  record_pass "keep 越界（0 / 1001）均返回 invalid_request"
else
  record_fail "keep 越界未按契约被拒：0⇒${lo:0:120} 1001⇒${hi:0:120}"
fi

api PUT /backups/settings '{"keep":50}' >/dev/null
keep_back="$(api GET /backups/settings | jget - 'd["data"]["keep"]')"
if [[ "$keep_back" == "50" ]]; then
  record_pass "PUT keep=50 回读一致（收尾恢复默认值）"
else
  record_fail "PUT keep=50 回读=${keep_back}，期望 50"
fi

verify_finish

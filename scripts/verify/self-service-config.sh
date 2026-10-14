#!/usr/bin/env bash
# scripts/verify/self-service-config.sh —— UC-032 cli/web 自建配置管理（独立 provider 形态）。
#
# 自建卡承载**三要素**：端点、凭据引用、模型清单；建/改/复制/删四动作经 cli 与 web
# **同接口**（同一 REST）完成且结果一致（含复制加后缀、不改当前生效项）；凭据只回
# 引用不回明文；删除保护（当前生效的自建卡 / 受管 / 官方卡不可删）；游客态可用面与
# 被拒面如实登记（与 UC-033 口径一致，不折 pass）。
#
# ── 本脚本冻结的可执行契约（能力未交付 ⇒ 全部如实红；实现方按此对齐后本脚本转绿）──
# 三要素承载（对 api-v1.md §4.2 Profile 的 **additive 扩展**；kind=user 卡专属）：
#   POST /tools/{id}/profiles
#     {name, from?, endpoint?, cred?|credRef?, models?:[id,…]}
#   PUT  /tools/{id}/profiles/{pid}
#     {name?, endpoint?, cred?|credRef?, models?, modelId?}
#   回读 Profile 增两键：`endpoint`（串）、`credRef`（串）；`models` 沿既有键（模型清单）。
#   - `cred` 为**一次性明文**（入托管凭据库，卡只落引用）；`credRef` 引用既有托管凭据
#     （yzagmgr-managed-token://<uid>，UC-018 A12 形态）；
#   - **响应恒只回 credRef，绝不回明文**（UC-018 A12 同口径）；
#   - 非自建卡（managed/official）不携带 endpoint/credRef（键缺席，不伪造空值）。
# CLI（UC-031 平权同一 REST；cred 明文只经 0600 文件交付，不进 argv/日志）：
#   yzagmgr profiles create   <tool> --name <n> [--endpoint <url>] [--cred-file <0600文件>] \
#                           [--models a,b,c] [--from <pid>] --json
#   yzagmgr profiles update   <tool> <pid> [--name <n>] [--endpoint <url>] [--cred-file <0600文件>] \
#                           [--models a,b,c] [--model-id <m>] --json
#   yzagmgr profiles list|delete|duplicate …（见 cli-parity.sh 冻结面）
# 复制语义（UC-018 A9）：名称加后缀（新名 ≠ 原名且以原名为前缀）、不改当前生效项
#   （activeProfileId 与 live 字节均不变）。
# 删除保护（api-v1.md §4.2 / UC-018 A3/A11）：删除当前生效的自建卡 ⇒ `invalid_request`
#   且 live 配置字节保住；受管卡与官方卡不可删（同 `invalid_request`）。
# 游客态口径（本脚本登记冻结面；与 UC-033 对齐，见 A4 段张力登记）：
#   自建配置管理是**本地能力**（admin 令牌即可，免登录可用——R122 ⑤ 口径）：
#   游客（session=anonymous）可用到「建/改/复制/删 + 三要素回读」全流程；
#   **被拒在启用链的凭据步**：凭据引用无法解析 ⇒ 显式报缺凭据，盘上零变化、无半写态
#   （UC-032 边界同口径）；需网关凭据的预览/应用与网关策略下发属 UC-033 A2 被拒面
#   （unauthenticated），本段只登记不越界重测。
#
# 现状红点（2026-09-24 覆盖审计 D4/D5，先红后绿）：
#   ① 自建卡仅 name+modelId（POST {name,from?} / PUT {name?,modelId?,settings?}）——
#      三要素（endpoint/credRef/models）承载形态待建；
#   ② CLI 缺 profiles 子命令族（cli-parity.sh 红点同源）；
#   ③ 游客态可用面/被拒面无登记载体。
#   本脚本在此状态下真跑并逐断言如实红（verdict FAIL，退出码 1），不得 skip/todo 冒充。
#
# 断言 ↔ UC-032 验收行：
#   TC-UC032-A1 独立配置三要素（endpoint/credRef/models 逐字段回读 + 凭据不回显明文）
#   TC-UC032-A2 双入口建改删复制一致（四动作 cli 与 web 同 REST、复制加后缀不切当前）
#   TC-UC032-A3 删除保护（当前生效自建卡被拒 + live 保住；受管/官方卡不可删）
#   TC-UC032-A4 游客态可用面登记（可用面=全流程本地能力；被拒面=启用链凭据步，零变化；
#               PW 握手=guest.pw.ts JUnit ↔ test-map UC-033 块逐名对账；游客态自建卡写
#               张力登记 unknown 不折）
#
# 用法: self-service-config.sh
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib.sh
. "$HERE/lib.sh"

unset ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN ANTHROPIC_BASE_URL ANTHROPIC_MODEL 2>/dev/null || true

verify_init self-service-config
seed_test_policy "https://gateway.example.com"
export YZAGMGR_TEST_VERSION_PROBE_FAMILIES=""
start_isolated_daemon

YZAGMGR="$ROOT/bin/yzagmgr"
[[ -x "$YZAGMGR" ]] || die "bin/yzagmgr 不存在（CLI 未构建）。先执行 make build。"
require_python

EV="$RUN/evidence"
mkdir -p "$EV"
EVN=0
CLI_LAST=""
API_LAST=""

_keep_first_fail() {
  if [[ ! -e "$EV/.first-fail-recorded" ]]; then
    printf '首败: %s\n期望=%s\n实际=%s\n证据=%s\n' "$1" "$2" "$3" "$4" >"$EV/first-fail.txt"
    : >"$EV/.first-fail-recorded"
  fi
}
pass_ev() { record_pass "$1"; }
fail_ev() {
  _keep_first_fail "$1" "$2" "$3" "$4"
  record_fail "${1}｜期望=${2}｜实际=${3}｜证据=${4}"
}

cli_run() {
  local label="$1"; shift
  EVN=$((EVN + 1))
  printf -v CLI_LAST '%02d-%s' "$EVN" "$label"
  printf 'yzagmgr %s\n' "$*" >>"$EV/commands.log"
  local rc=0
  run_capped 30 "$EV/${CLI_LAST}.out" "$EV/${CLI_LAST}.err" env \
    YZAGMGR_HOME="$RUN" YZAGMGR_API_PORT="$API_PORT" YZAGMGR_TEST_VERSION_PROBE_FAMILIES= \
    "$YZAGMGR" "$@" || rc=$?
  return $rc
}
cli_out() {
  { head -c 200 "$EV/${CLI_LAST}.err" 2>/dev/null; printf ' ‖ '; head -c 200 "$EV/${CLI_LAST}.out" 2>/dev/null; } \
    | tr '\n' ' ' || true
}
cli_out_full() { cat "$EV/${CLI_LAST}.out" 2>/dev/null || true; }
cli_jget() { jget "$(cli_out_full)" "$1"; }

api_ev() {
  local label="$1" method="$2" path="$3" body="${4:-}"
  EVN=$((EVN + 1))
  printf -v API_LAST '%02d-%s' "$EVN" "$label"
  if [[ -n "$body" ]]; then
    api "$method" "$path" "$body" >"$EV/${API_LAST}.out" 2>"$EV/${API_LAST}.err" || true
  else
    api "$method" "$path" >"$EV/${API_LAST}.out" 2>"$EV/${API_LAST}.err" || true
  fi
}
api_body() { cat "$EV/${API_LAST}.out" 2>/dev/null || true; }
api_jget() { jget "$(api_body)" "$1"; }

bytes_sig() {
  local out="" p
  while IFS= read -r p; do
    [[ -z "$p" ]] && continue
    if [[ -f "$p" ]]; then out="${out}${p}=$(sha256_file "$p")|"; else out="${out}${p}=<absent>|"; fi
  done < <(printf '%s\n' "$@" | LC_ALL=C sort)
  printf '%s' "${out%|}"
}

new_token() {
  python3 -c 'import secrets, sys, os
p = sys.argv[1]
open(p, "w").write("yzagmgr-test-" + secrets.token_hex(16) + "\n")
os.chmod(p, 0o600)' "$1"
}
leaked_sentinel() { # leaked_sentinel <token-file>…：打印含哨兵明文的证据/日志文件清单
  local f tok
  {
    for f in "$@"; do
      tok="$(tr -d '\r\n' <"$f")"
      grep -rlF -- "$tok" "$EV" "$RUN/daemon.log" "$RUN"/daemon.log.* 2>/dev/null || true
    done
  } | LC_ALL=C sort -u | paste -sd, - || true
}

TOOL=opencode
cred_a="$RUN/cred-ss-a.token"; new_token "$cred_a"
cred_b="$RUN/cred-ss-b.token"; new_token "$cred_b"

# live 路径（删除保护段的「live 配置保住」判据）。
rc=0; cli_run "tools-show" tools show "$TOOL" --json || rc=$?
paths_raw="$(cli_jget '"|".join(p["path"] for p in d["data"]["tool"]["paths"])')"
[[ "$rc" == "0" && -n "$paths_raw" ]] || die "cli tools show ${TOOL} 未给出受管路径（rc=${rc}）：$(cli_out)"
IFS='|' read -r -a LIVE_PATHS <<<"$paths_raw"

# ── TC-UC032-A1（正·独立配置三要素）──────────────────────────────────────────
# 自建卡承载端点、凭据引用、模型清单并逐字段回读；凭据不回显明文（UC-018 A12）。

rc=0; cli_run "a1-create" profiles create "$TOOL" --name selfsvc-a \
  --endpoint "https://selfsvc-a.example/v1" --cred-file "$cred_a" --models "m-a,m-b" --json || rc=$?
cid="$(cli_jget 'str(d.get("data",{}).get("id",""))')"
if [[ "$rc" == "0" && -n "$cid" ]]; then
  pass_ev "TC-UC032-A1[建卡] CLI 建自建卡成功（id=${cid}）"
else
  fail_ev "TC-UC032-A1[建卡] 自建卡三要素承载" \
    "rc=0 且 data.id 返回（endpoint/cred-file/models 均被接受）" \
    "rc=${rc}；id=${cid:-<空>}；out=$(cli_out)" "$EV/${CLI_LAST}.out"
fi

# 逐字段回读（CLI 通道；回读腿用 profiles list——POST 的 data 是单卡对象，list 才是清单）。
rc=0; cli_run "a1-list" profiles list "$TOOL" --json || rc=$?
got_ep="$(cli_jget "next((x.get(\"endpoint\",\"\") for x in (d.get(\"data\",{}).get(\"items\") or []) if x.get(\"id\")==\"${cid}\"), \"\")")"
got_cr="$(cli_jget "next((x.get(\"credRef\",\"\") for x in (d.get(\"data\",{}).get(\"items\") or []) if x.get(\"id\")==\"${cid}\"), \"\")")"
got_md="$(cli_jget "next((\"|\".join(x.get(\"models\") or []) for x in (d.get(\"data\",{}).get(\"items\") or []) if x.get(\"id\")==\"${cid}\"), \"\")")"
if [[ "$rc" == "0" && "$got_ep" == "https://selfsvc-a.example/v1" ]]; then
  pass_ev "TC-UC032-A1[回读端点] endpoint=https://selfsvc-a.example/v1 逐字段一致"
else
  fail_ev "TC-UC032-A1[回读端点] 端点逐字段回读" \
    "endpoint=https://selfsvc-a.example/v1" "endpoint=${got_ep:-<空>}" "$EV/${CLI_LAST}.out"
fi
if [[ "$got_cr" == yzagmgr-managed-token://* ]]; then
  pass_ev "TC-UC032-A1[回读凭据] credRef=${got_cr}（引用形态，非明文）"
else
  fail_ev "TC-UC032-A1[回读凭据] 凭据引用逐字段回读" \
    "credRef 形如 yzagmgr-managed-token://<uid>" "credRef=${got_cr:-<空>}" "$EV/${CLI_LAST}.out"
fi
if [[ "$got_md" == "m-a|m-b" ]]; then
  pass_ev "TC-UC032-A1[回读模型清单] models=[m-a,m-b] 逐字段一致"
else
  fail_ev "TC-UC032-A1[回读模型清单] 模型清单逐字段回读" \
    "models=m-a|m-b" "models=${got_md:-<空>}" "$EV/${CLI_LAST}.out"
fi

# web 通道回读一致 + 明文哨兵零泄漏。
api_ev "a1-api-list" GET "/tools/${TOOL}/profiles"
api_ep="$(api_jget "next((x.get(\"endpoint\",\"\") for x in (d.get(\"data\",{}).get(\"items\") or []) if x.get(\"id\")==\"${cid}\"), \"\")")"
api_cr="$(api_jget "next((x.get(\"credRef\",\"\") for x in (d.get(\"data\",{}).get(\"items\") or []) if x.get(\"id\")==\"${cid}\"), \"\")")"
if [[ "$api_ep" == "https://selfsvc-a.example/v1" && "$api_cr" == "$got_cr" && "$api_cr" == yzagmgr-managed-token://* ]]; then
  pass_ev "TC-UC032-A1[web 回读] web API 三要素回读与 CLI 一致（credRef 同一引用）"
else
  fail_ev "TC-UC032-A1[web 回读] 三要素双通道一致" \
    "endpoint 一致且 credRef 同一引用" \
    "api_ep=${api_ep:-<空>}；api_cr=${api_cr:-<空>}；cli_cr=${got_cr:-<空>}" "$EV/${API_LAST}.out"
fi

leaked="$(leaked_sentinel "$cred_a" "$cred_b")"
if [[ -z "$leaked" && "$got_cr" == yzagmgr-managed-token://* ]]; then
  pass_ev "TC-UC032-A1[凭据不回显] 全部 CLI 输出/管理 API 回读/守护进程日志 grep 哨兵明文零命中（非空洞前提：credRef 已在册）"
else
  fail_ev "TC-UC032-A1[凭据不回显] 凭据明文不得回显" \
    "哨兵明文零命中（且 credRef 在册）" "泄漏到: ${leaked:-<无>}；credRef=${got_cr:-<空>}" "${leaked:-$EV}"
fi

# ── TC-UC032-A2（正·双入口建改删复制一致）────────────────────────────────────
# 建、改、复制、删四动作经 cli 与 web 同一 REST 完成、结果一致。

# 建（web 通道对照）：三要素形态一致。
api_ev "a2-web-create" POST "/tools/${TOOL}/profiles" \
  '{"name":"selfsvc-b","endpoint":"https://selfsvc-b.example/v1","cred":"'"$(tr -d '\r\n' <"$cred_b")"'","models":["m-c","m-d"]}'
wid="$(api_jget 'str(d.get("data",{}).get("id",""))')"
wid_ep="$(api_jget 'str(d.get("data",{}).get("endpoint",""))')"
wid_cr="$(api_jget 'str(d.get("data",{}).get("credRef",""))')"
if [[ -n "$wid" && "$wid_ep" == "https://selfsvc-b.example/v1" && "$wid_cr" == yzagmgr-managed-token://* && "$wid_cr" != "$got_cr" ]]; then
  pass_ev "TC-UC032-A2[建] web 建卡成功且三要素形态与 CLI 一致（id=${wid}，credRef 与 CLI 卡互异）"
else
  fail_ev "TC-UC032-A2[建] 双入口建卡一致" \
    "web 建卡 id/endpoint/credRef 形态与 CLI 一致" \
    "wid=${wid:-<空>}；ep=${wid_ep:-<空>}；cr=${wid_cr:-<空>}（CLI cr=${got_cr:-<空>}）" "$EV/${API_LAST}.out"
fi

# 改：CLI 改 A / web 改 B ⇒ 双通道回读各自变化。
rc=0; cli_run "a2-cli-update" profiles update "$TOOL" "${cid:-no-id}" \
  --endpoint "https://selfsvc-a2.example/v1" --models "m-a,m-b,m-c" --json || rc=$?
api_ev "a2-web-update" PUT "/tools/${TOOL}/profiles/${wid:-no-id}" \
  '{"endpoint":"https://selfsvc-b2.example/v1","models":["m-x"]}'
api_ev "a2-readback" GET "/tools/${TOOL}/profiles"
a_ep="$(api_jget "next((x.get(\"endpoint\",\"\") for x in (d.get(\"data\",{}).get(\"items\") or []) if x.get(\"id\")==\"${cid}\"), \"\")")"
a_md="$(api_jget "next((\"|\".join(x.get(\"models\") or []) for x in (d.get(\"data\",{}).get(\"items\") or []) if x.get(\"id\")==\"${cid}\"), \"\")")"
b_ep="$(api_jget "next((x.get(\"endpoint\",\"\") for x in (d.get(\"data\",{}).get(\"items\") or []) if x.get(\"id\")==\"${wid}\"), \"\")")"
b_md="$(api_jget "next((\"|\".join(x.get(\"models\") or []) for x in (d.get(\"data\",{}).get(\"items\") or []) if x.get(\"id\")==\"${wid}\"), \"\")")"
if [[ "$rc" == "0" && "$a_ep" == "https://selfsvc-a2.example/v1" && "$a_md" == "m-a|m-b|m-c" \
      && "$b_ep" == "https://selfsvc-b2.example/v1" && "$b_md" == "m-x" ]]; then
  pass_ev "TC-UC032-A2[改] CLI 改 A、web 改 B 均生效且逐字段回读一致（同 REST）"
else
  fail_ev "TC-UC032-A2[改] 双入口改卡一致" \
    "A→selfsvc-a2/m-a,m-b,m-c；B→selfsvc-b2/m-x" \
    "rc=${rc}；A=[${a_ep:-<空>}/${a_md:-<空>}] B=[${b_ep:-<空>}/${b_md:-<空>}]" \
    "$EV/${CLI_LAST}.out 与 $EV/${API_LAST}.out"
fi

# 复制：CLI 复制 A / web 复制 B ⇒ 名称加后缀、不改当前生效项。
api_ev "a2-active-before" GET "/tools/${TOOL}/profiles"
active_before="$(api_jget 'str(d.get("data",{}).get("activeProfileId",""))')"
live_before="$(bytes_sig "${LIVE_PATHS[@]}")"
rc=0; cli_run "a2-cli-dup" profiles duplicate "$TOOL" "${cid:-no-id}" --json || rc=$?
dup_a="$(cli_jget 'str(d.get("data",{}).get("id",""))')"
dup_a_name="$(cli_jget 'str(d.get("data",{}).get("name",""))')"
api_ev "a2-web-dup" POST "/tools/${TOOL}/profiles/${wid:-no-id}/duplicate"
dup_b="$(api_jget 'str(d.get("data",{}).get("id",""))')"
dup_b_name="$(api_jget 'str(d.get("data",{}).get("name",""))')"
api_ev "a2-active-after" GET "/tools/${TOOL}/profiles"
active_after="$(api_jget 'str(d.get("data",{}).get("activeProfileId",""))')"
live_after="$(bytes_sig "${LIVE_PATHS[@]}")"
dup_ok=1
[[ -n "$dup_a" && "$dup_a_name" == selfsvc-a* && "$dup_a_name" != "selfsvc-a" && "$dup_a_name" != "selfsvc-a2" ]] || dup_ok=0
[[ -n "$dup_b" && "$dup_b_name" == selfsvc-b* && "$dup_b_name" != "selfsvc-b" && "$dup_b_name" != "selfsvc-b2" ]] || dup_ok=0
[[ "$active_before" == "$active_after" && "$live_before" == "$live_after" ]] || dup_ok=0
if [[ "$rc" == "0" && "$dup_ok" == "1" ]]; then
  pass_ev "TC-UC032-A2[复制] CLI/web 双入口复制成功：名称加后缀（A→${dup_a_name}、B→${dup_b_name}），activeProfileId 与 live 字节不变（UC-018 A9）"
else
  fail_ev "TC-UC032-A2[复制] 双入口复制一致（加后缀、不切当前）" \
    "两入口复制均得新卡、名称加后缀（≠原名且原名为前缀）；activeProfileId 与 live 字节不变" \
    "rc=${rc}；A=[${dup_a:-<空>}/${dup_a_name:-<空>}] B=[${dup_b:-<空>}/${dup_b_name:-<空>}]；active 前=[${active_before}] 后=[${active_after}]；live 不变=$([[ "$live_before" == "$live_after" ]] && echo yes || echo no)" \
    "$EV/${CLI_LAST}.out 与 $EV/${API_LAST}.out"
fi

# 删：CLI 删 A 副本 / web 删 B 副本 ⇒ 双通道消失。
rc=0; cli_run "a2-cli-del" profiles delete "$TOOL" "${dup_a:-no-id}" --json || rc=$?
api_ev "a2-web-del" DELETE "/tools/${TOOL}/profiles/${dup_b:-no-id}"
api_ev "a2-final-list" GET "/tools/${TOOL}/profiles"
remain="$(api_jget "len([x for x in (d.get(\"data\",{}).get(\"items\") or []) if x.get(\"id\") in {\"${dup_a}\",\"${dup_b}\"}])")"
web_ok="$(api_jget 'd.get("ok","")')"
if [[ "$rc" == "0" && "$remain" == "0" && "$web_ok" == "true" ]]; then
  pass_ev "TC-UC032-A2[删] CLI/web 双入口删副本成功，列表回读均消失"
else
  fail_ev "TC-UC032-A2[删] 双入口删卡一致" \
    "两入口删除均成功且列表无残留" \
    "rc=${rc}；残留=${remain:-<空>}；web_ok=${web_ok:-<空>}" "$EV/${CLI_LAST}.out 与 $EV/${API_LAST}.out"
fi

# ── TC-UC032-A3（负·删除保护）────────────────────────────────────────────────
# 删除当前生效的自建卡被拒（invalid_request）且 live 配置保住；受管/官方卡不可删。

rc=0; cli_run "a3-preview" preview "$TOOL" --profile "${cid:-no-id}" --json || rc=$?
rc_apply=0; cli_run "a3-apply" apply "$TOOL" --profile "${cid:-no-id}" --json || rc_apply=$?
live_active="$(bytes_sig "${LIVE_PATHS[@]}")"
if [[ "$rc_apply" != "0" ]]; then
  echo "note: 启用自建卡失败（rc=${rc_apply}）——A3 前置（当前生效的自建卡）不成立：$(cli_out)"
fi

rc=0; cli_run "a3-del-active" profiles delete "$TOOL" "${cid:-no-id}" --json || rc=$?
ec="$(cli_jget 'str(d.get("error",{}).get("code",""))')"
live_after_del="$(bytes_sig "${LIVE_PATHS[@]}")"
if [[ "$rc" == "1" && "$ec" == "invalid_request" && "$live_active" == "$live_after_del" ]]; then
  pass_ev "TC-UC032-A3[删当前生效] 删除被拒：退出码 1 + invalid_request，live 配置逐字节保住"
else
  fail_ev "TC-UC032-A3[删当前生效] 删除保护" \
    "rc=1 + error.code=invalid_request 且 live 字节不变（先切到别的卡才能删）" \
    "rc=${rc}；code=${ec:-<空>}；live 不变=$([[ "$live_active" == "$live_after_del" ]] && echo yes || echo no)（apply 前置 rc=${rc_apply}）" \
    "$EV/${CLI_LAST}.out"
fi

rc=0; cli_run "a3-del-managed" profiles delete "$TOOL" "yzagmgr-local" --json || rc=$?
ec="$(cli_jget 'str(d.get("error",{}).get("code",""))')"
if [[ "$rc" == "1" && "$ec" == "invalid_request" ]]; then
  pass_ev "TC-UC032-A3[删受管] 受管卡删除被拒（rc=1 + invalid_request）"
else
  fail_ev "TC-UC032-A3[删受管] 受管卡不可删" \
    "rc=1 + error.code=invalid_request" "rc=${rc}；code=${ec:-<空>}；out=$(cli_out)" "$EV/${CLI_LAST}.out"
fi

api_ev "a3-del-official" DELETE "/tools/claude/profiles/official"
ec="$(api_jget 'str(d.get("error",{}).get("code",""))')"
okk="$(api_jget 'd.get("ok","")')"
if [[ "$okk" == "false" && "$ec" == "invalid_request" ]]; then
  pass_ev "TC-UC032-A3[删官方] 官方卡删除被拒（invalid_request，UC-018 A3）"
else
  fail_ev "TC-UC032-A3[删官方] 官方卡不可删" \
    "ok=false + error.code=invalid_request" "ok=${okk:-<空>}；code=${ec:-<空>}" "$EV/${API_LAST}.out"
fi

# ── TC-UC032-A4（边界·游客态可用面登记）──────────────────────────────────────
# 可用面：自建配置管理全流程（本地能力，免登录——A1–A3 全程无登录态完成即为其证据）。
# 被拒面：启用链的凭据步——凭据引用无法解析 ⇒ 显式报缺凭据，盘上零变化、无半写态。
# PW 握手：guest.pw.ts 的 JUnit 回读（路径 YZAGMGR_PW_JUNIT 可配，缺省
#   web/test-results/pw-junit.xml）——在位 ⇒ 按 web/e2e/test-map.json 的 UC-033 块
#   junit_identity 逐名在场且零失败 ⇒ pass_ev（证据=报告路径）；缺位 ⇒ 回退 unknown
#   并给解除条件「先跑 guest.pw.ts 留 JUnit」。
# 张力登记：UC-033 非目标句「含自建卡写」若被裁决为「游客写=拒绝(unauthenticated)」，
# 则本段口径翻转（断言集中于本段，翻转成本一行）——已在交付报告「契约冲突」登记。
#   「游客态自建卡写」保持张力登记 unknown **不折**（guest.pw.ts 头注明确排除该面）。

api_ev "a4-session" GET /session
sess="$(api_jget 'str(d.get("data",{}).get("state",""))')"
if [[ "$sess" == "anonymous" ]]; then
  pass_ev "TC-UC032-A4[可用面] 全套件无登录态（session=anonymous）完成 A1–A3：游客可用面=建/改/复制/删+三要素回读（本地能力，R122 ⑤ 免登录口径）"
else
  fail_ev "TC-UC032-A4[可用面] 游客态可用面登记前提" \
    "session=anonymous（无登录态）" "state=${sess:-<空>}" "$EV/${API_LAST}.out"
fi

# 被拒面：凭据引用无法解析 ⇒ 启用时显式报缺凭据，盘上零变化。
api_ev "a4-badcred-create" POST "/tools/${TOOL}/profiles" \
  '{"name":"selfsvc-badcred","endpoint":"https://selfsvc-badcred.example/v1","credRef":"yzagmgr-managed-token://nonexistent-uid","models":["m-z"]}'
bcid="$(api_jget 'str(d.get("data",{}).get("id",""))')"
live_before_bad="$(bytes_sig "${LIVE_PATHS[@]}")"
rc=0; cli_run "a4-badcred-preview" preview "$TOOL" --profile "${bcid:-no-id}" --json || rc=$?
ec="$(cli_jget 'str(d.get("error",{}).get("code",""))')"
emsg="$(cli_out)"
live_after_bad="$(bytes_sig "${LIVE_PATHS[@]}")"
rejected=0
if [[ "$rc" == "1" && ( "$ec" == "invalid_request" || "$ec" == "config_invalid" ) && "$emsg" == *凭据* ]]; then
  rejected=1
fi
if [[ "$rejected" == "1" && "$live_before_bad" == "$live_after_bad" ]]; then
  pass_ev "TC-UC032-A4[被拒面] 未解析凭据引用在启用（preview）被显式报缺凭据（rc=1，code=${ec}），盘上零变化、无半写态"
else
  fail_ev "TC-UC032-A4[被拒面] 启用链凭据步显式拒绝且零变化" \
    "rc=1 + invalid_request/config_invalid + 消息含「凭据」，live 字节不变" \
    "rc=${rc}；code=${ec:-<空>}；msg=${emsg:0:120}；live 不变=$([[ "$live_before_bad" == "$live_after_bad" ]] && echo yes || echo no)（建卡 bcid=${bcid:-<空>}）" \
    "$EV/${CLI_LAST}.out"
fi

# PW 握手回读：guest.pw.ts JUnit ↔ test-map.json UC-033 块 junit_identity 对账。
PW_JUNIT="${YZAGMGR_PW_JUNIT:-$ROOT/web/test-results/pw-junit.xml}"
TEST_MAP="$ROOT/web/e2e/test-map.json"
if [[ ! -f "$PW_JUNIT" ]]; then
  record_unknown "TC-UC032-A4[游客面] 真浏览器（PW）游客旅程的自建配置管理可用/被拒面登记" \
    "PW JUnit 报告缺位（${PW_JUNIT}）" \
    "先跑 guest.pw.ts 留 JUnit（YZAGMGR_PW_JUNIT 可指报告路径），再复跑本握手"
else
  pw_out="$(python3 - "$TEST_MAP" "$PW_JUNIT" <<'PY'
import json, sys, xml.etree.ElementTree as ET

tm_path, junit_path = sys.argv[1], sys.argv[2]
try:
    tm = json.load(open(tm_path, encoding="utf-8"))
except Exception as e:
    print("fail")
    print("test-map 读取失败: %s" % e)
    sys.exit(0)
idents = [t.get("junit_identity", "") for t in tm.get("tests", [])
          if t.get("usecase") == "UC-033" and t.get("junit_identity")]
if not idents:
    print("fail")
    print("test-map.json 的 UC-033 块无 junit_identity 条目")
    sys.exit(0)
try:
    root = ET.parse(junit_path).getroot()
except Exception as e:
    print("fail")
    print("JUnit 解析失败: %s" % e)
    sys.exit(0)
# junit_identity 形态 = "<classname>::<testcase name>"，与 JUnit 逐字对齐。
cases = {}
for tc in root.iter("testcase"):
    key = "%s::%s" % (tc.get("classname", ""), tc.get("name", ""))
    bad = tc.find("failure") is not None or tc.find("error") is not None
    skipped = tc.find("skipped") is not None
    cases[key] = (bad, skipped)
missing, failed = [], []
for ident in idents:
    hit = cases.get(ident)
    if hit is None:
        missing.append(ident)
    elif hit[0]:
        failed.append(ident + "（失败）")
    elif hit[1]:
        failed.append(ident + "（skip 未真跑）")
n = len(idents)
if not missing and not failed:
    print("pass")
    print("guest.pw.ts JUnit 对 test-map.json UC-033 块 %d/%d 逐名在场、零失败、零 skip" % (n, n))
else:
    print("fail")
    print("期望 %d 条 junit_identity 逐名在场且零失败；缺 %d：%s；未过 %d：%s" % (
        n, len(missing), " ‖ ".join(missing) or "无", len(failed), " ‖ ".join(failed) or "无"))
PY
)"
  pw_verdict="$(printf '%s' "$pw_out" | head -n 1)"
  pw_detail="$(printf '%s' "$pw_out" | tail -n +2 | tr '\n' ' ')"
  if [[ "$pw_verdict" == "pass" ]]; then
    pass_ev "TC-UC032-A4[游客面] 真浏览器（PW）游客旅程在册：${pw_detail}（证据=${PW_JUNIT}）"
  else
    fail_ev "TC-UC032-A4[游客面] 真浏览器（PW）游客旅程 JUnit 对账" \
      "web/e2e/test-map.json UC-033 块 junit_identity 逐名在场且零失败（guest.pw.ts）" \
      "${pw_detail}" \
      "$PW_JUNIT"
  fi
fi

# 「游客态自建卡写」张力登记——guest.pw.ts 头注明确排除该面（UC-033 非目标句「含自建卡写」），不折 pass。
record_unknown "TC-UC032-A4[自建卡写] 游客态自建卡写面" \
  "guest.pw.ts 头注明确排除该面（UC-033 非目标句「含自建卡写」张力登记不折）" \
  "UC-033 非目标句裁决后翻转本行口径（游客写=拒绝(unauthenticated)）或交付游客自建卡写 PW 旅程后另行签收"

printf 'report  self-service-config: tool=%s cards=a:%s b:%s evidence=%s\n' "$TOOL" "${cid:-<空>}" "${wid:-<空>}" "$EV"
verify_finish

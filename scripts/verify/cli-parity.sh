#!/usr/bin/env bash
# scripts/verify/cli-parity.sh —— UC-031 CLI 复用相同组件接口（管理面平权 + 同库自证）。
#
# yzagmgr-cli 与 Mac yzagmgr app（web）复用相同组件接口：同一守护进程 REST、同一数据根，
# 仅交互不同。本脚本断言：CLI 覆盖 **建档 / 档位卡 CRUD / 物化**（对照 web 同 REST）；
# CLI **零本地 DB 访问**（无令牌文件时给明确错误、绝不绕道直写盘/私有状态）；
# CLI 建的卡经 web API 可见、web 建的卡经 CLI 可见（同一 daemon 读回，同库双向自证）；
# 全流程在 Mac 隔离可复跑。
#
# ── 本脚本冻结的可执行契约（能力未交付 ⇒ 全部如实红；实现方按此对齐后本脚本转绿）──
# 平权清单（CLI 入口 ↔ web REST 一一对应；「清单内缺 CLI 入口为红」由本表逐行断言，
# 「清单外调用为红」属 Go 契约测试的完备性面——实现期另行登记）：
#   ① 建档：
#      yzagmgr ensure-gateway-profiles [--user <uid>] [--policy-version <n>] --json
#        ⇒ POST /api/v1/tools/ensure-gateway-profiles（同形 data：items[]/liveDrift[]/autoEnable[]）
#   ② 档位卡 CRUD（api-v1.md §4.2 同一 REST，零 API 变更）：
#      yzagmgr profiles list     <tool> --json            ⇒ GET    /tools/{id}/profiles
#      yzagmgr profiles create   <tool> --name <n> [--from <pid>]
#                             [--endpoint <url>] [--cred-file <0600文件>] [--models a,b,c] --json
#        ⇒ POST /tools/{id}/profiles（cred-file：明文只经 0600 文件交付，不进 argv/日志，
#          守护进程收进托管凭据库，卡只落引用——UC-023 routes add 同口径）
#      yzagmgr profiles update   <tool> <pid> [--name <n>] [--model-id <m>]
#                             [--endpoint <url>] [--cred-file <0600文件>] [--models a,b,c] --json
#        ⇒ PUT  /tools/{id}/profiles/{pid}
#      yzagmgr profiles delete   <tool> <pid> --json       ⇒ DELETE /tools/{id}/profiles/{pid}
#      yzagmgr profiles duplicate <tool> <pid> --json      ⇒ POST   /tools/{id}/profiles/{pid}/duplicate
#   ③ 物化（UC-019 语义）：
#      yzagmgr materialize [--refresh] --json              ⇒ GET|POST /materialize
#        （缺省 = 只读盘点；--refresh = 重新物化）
#   信封/组合视图字节保真沿 doc/cli.md 冻结面：单发命令 data 逐字节保留 REST 载荷原文；
#   退出码沿 UC-022 A4：0 成功 / 1 业务失败 / 2 用法错误 / 3 令牌缺失 / 4 守护进程不可达。
# 同库自证：CLI 与 web 读写**同一**守护进程数据根（YZAGMGR_HOME 隔离根下 .yzagmgr/），
# 双向可见是契约；「当前生效」以盘上判定为准（UC-018 A7，回读腿用 profiles.activeProfileId）。
# 零本地 DB：CLI 是纯 REST 客户端——无 admin.token 时退出码 3 + 明确错误（含「令牌」字样），
# 有令牌时一切经 REST；usage.db 等私有状态文件只属守护进程，CLI 全程不得打开/写入。
#
# 现状红点（2026-09-24 覆盖审计 D4，先红后绿）：
#   CLI 仅约 11/82 端点（health/tools/preview/apply/restore/backups/policy），
#   `yzagmgr ensure-gateway-profiles|profiles|materialize` 全部落「未知子命令」用法错误
#   （退出码 2）——建档/建卡/物化的平权覆盖面是本条缺口；零本地 DB 判据无载体。
#   本脚本在此状态下真跑并逐断言如实红（verdict FAIL，退出码 1），不得 skip/todo 冒充。
#
# 断言 ↔ UC-031 验收行：
#   TC-UC031-A1 端点平权（清单逐行：CLI 入口存在 + 同形信封 + 与 web REST 双通道一致）
#   TC-UC031-A2 同库自证双向（CLI 建卡 web 可见 / web 建卡 CLI 可见，逐字段一致）
#   TC-UC031-A3 零本地 DB 访问（无令牌明确错误 + 私有状态文件全程字节不变 + 不可达退出码 4）
#   TC-UC031-A4 Mac 可复跑（隔离根在 RUN 内 + 关键序列两遍签名一致 + 输出留档）
#
# 用法: cli-parity.sh
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib.sh
. "$HERE/lib.sh"

unset ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN ANTHROPIC_BASE_URL ANTHROPIC_MODEL 2>/dev/null || true

verify_init cli-parity
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

# ── 证据与三态辅助 ───────────────────────────────────────────────────────────

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

cli_run() { # cli_run <label> <yzagmgr 参数...>：返回 CLI 退出码
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

api_ev() { # api_ev <label> <METHOD> <path> [json-body]：web REST 腿；体落证据文件
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

new_token() {
  python3 -c 'import secrets, sys, os
p = sys.argv[1]
open(p, "w").write("yzagmgr-test-" + secrets.token_hex(16) + "\n")
os.chmod(p, 0o600)' "$1"
}

DATA_ROOT="$RUN/.yzagmgr/data"
USAGE_DB="$DATA_ROOT/usage.db"

# ── TC-UC031-A1（正·端点平权）────────────────────────────────────────────────
# 清单逐行：CLI 入口存在（缺入口 =「未知子命令/未知旗标」⇒ 红）+ 同形信封 +
# 与 web REST 双通道回读一致。

# ① 建档：ensure-gateway-profiles
rc=0; cli_run "ensure" ensure-gateway-profiles --json || rc=$?
cli_tools="$(cli_jget '"|".join(sorted(x.get("toolId","") for x in (d.get("data",{}).get("items") or [])))')"
api_ev "ensure-api" POST /tools/ensure-gateway-profiles '{}'
api_tools="$(api_jget '"|".join(sorted(x.get("toolId","") for x in (d.get("data",{}).get("items") or [])))')"
cli_status_set="$(cli_jget '"|".join(sorted({x.get("status","") for x in (d.get("data",{}).get("items") or [])}))')"
status_enum_ok=1
IFS='|' read -r -a _sts <<<"$cli_status_set"
for _s in "${_sts[@]}"; do
  case "$_s" in created|reused|failed|skipped|"") ;; *) status_enum_ok=0 ;; esac
done
if [[ "$rc" == "0" && -n "$cli_tools" && "$cli_tools" == "$api_tools" && "$status_enum_ok" == "1" ]]; then
  pass_ev "TC-UC031-A1[建档] ensure-gateway-profiles 与 web POST /tools/ensure-gateway-profiles 同形（items toolId 集一致：${cli_tools}）"
else
  fail_ev "TC-UC031-A1[建档] ensure-gateway-profiles 平权" \
    "rc=0 且与 REST 同形（items.toolId 集一致，status ∈ created/reused/failed/skipped）" \
    "rc=${rc}；CLI=[${cli_tools:-<空>}] status∈[${cli_status_set:-<空>}]；API=[${api_tools:-<空>}]；out=$(cli_out)" \
    "$EV/${CLI_LAST}.out 与 $EV/${API_LAST}.out"
fi

# ② 档位卡 CRUD 五入口（list/create/update/delete/duplicate）。
TOOL=claude
rc=0; cli_run "profiles-list" profiles list "$TOOL" --json || rc=$?
list_sig="$(cli_jget '"|".join(sorted(x.get("id","")+":"+x.get("name","") for x in (d.get("data",{}).get("items") or [])))')"
api_ev "profiles-list-api" GET "/tools/${TOOL}/profiles"
api_list_sig="$(api_jget '"|".join(sorted(x.get("id","")+":"+x.get("name","") for x in (d.get("data",{}).get("items") or [])))')"
if [[ "$rc" == "0" && -n "$list_sig" && "$list_sig" == "$api_list_sig" ]]; then
  pass_ev "TC-UC031-A1[profiles list] CLI 与 web GET /tools/${TOOL}/profiles 同形同库（items 签名一致）"
else
  fail_ev "TC-UC031-A1[profiles list] 平权回读" \
    "rc=0 且与 REST items 逐字段一致" \
    "rc=${rc}；CLI=[${list_sig:-<空>}]；API=[${api_list_sig:-<空>}]；out=$(cli_out)" \
    "$EV/${CLI_LAST}.out 与 $EV/${API_LAST}.out"
fi

cred_a="$RUN/cred-a.token"; new_token "$cred_a"
rc=0; cli_run "profiles-create" profiles create "$TOOL" --name parity-a --endpoint "https://parity-a.example/v1" \
  --cred-file "$cred_a" --models "m-a,m-b" --json || rc=$?
cid="$(cli_jget 'str(d.get("data",{}).get("id",""))')"
if [[ "$rc" == "0" && -n "$cid" ]]; then
  pass_ev "TC-UC031-A1[profiles create] CLI 建卡成功（id=${cid}）"
else
  fail_ev "TC-UC031-A1[profiles create] 平权入口" \
    "rc=0 且 data.id 返回" "rc=${rc}；id=${cid:-<空>}；out=$(cli_out)" "$EV/${CLI_LAST}.out"
fi

rc=0; cli_run "profiles-update" profiles update "$TOOL" "${cid:-no-id}" --name parity-a2 --json || rc=$?
if [[ "$rc" == "0" ]]; then
  pass_ev "TC-UC031-A1[profiles update] CLI 改卡成功"
else
  fail_ev "TC-UC031-A1[profiles update] 平权入口" \
    "rc=0" "rc=${rc}；out=$(cli_out)" "$EV/${CLI_LAST}.out"
fi

rc=0; cli_run "profiles-duplicate" profiles duplicate "$TOOL" "${cid:-no-id}" --json || rc=$?
did="$(cli_jget 'str(d.get("data",{}).get("id",""))')"
if [[ "$rc" == "0" && -n "$did" && "$did" != "$cid" ]]; then
  pass_ev "TC-UC031-A1[profiles duplicate] CLI 复制卡成功（新 id=${did}）"
else
  fail_ev "TC-UC031-A1[profiles duplicate] 平权入口" \
    "rc=0 且返回新 id" "rc=${rc}；id=${did:-<空>}（原 ${cid:-<空>}）；out=$(cli_out)" "$EV/${CLI_LAST}.out"
fi

rc=0; cli_run "profiles-delete-dup" profiles delete "$TOOL" "${did:-no-id}" --json || rc=$?
if [[ "$rc" == "0" ]]; then
  pass_ev "TC-UC031-A1[profiles delete] CLI 删卡成功"
else
  fail_ev "TC-UC031-A1[profiles delete] 平权入口" \
    "rc=0" "rc=${rc}；out=$(cli_out)" "$EV/${CLI_LAST}.out"
fi

# ③ 物化：只读盘点 + 重新物化。
rc=0; cli_run "materialize" materialize --json || rc=$?
fams="$(cli_jget '"|".join(sorted(f.get("toolId","") for f in (d.get("data",{}).get("families") or [])))')"
api_ev "materialize-api" GET /materialize
api_fams="$(api_jget '"|".join(sorted(f.get("toolId","") for f in (d.get("data",{}).get("families") or [])))')"
if [[ "$rc" == "0" && -n "$fams" && "$fams" == "$api_fams" ]]; then
  pass_ev "TC-UC031-A1[materialize GET] CLI 盘点与 web GET /materialize 同形（families 集一致）"
else
  fail_ev "TC-UC031-A1[materialize GET] 平权盘点" \
    "rc=0 且与 REST families 集一致" \
    "rc=${rc}；CLI=[${fams:-<空>}]；API=[${api_fams:-<空>}]；out=$(cli_out)" \
    "$EV/${CLI_LAST}.out 与 $EV/${API_LAST}.out"
fi
rc=0; cli_run "materialize-refresh" materialize --refresh --json || rc=$?
if [[ "$rc" == "0" && -n "$(cli_jget 'd.get("data",{}).get("root","")')" ]]; then
  pass_ev "TC-UC031-A1[materialize POST] 重新物化成功（root=$(cli_jget 'str(d["data"]["root"])')）"
else
  fail_ev "TC-UC031-A1[materialize POST] 平权重新物化" \
    "rc=0 且 data.root 返回" "rc=${rc}；out=$(cli_out)" "$EV/${CLI_LAST}.out"
fi

# ── TC-UC031-A2（正·同库自证双向）────────────────────────────────────────────
# CLI 建的卡经 web API 可见逐字段一致；web（REST）建的卡经 CLI 可见可管理。

if [[ -n "$cid" ]]; then
  api_ev "a2-see-cli-card" GET "/tools/${TOOL}/profiles"
  got="$(api_jget "next((x.get(\"id\",\"\")+\"|\"+x.get(\"name\",\"\") for x in (d.get(\"data\",{}).get(\"items\") or []) if x.get(\"id\")==\"${cid}\"), \"\")")"
  if [[ "$got" == "${cid}|parity-a2" ]]; then
    pass_ev "TC-UC031-A2[CLI→web] CLI 建的卡 ${cid} 经 web API 可见（id|name 逐字段一致：${got}）"
  else
    fail_ev "TC-UC031-A2[CLI→web] CLI 建卡 web 可见" \
      "${cid}|parity-a2 在 GET /tools/${TOOL}/profiles 中" "回读=${got:-<缺失>}" "$EV/${API_LAST}.out"
  fi
else
  fail_ev "TC-UC031-A2[CLI→web] CLI 建卡 web 可见" "CLI 建卡成功后 web 可见" "CLI 建卡未成功（见 A1 create 红项），判据不成立" "$EV/commands.log"
fi

cred_b="$RUN/cred-b.token"; new_token "$cred_b"
api_ev "a2-web-create" POST "/tools/${TOOL}/profiles" \
  '{"name":"parity-b","endpoint":"https://parity-b.example/v1","cred":"'"$(tr -d '\r\n' <"$cred_b")"'","models":["m-c"]}'
wid="$(api_jget 'str(d.get("data",{}).get("id",""))')"
rc=0; cli_run "a2-cli-list" profiles list "$TOOL" --json || rc=$?
got="$(cli_jget "next((x.get(\"id\",\"\")+\"|\"+x.get(\"name\",\"\") for x in (d.get(\"data\",{}).get(\"items\") or []) if x.get(\"id\")==\"${wid}\"), \"\")")"
if [[ -n "$wid" && "$rc" == "0" && "$got" == "${wid}|parity-b" ]]; then
  pass_ev "TC-UC031-A2[web→CLI] web 建的卡 ${wid} 经 CLI 可见（id|name 逐字段一致：${got}）"
else
  fail_ev "TC-UC031-A2[web→CLI] web 建卡 CLI 可见" \
    "REST 建卡后 profiles list 含 ${wid}|parity-b" \
    "wid=${wid:-<空>}；CLI 回读=${got:-<缺失>}（rc=${rc}）" "$EV/${API_LAST}.out 与 $EV/${CLI_LAST}.out"
fi

# ── TC-UC031-A3（负·零本地 DB 访问）──────────────────────────────────────────
# CLI 不打开/不写本地 DB 或私有状态文件（usage.db 只属守护进程）；无令牌 ⇒ 退出码 3
# + 明确错误；守护进程不可达 ⇒ 退出码 4；两种失败路径都不得落盘。

if [[ -f "$USAGE_DB" ]]; then
  usage_before="$(file_meta "$USAGE_DB")"
else
  usage_before="exists=0 size=- sha256=- mtime_ns=-"
fi

# ① 有令牌跑一轮只读 CLI 期间 usage.db 全量元数据不变（CLI 不写私有状态）。
rc=0; cli_run "a3-readonly" tools show "$TOOL" --json || rc=$?
rc=0; cli_run "a3-readonly2" health --json || rc=$?
usage_after="$(file_meta "$USAGE_DB")"
if [[ "$usage_after" != "exists=1"* ]]; then
  fail_ev "TC-UC031-A3[零写库] usage.db 应存在且不变" \
    "exists=1 且前后元数据一致" "usage.db 不存在（无法形成非空洞判据）" "$DATA_ROOT"
elif [[ "$usage_before" == "$usage_after" ]]; then
  pass_ev "TC-UC031-A3[零写库] CLI 只读序列前后 usage.db 全量元数据逐字段不变（${usage_after}）"
else
  fail_ev "TC-UC031-A3[零写库] CLI 不得打开/写入 usage.db" \
    "前后元数据逐字段不变" "前=[${usage_before}] 后=[${usage_after}]" "$DATA_ROOT"
fi

# ② 无令牌文件 ⇒ 明确错误（退出码 3 +「令牌」字样），且不绕道直写盘（哨兵字节不变）。
#    载体用既有子命令 tools list：平权入口缺失的红在 A1 逐行登记，本段量 CLI **进程级**
#    零本地 DB 契约（不因入口缺失而把行为面判据悬空）。
mv "$TOKEN_FILE" "$TOKEN_FILE.hidden"
printf 'sentinel-private-db-0000\n' >"$DATA_ROOT/usage.db.sentinel"
sent_before="$(file_meta "$DATA_ROOT/usage.db.sentinel")"
rc=0; run_capped 30 "$EV/a3-notoken.out" "$EV/a3-notoken.err" env \
  YZAGMGR_HOME="$RUN" YZAGMGR_API_PORT="$API_PORT" "$YZAGMGR" tools list --json || rc=$?
sent_after="$(file_meta "$DATA_ROOT/usage.db.sentinel")"
# --json 形态下错误信封走 stdout（doc/cli.md 输出形态）；人工错误才在 stderr——两处都查。
err_txt="$(cat "$EV/a3-notoken.err" "$EV/a3-notoken.out" 2>/dev/null || true)"
if [[ "$rc" == "3" && "$err_txt" == *令牌* && "$sent_before" == "$sent_after" ]]; then
  pass_ev "TC-UC031-A3[无令牌] 无 admin.token ⇒ 退出码 3 +「令牌」明确错误，私有状态哨兵字节不变（不绕道直写盘）"
else
  fail_ev "TC-UC031-A3[无令牌] 零本地 DB：无令牌只许明确报错" \
    "rc=3 且错误含「令牌」且哨兵不变" \
    "rc=${rc}；err=${err_txt:0:120}；哨兵前=[${sent_before}] 后=[${sent_after}]" \
    "$EV/a3-notoken.err 与 $EV/a3-notoken.out"
fi

# ③ 守护进程不可达 ⇒ 退出码 4，同样零落盘（不自动拉起、不离线直写）。
#    先恢复令牌：本腿要量的是「不可达」，令牌缺失会先落退出码 3（错位判据）。
mv "$TOKEN_FILE.hidden" "$TOKEN_FILE"
dead_port="$(pick_free_port)"
rc=0; run_capped 15 "$EV/a3-dead.out" "$EV/a3-dead.err" env \
  YZAGMGR_HOME="$RUN" YZAGMGR_API_PORT="$dead_port" "$YZAGMGR" tools list --json || rc=$?
sent_after2="$(file_meta "$DATA_ROOT/usage.db.sentinel")"
if [[ "$rc" == "4" && "$sent_before" == "$sent_after2" ]]; then
  pass_ev "TC-UC031-A3[不可达] 管理 API 不可达 ⇒ 退出码 4 且零落盘（哨兵不变、不自动拉起）"
else
  fail_ev "TC-UC031-A3[不可达] 零本地 DB：不可达只许退出码 4" \
    "rc=4 且哨兵不变" "rc=${rc}；哨兵前=[${sent_before}] 后=[${sent_after2}]" "$EV/a3-dead.err"
fi

# ── TC-UC031-A4（边界·Mac 可复跑）────────────────────────────────────────────
# 隔离：受管路径全落 RUN 内（真实 HOME 零触碰）；复跑：关键序列两遍签名一致（含
# ensure 幂等 reused）；输出可留档（commands.log）。全流程全绿（退出码 0）= 本套件
# verdict 自身——红相如实红。

rc=0; cli_run "a4-paths" tools list --json || rc=$?
# RUN 的 mktemp 模板含 TMPDIR 尾斜杠 ⇒ 形如 …/T//name；工具回读的路径是规范化单斜杠。
# 比较前先把 RUN 规范化（cd+pwd），否则「越界」全是假红。
RUN_N="$(cd "$RUN" && pwd)"
paths_raw="$(cli_jget '"|".join(p.get("path","") for t in (d.get("data") or []) for p in (t.get("paths") or []))')"
outside=""
IFS='|' read -r -a allpaths <<<"$paths_raw"
for p in "${allpaths[@]}"; do
  [[ -z "$p" ]] && continue
  case "$p" in
    "$RUN_N"/*) ;;
    *) outside="${outside}${p} " ;;
  esac
done
if [[ "$rc" == "0" && -n "$paths_raw" && -z "$outside" ]]; then
  pass_ev "TC-UC031-A4[隔离] 全部受管路径落隔离根内（YZAGMGR_HOME=${RUN_N}），真实 HOME 零触碰"
else
  fail_ev "TC-UC031-A4[隔离] 全流程隔离可复跑" \
    "受管路径全部 ∈ ${RUN_N}" "越界路径: ${outside:-<无>}/rc=${rc}；paths=${paths_raw:0:200}" "$EV/${CLI_LAST}.out"
fi

rc=0; cli_run "a4-ensure-2" ensure-gateway-profiles --json || rc=$?
reused_set="$(cli_jget '"|".join(sorted({x.get("status","") for x in (d.get("data",{}).get("items") or [])}))')"
rc2=0; cli_run "a4-list-2" profiles list "$TOOL" --json || rc2=$?
list_sig2="$(cli_jget '"|".join(sorted(x.get("id","")+":"+x.get("name","") for x in (d.get("data",{}).get("items") or [])))')"
api_ev "a4-list-api" GET "/tools/${TOOL}/profiles"
api_list_sig2="$(api_jget '"|".join(sorted(x.get("id","")+":"+x.get("name","") for x in (d.get("data",{}).get("items") or [])))')"
if [[ "$rc" == "0" && "$rc2" == "0" && "$reused_set" == "reused" && "$list_sig2" == "$api_list_sig2" && -s "$EV/commands.log" ]]; then
  pass_ev "TC-UC031-A4[复跑] ensure 复跑全 reused（幂等）且 list 与 web 回读签名一致；命令留档 commands.log（全流程全绿=套件 verdict）"
else
  fail_ev "TC-UC031-A4[复跑] Mac 可复跑（幂等 + 签名一致 + 留档）" \
    "ensure rc=0 全 reused；list 双通道签名一致；commands.log 非空" \
    "ensure rc=${rc} status∈[${reused_set:-<空>}]；list rc=${rc2}；CLI=[${list_sig2:-<空>}] API=[${api_list_sig2:-<空>}]" \
    "$EV/${CLI_LAST}.out 与 $EV/${API_LAST}.out"
fi

printf 'report  cli-parity: tool=%s evidence=%s\n' "$TOOL" "$EV"
verify_finish

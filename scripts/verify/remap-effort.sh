#!/usr/bin/env bash
# scripts/verify/remap-effort.sh —— UC-029 remap effort 单层钳制。
#
# 请求 effort 经映射表**白名单归一、单跳**钳制到上游接受档位；缺项 **fail-closed**
# （4xx 不回落不透传）；**逐臂**独立钳制；禁多层嵌套（与 UC-028 同为单层原则）。
#
# ── 本脚本冻结的可执行契约（能力未交付 ⇒ 全部如实红；实现方按此对齐后本脚本转绿）──
# 管理面（经 bin/yzagmgr CLI；臂登记沿 UC-023 routes 家族，见 api-v1.md §7.1 契约）：
#   yzagmgr remap effort set   <arm> --from <src> --to <dst> --json
#   yzagmgr remap effort force <arm> --to <dst> --json     # df 形态：forceEffort=<dst>
#   yzagmgr remap effort list  <arm> --json ⇒ {"ok":true,"data":{"items":[{from,to},…],
#                                              "forceEffort":"<档> 或空串"}}
#   语义：
#     - **单跳**：请求 effort 查表一次改写为 to 后发出，不再二跳；
#     - **恒等条目 v→v = 上游白名单登记**，不是改写边——允许与改写边共存
#       （承载语义锚 glm 形态 `{xhigh→max, max→max}` 强制要求这种表；
#       证据索引 reports/mixed-five-route-20260923/summary.md 五路由矩阵）；
#     - **禁嵌套（A5）**：改写型条目（from≠to）的 to 不得再作为任何条目的 from
#       （恒等条目除外，即「映射目标不得再作为源被二次映射」UC-029 主流程 2）；
#       改写型条目的 from 也不得再成为任何条目的 to（否则同样构成二跳）。违反 ⇒ 业务拒绝
#       （退出码 1）且原表不变；
#     - **缺项 fail-closed（A3）**：请求 effort 不在表内 ⇒ 4xx，不回落默认档、不透传
#       （上游零命中）；effort 字段缺席且表无缺省项 ⇒ 同样 4xx；非法档在**非 force 表**
#       仍走缺项 4xx（与 force 语义不混）；
#     - **force 形态（df）**：入参任意（含缺席、非法档）恒发 forceEffort 目标档。
# 数据面（本地 router）：
#   POST /router/v1/messages；effort 线形 = 请求 body `output_config.effort`
#   （v1 实证 wire：转录顶层 effort = 实发 output_config.effort）；出站同形单跳改写。
#
# 承载语义锚（已实证，R107/R108）：reports/mixed-five-route-20260923/
#   mp 臂 `xhigh→high`；glm 臂 `xhigh→max` 且 `max→max`；df 臂 `force=max`（恒发 max）。
#
# 现状红点（2026-09-24 覆盖审计 D3，先红后绿）：remap 的存储/REST/CLI **全缺**——
# `yzagmgr remap …` 落「未知子命令」用法错误（退出码 2）；缺项 fail-closed 与逐臂钳制
# 均为待建硬语义。本脚本在此状态下真跑并逐断言如实红（verdict FAIL），不得 skip/todo 冒充。
#
# 断言 ↔ UC-029 验收行：
#   TC-UC029-A1 白名单归一单跳（一次改写为表目标档，不再二跳）
#   TC-UC029-A2 承载语义锚定（mp xhigh→high / glm xhigh→max 且 max→max / df force=max）
#   TC-UC029-A3 缺项 fail-closed（缺项、缺席、非法档 ⇒ 4xx 且上游零命中）
#   TC-UC029-A4 逐臂钳制独立（同值入参不同出站；一臂拒绝不影响他臂）
#   TC-UC029-A5 单层禁嵌套（target 再作源 / source 再作 target ⇒ 注册被拒；恒等登记豁免有对照）
#
# 用法: remap-effort.sh
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib.sh
. "$HERE/lib.sh"

unset ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN ANTHROPIC_BASE_URL ANTHROPIC_MODEL 2>/dev/null || true

verify_init remap-effort
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
REQ_LAST=""
REQ_CODE=""

declare -a ARMS=(mp glm df)   # 承载语义锚三形态（UC-029 A2）

# ── 证据与三态辅助（失败打印「期望/实际/证据路径」；输出有界）──────────────────

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
cli_out() { # 有界输出 CLI 的 stderr+stdout（错误原话多在 stderr；压平换行保持单行记录）
  { head -c 200 "$EV/${CLI_LAST}.err" 2>/dev/null; printf ' ‖ '; head -c 200 "$EV/${CLI_LAST}.out" 2>/dev/null; } \
    | tr '\n' ' ' || true
}
cli_out_full() { cat "$EV/${CLI_LAST}.out" 2>/dev/null || true; }
cli_jget() { jget "$(cli_out_full)" "$1"; }

effort_req() { # effort_req <label> <arm> <effort|"">：effort 空 = 字段缺席；结果进 $REQ_CODE/$REQ_LAST
  local label="$1" arm="$2" eff="$3" base body
  EVN=$((EVN + 1))
  printf -v base '%02d-%s' "$EVN" "$label"
  if [[ -n "$eff" ]]; then
    body="{\"model\":\"${arm}\",\"output_config\":{\"effort\":\"${eff}\"},\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"
  else
    body="{\"model\":\"${arm}\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"
  fi
  REQ_CODE="$(curl -sS -o "$EV/${base}.out" -w '%{http_code}' -m 15 -X POST \
    -H 'Content-Type: application/json' --data-binary "$body" \
    "http://127.0.0.1:${PROXY_PORT}/router/v1/messages" 2>"$EV/${base}.err" || true)"
  REQ_LAST="$base"
}
req_ev() { printf '%s' "$EV/${REQ_LAST}.out"; }

effort_snapshot() { # effort_snapshot <arm>：表排序串（from→to|…）+ force → $SNAP_MAP / $SNAP_FORCE
  local rc=0
  cli_run "effort-list-$1" remap effort list "$1" --json || rc=$?
  SNAP_MAP="$(cli_jget '"|".join(sorted(x["from"]+"→"+x["to"] for x in d["data"]["items"]))')"
  SNAP_FORCE="$(cli_jget 'str(d["data"].get("forceEffort",""))')"
}

# ── 夹具（0600 哨兵令牌 + 三口上游桩；桩记录 model/effort/auth_sha12 摘要）────────

new_token() {
  python3 -c 'import secrets, sys, os
p = sys.argv[1]
open(p, "w").write("yzagmgr-test-" + secrets.token_hex(16) + "\n")
os.chmod(p, 0o600)' "$1"
}
auth_sha12_of() {
  python3 -c 'import hashlib, sys
tok = open(sys.argv[1]).read().strip().encode()
print(hashlib.sha256(b"Bearer " + tok).hexdigest()[:12])' "$1"
}

cat >"$RUN/arm_stub.py" <<'PY'
import hashlib, json, sys, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PAIRS = list(zip(sys.argv[1::2], sys.argv[2::2]))

def note(log, obj):
    with open(log, "a", encoding="utf-8") as f:
        f.write(json.dumps(obj, ensure_ascii=False) + "\n")

def handler_for(port, log):
    class H(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"
        def log_message(self, *a):
            pass
        def _send(self, payload):
            raw = json.dumps(payload).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)
        def do_GET(self):
            self._send({"ok": True})
        def do_POST(self):
            n = int(self.headers.get("Content-Length") or 0)
            body = self.rfile.read(n)
            try:
                doc = json.loads(body or b"{}")
            except Exception:
                doc = {}
            oc = doc.get("output_config") or {}
            auth = self.headers.get("Authorization") or ""
            note(log, {"port": int(port), "path": self.path,
                       "model": str(doc.get("model") or ""),
                       "effort": str(oc.get("effort") or doc.get("effort") or ""),
                       "auth_sha12": hashlib.sha256(auth.encode()).hexdigest()[:12] if auth else "",
                       "bytes": n})
            self._send({"id": "msg_stub", "type": "message", "role": "assistant",
                        "model": str(doc.get("model") or "stub"),
                        "content": [{"type": "text", "text": "stub-ok"}],
                        "stop_reason": "end_turn",
                        "usage": {"input_tokens": 3, "output_tokens": 2}})
    return H

for p, lg in PAIRS:
    s = ThreadingHTTPServer(("0.0.0.0", int(p)), handler_for(p, lg))
    threading.Thread(target=s.serve_forever, daemon=True).start()
while True:
    time.sleep(3600)
PY

stub_reset() { : >"$1"; }
stub_hits() { [[ -s "$1" ]] || { printf '0'; return 0; }; wc -l <"$1" | tr -d ' '; }
stub_last_field() {
  python3 -c 'import json, sys
try:
    print(json.loads(open(sys.argv[1]).read().splitlines()[-1]).get(sys.argv[2], ""))
except Exception:
    print("")' "$1" "$2"
}

UP_IP="$(host_ip || true)"
[[ -n "$UP_IP" ]] || die "本机没有非回环 IPv4：上游端点禁止回环（UC-007 A13 口径），无法起 hermetic 上游桩"

declare -a STUB_PORTS=() STUB_LOGS=()
for _n in "${ARMS[@]}"; do
  _p="$(pick_free_port)"; _lg="$RUN/stub-${_n}.log"; stub_reset "$_lg"
  STUB_PORTS+=("$_p"); STUB_LOGS+=("$_lg")
done
python3 "$RUN/arm_stub.py" \
  "${STUB_PORTS[0]}" "${STUB_LOGS[0]}" \
  "${STUB_PORTS[1]}" "${STUB_LOGS[1]}" \
  "${STUB_PORTS[2]}" "${STUB_LOGS[2]}" \
  >"$RUN/arm_stub.out" 2>&1 &
verify_register_stub_pid "$!" "$RUN/arm_stub.py"
_stub_ready=0
for _w in $(seq 1 50); do
  if curl -fsS -m 1 -o /dev/null "http://127.0.0.1:${STUB_PORTS[0]}/__ping" 2>/dev/null \
     && curl -fsS -m 1 -o /dev/null "http://127.0.0.1:${STUB_PORTS[1]}/__ping" 2>/dev/null \
     && curl -fsS -m 1 -o /dev/null "http://127.0.0.1:${STUB_PORTS[2]}/__ping" 2>/dev/null; then
    _stub_ready=1; break
  fi
  kill -0 "$STUB_PID" 2>/dev/null || break
  sleep 0.1
done
(( _stub_ready == 1 )) || harness_error "三条请求映射上游桩未就绪：见 ${RUN}/arm_stub.out"
echo "arm stubs up pid=$STUB_PID ports=${STUB_PORTS[*]}"

declare -A CRED_FILE=()
for i in "${!ARMS[@]}"; do
  n="${ARMS[$i]}"
  CRED_FILE[$n]="$RUN/cred-${n}.token"
  new_token "${CRED_FILE[$n]}"
done

# ── 前置：UC-023 臂登记（失败则各断言如实连带红，不折 unknown）──────────────────
routes_ok=1
for i in "${!ARMS[@]}"; do
  n="${ARMS[$i]}"
  rc=0
  cli_run "pre-routes-add-${n}" routes add "$n" --endpoint "http://${UP_IP}:${STUB_PORTS[$i]}" --cred-file "${CRED_FILE[$n]}" --json || rc=$?
  [[ "$rc" == "0" ]] || routes_ok=0
done

# ── A1（正·白名单归一单跳）──────────────────────────────────────────────────
rc=0
cli_run "a1-set-mp" remap effort set mp --from xhigh --to high --json || rc=$?
set_mp_ok=0
if [[ "$rc" == "0" ]]; then
  set_mp_ok=1
  pass_ev "TC-UC029-A1 remap effort set mp：xhigh→high 注册成功（退出码 0）"
else
  fail_ev "TC-UC029-A1 remap effort set 注册映射项" \
    "退出码 0" "退出码 ${rc}；out=$(cli_out)" "$EV/${CLI_LAST}.out"
fi

effort_snapshot mp
if (( set_mp_ok == 1 )) && [[ "$SNAP_MAP" == "xhigh→high" ]]; then
  pass_ev "TC-UC029-A1 映射表回读逐字段一致（mp items=[${SNAP_MAP}]）"
else
  fail_ev "TC-UC029-A1 映射表回读逐字段一致" \
    "mp items=[xhigh→high]" "items=[${SNAP_MAP:-<空>}]（注册成功=${set_mp_ok}）" \
    "$EV/${CLI_LAST}.out"
fi

stub_reset "${STUB_LOGS[0]}"
effort_req "a1-mp-xhigh" mp xhigh
seen_effort="$(stub_last_field "${STUB_LOGS[0]}" effort)"
rewrite_ok=0
if (( set_mp_ok == 1 )) && [[ "$REQ_CODE" == "200" && "$seen_effort" == "high" ]]; then
  rewrite_ok=1
  pass_ev "TC-UC029-A1 白名单归一单跳：effort=xhigh 经映射一次改写为 high（上游白名单档；high 非改写源 ⇒ 无二跳）"
else
  fail_ev "TC-UC029-A1 白名单归一单跳（一次改写）" \
    "HTTP 200 且上游收到 output_config.effort=high" \
    "HTTP ${REQ_CODE}；上游收到 effort=${seen_effort:-<无转发>}（注册成功=${set_mp_ok}）" \
    "$(req_ev) 与 ${STUB_LOGS[0]}"
fi

# ── A2（正·承载语义锚定）────────────────────────────────────────────────────
# 三形态（reports/mixed-five-route-20260923/summary.md 五路由矩阵）：
#   mp xhigh→high / glm xhigh→max 且 max→max / df force=max（任意入参恒发 max）。

# [mp] xhigh→high（独立流量证据，不复用 A1 记录）。
stub_reset "${STUB_LOGS[0]}"
effort_req "a2-mp-xhigh" mp xhigh
seen="$(stub_last_field "${STUB_LOGS[0]}" effort)"
if (( set_mp_ok == 1 )) && [[ "$REQ_CODE" == "200" && "$seen" == "high" ]]; then
  pass_ev "TC-UC029-A2[mp] 锚定形态成立：xhigh→high"
else
  fail_ev "TC-UC029-A2[mp] 锚定形态 xhigh→high" \
    "HTTP 200 且上游 effort=high" "HTTP ${REQ_CODE}；上游 effort=${seen:-<无转发>}" \
    "$(req_ev) 与 ${STUB_LOGS[0]}"
fi

# [glm] xhigh→max 且 max→max（max→max 为恒等白名单登记）。
rc1=0; cli_run "a2-set-glm-xhigh" remap effort set glm --from xhigh --to max --json || rc1=$?
rc2=0; cli_run "a2-set-glm-max" remap effort set glm --from max --to max --json || rc2=$?
set_glm_ok=0
if [[ "$rc1" == "0" && "$rc2" == "0" ]]; then set_glm_ok=1; fi
effort_snapshot glm
if (( set_glm_ok == 1 )) && [[ "$SNAP_MAP" == "max→max|xhigh→max" ]]; then
  pass_ev "TC-UC029-A2[glm] 映射表 {xhigh→max, max→max} 登记成功且逐字段一致（max→max=恒等白名单登记）"
else
  fail_ev "TC-UC029-A2[glm] 映射表 {xhigh→max, max→max} 登记" \
    "两次 set 退出码 0 且 items=[max→max|xhigh→max]" \
    "set rc=${rc1}/${rc2}；items=[${SNAP_MAP:-<空>}]" \
    "$EV/${CLI_LAST}.out"
fi
for src in xhigh max; do
  stub_reset "${STUB_LOGS[1]}"
  effort_req "a2-glm-${src}" glm "$src"
  seen="$(stub_last_field "${STUB_LOGS[1]}" effort)"
  if (( set_glm_ok == 1 )) && [[ "$REQ_CODE" == "200" && "$seen" == "max" ]]; then
    pass_ev "TC-UC029-A2[glm] 锚定形态成立：${src}→max"
  else
    fail_ev "TC-UC029-A2[glm] 锚定形态 ${src}→max" \
      "HTTP 200 且上游 effort=max" "HTTP ${REQ_CODE}；上游 effort=${seen:-<无转发>}" \
      "$(req_ev) 与 ${STUB_LOGS[1]}"
  fi
done

# [df] force=max：任意入参（low / xhigh / 非法档 / 缺席）恒发 max。
rc=0
cli_run "a2-force-df" remap effort force df --to max --json || rc=$?
set_df_ok=0
if [[ "$rc" == "0" ]]; then set_df_ok=1; fi
effort_snapshot df
if (( set_df_ok == 1 )) && [[ "$SNAP_FORCE" == "max" ]]; then
  pass_ev "TC-UC029-A2[df] forceEffort=max 登记成功（list 回读 forceEffort=max）"
else
  fail_ev "TC-UC029-A2[df] force=max 登记" \
    "退出码 0 且 list 回读 forceEffort=max" \
    "退出码 ${rc}；forceEffort=[${SNAP_FORCE:-<空>}]" \
    "$EV/${CLI_LAST}.out"
fi
for src in low xhigh bogus __absent__; do
  stub_reset "${STUB_LOGS[2]}"
  if [[ "$src" == "__absent__" ]]; then
    effort_req "a2-df-absent" df ""
    srcdesc="effort 字段缺席"
  else
    effort_req "a2-df-${src}" df "$src"
    srcdesc="effort=${src}"
  fi
  seen="$(stub_last_field "${STUB_LOGS[2]}" effort)"
  if (( set_df_ok == 1 )) && [[ "$REQ_CODE" == "200" && "$seen" == "max" ]]; then
    pass_ev "TC-UC029-A2[df] force=max：${srcdesc} ⇒ 恒发 max"
  else
    fail_ev "TC-UC029-A2[df] force=max（${srcdesc} 恒发 max）" \
      "HTTP 200 且上游 effort=max" "HTTP ${REQ_CODE}；上游 effort=${seen:-<无转发>}" \
      "$(req_ev) 与 ${STUB_LOGS[2]}"
  fi
done

# ── A3（负·缺项 fail-closed）────────────────────────────────────────────────
# mp 表只有 xhigh→high：medium / 缺席 / 非法档均 4xx，且**上游零命中**（不回落、不透传）。
# 非空洞守卫：须先有 rewrite_ok（同臂映射项可正常 200），4xx 才是「缺项」而不是全盘不通。

for src in medium bogus __absent__; do
  stub_reset "${STUB_LOGS[0]}"
  if [[ "$src" == "__absent__" ]]; then
    effort_req "a3-mp-absent" mp ""
    srcdesc="effort 字段缺席（表无缺省项）"
  else
    effort_req "a3-mp-${src}" mp "$src"
    srcdesc="effort=${src}"
  fi
  hits="$(stub_hits "${STUB_LOGS[0]}")"
  code4xx=0
  [[ "$REQ_CODE" =~ ^4[0-9][0-9]$ ]] && code4xx=1
  if (( set_mp_ok == 1 && rewrite_ok == 1 && code4xx == 1 )) && [[ "$hits" == "0" ]]; then
    pass_ev "TC-UC029-A3 缺项 fail-closed：${srcdesc} ⇒ HTTP ${REQ_CODE}（不回落默认档、不透传，上游 0 命中）"
  else
    fail_ev "TC-UC029-A3 缺项 fail-closed（${srcdesc}）" \
      "HTTP 4xx 且上游 0 命中（同一请求映射的映射项可用为对照，见 A1）" \
      "HTTP ${REQ_CODE}（4xx 判定=${code4xx}）；上游命中=${hits}（注册=${set_mp_ok}，同一请求映射对照可用=${rewrite_ok}）" \
      "$(req_ev) 与 ${STUB_LOGS[0]}"
  fi
done

# ── A4（边界·逐臂钳制独立）──────────────────────────────────────────────────

# ① 同值入参不同出站：mp 与 glm 同发 xhigh，各自按本臂表钳制。
stub_reset "${STUB_LOGS[0]}"; stub_reset "${STUB_LOGS[1]}"
effort_req "a4-mp-xhigh" mp xhigh
mp_seen="$(stub_last_field "${STUB_LOGS[0]}" effort)"
effort_req "a4-glm-xhigh" glm xhigh
glm_seen="$(stub_last_field "${STUB_LOGS[1]}" effort)"
if (( set_mp_ok == 1 && set_glm_ok == 1 )) && [[ "$mp_seen" == "high" && "$glm_seen" == "max" ]]; then
  pass_ev "TC-UC029-A4[独立钳制] 同值入参 xhigh 逐条出站互不影响（mp→high / glm→max）"
else
  fail_ev "TC-UC029-A4[独立钳制] 逐条独立" \
    "mp 出站 high 且 glm 出站 max" \
    "mp→${mp_seen:-<无转发>}（HTTP $REQ_CODE 末腿）；glm→${glm_seen:-<无转发>}" \
    "${STUB_LOGS[0]} 与 ${STUB_LOGS[1]}"
fi

# ② 一臂拒绝不影响他臂：mp 的 medium 缺项 4xx 后，glm 的 max 照常 200→max。
stub_reset "${STUB_LOGS[0]}"
effort_req "a4-mp-reject" mp medium
mp_code="$REQ_CODE"
stub_reset "${STUB_LOGS[1]}"
effort_req "a4-glm-after-reject" glm max
glm_seen="$(stub_last_field "${STUB_LOGS[1]}" effort)"
code4xx=0
[[ "$mp_code" =~ ^4[0-9][0-9]$ ]] && code4xx=1
if (( set_glm_ok == 1 && rewrite_ok == 1 && code4xx == 1 )) && [[ "$REQ_CODE" == "200" && "$glm_seen" == "max" ]]; then
  pass_ev "TC-UC029-A4[拒绝隔离] mp 缺项拒绝（HTTP ${mp_code}）不影响 glm（HTTP 200，出站 max）"
else
  fail_ev "TC-UC029-A4[拒绝隔离] 一条请求映射拒绝不影响其余请求映射" \
    "mp medium→4xx 后 glm max→200 且出站 max" \
    "mp HTTP ${mp_code}；glm HTTP ${REQ_CODE} 出站 ${glm_seen:-<无转发>}" \
    "${STUB_LOGS[0]} 与 ${STUB_LOGS[1]}"
fi

# ③ force 与非 force 表两语义不混：df 对非法档恒发 max（对照 A3 的 mp 非法档 4xx）。
stub_reset "${STUB_LOGS[2]}"
effort_req "a4-df-bogus" df bogus
df_seen="$(stub_last_field "${STUB_LOGS[2]}" effort)"
if (( set_df_ok == 1 )) && [[ "$REQ_CODE" == "200" && "$df_seen" == "max" ]]; then
  pass_ev "TC-UC029-A4[force 独立] df 非法档恒发 max 与 mp 非法档 4xx 两语义不混"
else
  fail_ev "TC-UC029-A4[force 独立] 两语义不混" \
    "df bogus→HTTP 200 且恒发 max（对照 mp bogus→4xx）" \
    "HTTP ${REQ_CODE}；出站 ${df_seen:-<无转发>}" \
    "${STUB_LOGS[2]}"
fi

# ── A5（负·单层禁嵌套）──────────────────────────────────────────────────────
# 改写型 target 不得再作源、改写型 source 不得再作 target；违反注册被拒且原表不变。

expect_rejected() { # expect_rejected <tc-id> <说明> <label> <cli 参数...>
  local tc="$1" why="$2" label="$3"; shift 3
  effort_snapshot mp; local before="$SNAP_MAP"
  local rc=0 err
  cli_run "$label" "$@" || rc=$?
  err="$(cli_out)"
  effort_snapshot mp; local after="$SNAP_MAP"
  if [[ "$rc" == "1" && -n "${err// /}" && "$after" == "$before" ]]; then
    pass_ev "${tc} ${why}：注册被拒（退出码 1），原表不变（[${after:-<空>}]）"
  else
    fail_ev "${tc} ${why}" \
      "退出码 1 + 明确错误 + 原表不变" \
      "退出码 ${rc}；错误=[${err:-<空>}]；表前=[${before:-<空>}] 后=[${after:-<空>}]" \
      "$EV/${CLI_LAST}.out 与 $EV/${CLI_LAST}.err"
  fi
}

# (a) high 是 xhigh→high（改写型）的 target ⇒ 不得再作源（否则 xhigh→high→max 二跳）。
expect_rejected "TC-UC029-A5[target 再作源]" "high 是改写型 target，不得再作源（单跳）" "a5-target-as-source" \
  remap effort set mp --from high --to max --json
# (b) xhigh 是改写型 source ⇒ 不得再作 target（否则 q-clamp→xhigh→high 二跳）。
expect_rejected "TC-UC029-A5[source 再作 target]" "xhigh 是改写型 source，不得再作 target（单跳）" "a5-source-as-target" \
  remap effort set mp --from q-clamp --to xhigh --json

# (c) 对照非空：glm 的 {xhigh→max, max→max} 恒等登记被接受（A2 已断言）——证明禁嵌套
#     只封「可观察的二跳」，不误杀恒等白名单登记。
effort_snapshot glm
if (( set_glm_ok == 1 )) && [[ "$SNAP_MAP" == "max→max|xhigh→max" ]]; then
  pass_ev "TC-UC029-A5[恒等豁免对照] glm {xhigh→max, max→max} 在册（恒等登记不构成嵌套；封禁仅针对可观察二跳）"
else
  fail_ev "TC-UC029-A5[恒等豁免对照]" \
    "glm 表 [max→max|xhigh→max] 在册" "表=[${SNAP_MAP:-<空>}]（登记成功=${set_glm_ok}）" \
    "$EV/${CLI_LAST}.out"
fi

printf 'report  remap-effort: arms=%s stub-ports=%s evidence=%s\n' "${ARMS[*]}" "${STUB_PORTS[*]}" "$EV"
verify_finish

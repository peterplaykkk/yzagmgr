#!/usr/bin/env bash
# scripts/verify/remap-model.sh —— UC-028 remap model 单层映射。
#
# 为某臂注册模型映射 `alias → target`（单层：target 是最终上游模型名）；客户端请求携带
# alias，本地 router 在上游选择路径**改写一次**为 target 后发出；拒绝链式（target 自身
# 带路由/映射）与自映射；映射重启存活；仅作用于本地 router 上游（非 router 前缀逐字节不变）。
#
# ── 本脚本冻结的可执行契约（能力未交付 ⇒ 全部如实红；实现方按此对齐后本脚本转绿）──
# 管理面（经 bin/yzagmgr CLI；形态沿 UC-023 同一套 routes/remp 家族）：
#   yzagmgr remap model set    <arm> --alias <alias> --target <target> --json
#   yzagmgr remap model list   <arm> --json ⇒ {"ok":true,"data":{"items":[{alias,target},…]}}
#   （UC-023 的 routes add 先登记臂：name/endpoint/credRef，见 api-v1.md §7.1 契约）
#   语义：
#     - alias 登记在 <arm> 上即成为该臂的请求侧选臂名；出站 model 一次改写为 target；
#     - **单层**：target 不得再被二次映射/路由（拒绝链式 = target 自身已带路由或映射）；
#     - 拒绝自映射（alias == target，UC-017 A3 同语义）；拒绝类均为业务拒绝 ⇒ 退出码 1，
#       且**原映射不变**。
#   记账双名（UC-017 A10 口径）：GET /api/v1/usage/requests 的记录保留
#   modelRequested=<alias> / modelUpstream=<target>。
# 数据面（本地 router）：
#   POST /router/v1/messages，`model` = alias（或路由名）⇒ 命中该臂并改写出站 model；
#   非 router 前缀请求的 model **逐字节不变**（A5 直连腿用零映射的 /openai 前缀量——
#   /claude 自带 UC-017 A3 家族映射，对任何模型名都会换名，不能用来判 remap 作用域）。
#
# 语义锚（R107/R108 已实证形态）：reports/mixed-five-route-20260923/
#   alias=claude-fable-5-mp → target=mimo-v2.6-pro（见 summary.md 五路由矩阵首行）。
#
# 现状红点（2026-09-24 覆盖审计 D3，先红后绿）：remap 的存储/REST/CLI **全缺**——
# `yzagmgr remap …` 落「未知子命令」用法错误（退出码 2）；链式/自映射**零校验**能力待建。
# 本脚本在此状态下真跑并逐断言如实红（verdict FAIL），不得 skip/todo 冒充。
#
# 断言 ↔ UC-028 验收行：
#   TC-UC028-A1 单层改写（alias→target 出站替换一次 + 记账双名）
#   TC-UC028-A2 拒绝链式（target 自身带映射 / 带路由 ⇒ 退出码 1 且原映射不变）
#   TC-UC028-A3 拒绝自映射（alias == target ⇒ 退出码 1 且原映射不变）
#   TC-UC028-A4 重启存活（映射逐字段一致 + 改写仍生效）
#   TC-UC028-A5 作用域仅本地 router 上游（非 router 前缀 model 逐字节不变）
#
# 用法: remap-model.sh
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib.sh
. "$HERE/lib.sh"

unset ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN ANTHROPIC_BASE_URL ANTHROPIC_MODEL 2>/dev/null || true

verify_init remap-model
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
ROUTER_LAST=""
ROUTER_CODE=""
SNAPSHOT=""

ARM=mimo
ALIAS=claude-fable-5-mp          # 语义锚：reports/mixed-five-route-20260923 首行
TARGET=mimo-v2.6-pro

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

cli_run() { # cli_run <label> <yzagmgr 参数...>
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

router_post() { # router_post <label> <json-body>：数据面 /router/v1/messages → $ROUTER_CODE/$ROUTER_LAST
  local label="$1" body="$2" base
  EVN=$((EVN + 1))
  printf -v base '%02d-%s' "$EVN" "$label"
  ROUTER_CODE="$(curl -sS -o "$EV/${base}.out" -w '%{http_code}' -m 15 -X POST \
    -H 'Content-Type: application/json' --data-binary "$body" \
    "http://127.0.0.1:${PROXY_PORT}/router/v1/messages" 2>"$EV/${base}.err" || true)"
  ROUTER_LAST="$base"
}
router_ev() { printf '%s' "$EV/${ROUTER_LAST}.out"; }

direct_post() { # direct_post <label> <prefix> <json-body>：非 router 前缀（直连腿）→ $ROUTER_CODE/$ROUTER_LAST
  local label="$1" prefix="$2" body="$3" base
  EVN=$((EVN + 1))
  printf -v base '%02d-%s' "$EVN" "$label"
  ROUTER_CODE="$(curl -sS -o "$EV/${base}.out" -w '%{http_code}' -m 15 -X POST \
    -H 'Content-Type: application/json' --data-binary "$body" \
    "http://127.0.0.1:${PROXY_PORT}/${prefix}/v1/messages" 2>"$EV/${base}.err" || true)"
  ROUTER_LAST="$base"
}

remap_snapshot() { # 当前臂的映射表排序串（alias→target|…）→ $SNAPSHOT
  local rc=0
  cli_run "remap-list" remap model list "$ARM" --json || rc=$?
  SNAPSHOT="$(cli_jget '"|".join(sorted(x["alias"]+"→"+x["target"] for x in d["data"]["items"]))')"
}

# ── 夹具（原与 router-three-arm.sh 同构——该脚本已随 design §4-I-12 删除；0600 哨兵令牌 + auth 摘要 + 双口上游桩）────

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

# 两个桩口：P_ARM = 该臂上游（router 出站）；P_DIR = 直连腿上游（非 router 前缀 /claude/）。
P_ARM="$(pick_free_port)"
P_DIR="$(pick_free_port)"
LOG_ARM="$RUN/stub-arm.log"
LOG_DIR="$RUN/stub-direct.log"
stub_reset "$LOG_ARM"; stub_reset "$LOG_DIR"
python3 "$RUN/arm_stub.py" "$P_ARM" "$LOG_ARM" "$P_DIR" "$LOG_DIR" >"$RUN/arm_stub.out" 2>&1 &
verify_register_stub_pid "$!" "$RUN/arm_stub.py"
_stub_ready=0
for _w in $(seq 1 50); do
  if curl -fsS -m 1 -o /dev/null "http://127.0.0.1:${P_ARM}/__ping" 2>/dev/null \
     && curl -fsS -m 1 -o /dev/null "http://127.0.0.1:${P_DIR}/__ping" 2>/dev/null; then
    _stub_ready=1; break
  fi
  kill -0 "$STUB_PID" 2>/dev/null || break
  sleep 0.1
done
(( _stub_ready == 1 )) || harness_error "上游桩未就绪：见 ${RUN}/arm_stub.out"
echo "stubs up pid=$STUB_PID arm-port=${P_ARM} direct-port=${P_DIR}"

CRED="$RUN/cred-${ARM}.token"
new_token "$CRED"

# ── 前置：UC-023 臂登记（remap 的承载；失败则各断言如实连带红，不折 unknown）────────
rc=0
cli_run "pre-routes-add" routes add "$ARM" --endpoint "http://${UP_IP}:${P_ARM}" --cred-file "$CRED" --json || rc=$?
route_ok=0
[[ "$rc" == "0" ]] && route_ok=1

# ── A1（正·单层改写）────────────────────────────────────────────────────────
rc=0
cli_run "a1-remap-set" remap model set "$ARM" --alias "$ALIAS" --target "$TARGET" --json || rc=$?
set_ok=0
if [[ "$rc" == "0" ]]; then
  set_ok=1
  pass_ev "TC-UC028-A1 remap model set ${ARM}：alias=${ALIAS} → target=${TARGET} 注册成功（退出码 0）"
else
  fail_ev "TC-UC028-A1 remap model set 注册 alias→target" \
    "退出码 0" "退出码 ${rc}；out=$(cli_out)" "$EV/${CLI_LAST}.out"
fi

remap_snapshot
if (( set_ok == 1 )) && [[ "$SNAPSHOT" == "${ALIAS}→${TARGET}" ]]; then
  pass_ev "TC-UC028-A1 映射回读逐字段一致（items=[${SNAPSHOT}]）"
else
  fail_ev "TC-UC028-A1 映射回读逐字段一致" \
    "items=[${ALIAS}→${TARGET}]" "items=[${SNAPSHOT:-<空>}]（注册成功=${set_ok}）" \
    "$EV/${CLI_LAST}.out"
fi

stub_reset "$LOG_ARM"
router_post "a1-rewrite" "{\"model\":\"${ALIAS}\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"
seen_model="$(stub_last_field "$LOG_ARM" model)"
if (( set_ok == 1 )) && [[ "$ROUTER_CODE" == "200" && "$seen_model" == "$TARGET" && "$seen_model" != "$ALIAS" ]]; then
  pass_ev "TC-UC028-A1 单层改写：客户端发 ${ALIAS} ⇒ 上游收到 ${seen_model}（改写一次为 target，非空洞：出站≠入站）"
else
  fail_ev "TC-UC028-A1 出站模型名替换（单层改写一次）" \
    "HTTP 200 且上游收到 model=${TARGET}（≠${ALIAS}）" \
    "HTTP ${ROUTER_CODE}；上游收到 model=${seen_model:-<无转发>}（注册成功=${set_ok}）" \
    "$(router_ev) 与 ${LOG_ARM}"
fi

sleep 0.3
usage_rec="$(curl -sS -m 10 "http://127.0.0.1:${API_PORT}/api/v1/usage/requests?limit=5" \
  -H "X-Agmgr-Token: $(cat "$TOKEN_FILE")" 2>/dev/null \
  | jget - 'next((str(b.get("modelRequested",""))+"→"+str(b.get("modelUpstream","")) for b in d["data"]["items"] if b.get("modelRequested")=="'"${ALIAS}"'"), "")' || true)"
if (( set_ok == 1 )) && [[ "$usage_rec" == "${ALIAS}→${TARGET}" ]]; then
  pass_ev "TC-UC028-A1 记账保留双名（UC-017 A10）：modelRequested=${ALIAS} / modelUpstream=${TARGET}"
else
  fail_ev "TC-UC028-A1 记账保留双名（UC-017 A10）" \
    "usage 记录含 ${ALIAS}→${TARGET} 双名" \
    "usage 取到 [${usage_rec:-<无匹配记录>}]（注册成功=${set_ok}）" \
    "$EV/${CLI_LAST}.out 与 usage API"
fi

# ── A2（负·拒绝链式）────────────────────────────────────────────────────────
# target 自身已带映射 / 带路由 ⇒ 注册被拒（退出码 1）、报明确错误、原映射不变。

expect_rejected() { # expect_rejected <tc-id> <说明> <label> <cli 参数...>
  local tc="$1" why="$2" label="$3"; shift 3
  remap_snapshot; local before="$SNAPSHOT"
  local rc=0 err
  cli_run "$label" "$@" || rc=$?
  err="$(cli_out)"
  remap_snapshot; local after="$SNAPSHOT"
  if [[ "$rc" == "1" && -n "${err// /}" && "$after" == "$before" ]]; then
    pass_ev "${tc} ${why}：注册被拒（退出码 1，业务拒绝），错误明确，原映射不变（[${after:-<空>}]）"
  else
    fail_ev "${tc} ${why}" \
      "退出码 1 + 明确错误 + 原映射不变" \
      "退出码 ${rc}；错误=[${err:-<空>}]；映射前=[${before:-<空>}] 后=[${after:-<空>}]" \
      "$EV/${CLI_LAST}.out 与 $EV/${CLI_LAST}.err"
  fi
}

# (a) target 自身带映射：${ALIAS} 已有映射 ⇒ 又把它当 target。
expect_rejected "TC-UC028-A2[带映射]" "target 自身带映射（链式 ${ALIAS} 已 → ${TARGET}）" "a2-chain-mapped" \
  remap model set "$ARM" --alias x-chain --target "$ALIAS" --json
# (b) target 自身带路由：臂名 ${ARM} 是命名路由。
expect_rejected "TC-UC028-A2[带路由]" "target 自身带路由（${ARM} 是请求映射名）" "a2-chain-routed" \
  remap model set "$ARM" --alias x-route --target "$ARM" --json

# ── A3（负·拒绝自映射）──────────────────────────────────────────────────────
expect_rejected "TC-UC028-A3" "自映射 alias==target（UC-017 A3 同语义）" "a3-self-map" \
  remap model set "$ARM" --alias self-map --target self-map --json

# ── A4（边界·重启存活）──────────────────────────────────────────────────────
remap_snapshot
before_restart="$SNAPSHOT"
before_ev="$EV/${CLI_LAST}.out"
cp "$RUN/daemon.log" "$RUN/daemon.log.pre-restart" 2>/dev/null || true
kill "$DAEMON_PID" 2>/dev/null || true
wait "$DAEMON_PID" 2>/dev/null || true
DAEMON_PID=""
start_isolated_daemon
remap_snapshot
after_restart="$SNAPSHOT"
if (( set_ok == 1 )) && [[ -n "$before_restart" && "$before_restart" == "$after_restart" ]]; then
  pass_ev "TC-UC028-A4 重启存活：映射逐字段一致（[${after_restart}]）"
else
  fail_ev "TC-UC028-A4 重启存活（映射逐字段一致）" \
    "重启前=重启后且映射在册" \
    "前=[${before_restart:-<空>}]/后=[${after_restart:-<空>}]（注册成功=${set_ok}）" \
    "${before_ev} 与 $EV/${CLI_LAST}.out、${RUN}/daemon.log.pre-restart、${RUN}/daemon.log"
fi
if (( set_ok == 1 )); then
  stub_reset "$LOG_ARM"
  router_post "a4-rewrite-after-restart" "{\"model\":\"${ALIAS}\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"
  seen_model="$(stub_last_field "$LOG_ARM" model)"
  if [[ "$ROUTER_CODE" == "200" && "$seen_model" == "$TARGET" ]]; then
    pass_ev "TC-UC028-A4 重启后改写仍生效（${ALIAS} ⇒ ${seen_model}，无内存态丢失）"
  else
    fail_ev "TC-UC028-A4 重启后改写仍生效" \
      "HTTP 200 且上游收到 ${TARGET}" \
      "HTTP ${ROUTER_CODE}；上游收到 ${seen_model:-<无转发>}" \
      "$(router_ev) 与 ${LOG_ARM}"
  fi
else
  fail_ev "TC-UC028-A4 重启后改写仍生效" "注册成功后重启，改写仍生效" "注册未成功，无法形成判据" "$EV/commands.log"
fi

# ── A5（边界·作用域仅本地 router 上游）──────────────────────────────────────
# remap 只作用于本地 router 的上游选择路径；直连/非 router 前缀请求的 model 逐字节不变。
# 直连腿走 **/openai 前缀**（PUT /proxy/routes/openai 指向直连桩；既有能力）。
#
# 为什么不用 /claude 做直连腿（实测教训，2026-09-24）：/claude 前缀自带 UC-017 A3 的
# 家族映射（internal/api/modelmap.go 从受管计划字节装配 keywords+Default，实测连
# `scope-probe-*` 这类名字也会被 Default 兜到 deepseek-flash）——「逐字节不变」在
# /claude 上**对任何模型名都不成立**，与 remap 无关，量 remap 的作用域必须挑一张
# 零映射的前缀。modelMappingFor 只给 claude/claude-desktop 装表（internal/api/modelmap.go），
# /openai 前缀恒零映射 ⇒ 逐字节不变在这里才是可达成且有判别力的断言。
probe_alias=scope-probe-9f2a
probe_target=scope-probe-upstream
rc=0
cli_run "a5-probe-set" remap model set "$ARM" --alias "$probe_alias" --target "$probe_target" --json || rc=$?
probe_ok=0
if [[ "$rc" == "0" ]]; then probe_ok=1; fi

curl -sS -m 10 -X PUT -H "X-Agmgr-Token: $(cat "$TOKEN_FILE")" -H 'Content-Type: application/json' \
  --data-binary "{\"upstream\":\"http://${UP_IP}:${P_DIR}\"}" \
  "http://127.0.0.1:${API_PORT}/api/v1/proxy/routes/openai" >"$EV/direct-route.put.json" 2>&1 || true

stub_reset "$LOG_DIR"
direct_post "a5-direct" openai "{\"model\":\"${probe_alias}\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"
direct_code="$ROUTER_CODE"
direct_model="$(stub_last_field "$LOG_DIR" model)"
stub_reset "$LOG_ARM"
router_post "a5-router-contrast" "{\"model\":\"${probe_alias}\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"
router_model="$(stub_last_field "$LOG_ARM" model)"
if (( probe_ok == 1 )) && [[ "$direct_model" == "$probe_alias" && "$router_model" == "$probe_target" ]]; then
  pass_ev "TC-UC028-A5 作用域仅请求映射：/openai 前缀（零映射）逐字节直呼 ${probe_alias}（不改写），/router/ 路径改写 ${probe_target}（两侧对照成立，差异只来自 remap）"
else
  fail_ev "TC-UC028-A5 作用域仅请求映射上游" \
    "探针 ${probe_alias} 注册成功；非 router 前缀 model 逐字节不变（=${probe_alias}）且 router 路径改写（=${probe_target}）" \
    "注册 rc=${rc}（probe_ok=${probe_ok}）；直连腿（/openai，零映射前缀）HTTP ${direct_code} 上游收到 [${direct_model:-<无转发>}]；router 腿 HTTP ${ROUTER_CODE} 上游收到 [${router_model:-<无转发>}]" \
    "${LOG_DIR} 与 ${LOG_ARM}、$(router_ev)"
fi

printf 'report  remap-model: arm=%s alias=%s target=%s stubs=%s evidence=%s\n' "$ARM" "$ALIAS" "$TARGET" "${P_ARM},${P_DIR}" "$EV"
verify_finish

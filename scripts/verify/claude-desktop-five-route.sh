#!/usr/bin/env bash
# scripts/verify/claude-desktop-five-route.sh —— UC-030 claude-desktop mixed 五路由（配置级）。
#
# 在 v2 多臂路由模型（UC-023 routes × UC-028 remap model × UC-029 remap effort）下生成
# claude-desktop 的**一个**配置：3P 档案恰含五条目 mp / mf / z / zf / df，逐条目断言
# 目标、supports1m、maxEffort 三元；一次 apply 后断言档案字节形态；restore 逐字节回起点；
# 真机 Claude Desktop 接受（重启后列出五标签）不可自动化 ⇒ 登记 `unknown` + 人工步骤，**不折 pass**。
#
# ── 本脚本冻结的可执行契约（能力未交付 ⇒ 全部如实红；实现方按此对齐后本脚本转绿）──
# 管理面（配置一律经 bin/yzagmgr CLI；臂/别名/effort 三表沿 UC-023/028/029 冻结家族，
# 见 remap-model.sh / remap-effort.sh 头注释，本脚本不重复展开；原 router-three-arm.sh
# 已随 design §4-I-12 删除）：
#   yzagmgr routes add <arm> --endpoint <url> --cred-file <0600文件> --json   # 三臂：mimo / glm / direct
#   yzagmgr remap model set <arm> --alias <alias> --target <target> \
#        [--supports1m] [--prefer1m] --json
#       ★ 本脚本相对 remap-model.sh 冻结面的 **additive 扩展**：可选能力位旗标
#         （缺省 = 不声明；items 回读增 supports1m/prefer1m 字段）。桌面 schema 拒绝
#         `[1m]` 后缀（R108 实证：1M 由能力位表达），故 mp 的 `[1m]` 变体承载 =
#         裸名条目 + supports1m:true，条目名恒为裸名。
#   yzagmgr remap effort set <arm> --from <src> --to <dst> --json
#   yzagmgr remap effort force <arm> --to <dst> --json
# 多条目派生（本脚本核心冻结面）：
#   本地 router 多臂模型**在册**（每臂：路由 + 别名 remap + effort 归属齐备）时，
#   `yzagmgr apply claude-desktop --profile <受管卡>` 的网关档目标档案（= apply 后盘上
#   `Claude-3p/configLibrary/<PROFILE_ID>.json`，经 `yzagmgr tools show claude-desktop --json`
#   的 paths 发现，不硬编码）**每条 remap 别名恰一条目**：
#     name            = 别名（请求侧选臂名，裸名无 [1m] 后缀）
#     labelOverride   = 目标模型（沿 v2 ProfileBytes 既有约定：labelOverride 承载上游模型名）
#     supports1m/prefer1m = remap model set 注册的能力位（false/缺席不伪造键）
#     maxEffort       = "max"（v2 MaxEffort 常量口径；desktop-effort-oracle 五名全
#                       `low,medium,high*,xhigh,max` 阶梯不得被条目上限切断）
#     anthropicFamilyTier = "fable"
#   条目集**恰**等于别名集（无兜底条目、无多余条目）；逐条目键集 ⊆ app.asar 接受集
#   （name/labelOverride/supports1m/prefer1m/anthropicFamilyTier/maxEffort，写未知键会被
#   schema 静默丢弃）。路由模型为空/不完整时保持现行单条目行为或业务拒绝——**不得**
#   产出残缺五条目。
#   载体语义锚（已实证，语义照抄不重推）：reports/mixed-five-route-20260923/
#   （R107/R108 五路由矩阵 + profile-candidate 打码件）。
# 数据面（本地 router，负向段）：
#   POST /router/v1/messages，`model` = 别名命中该条目；未知别名/缺条目 ⇒ 显式 4xx
#   且全部上游桩零命中（无兜底条目可落）；错误路径 ⇒ 4xx（UC-007 前缀口径）。
# restore：裸 `yzagmgr restore --json`（= 撤销最近一次写入，cli-rewrite.sh 冻结面同口径）
#   ⇒ 全部受管路径逐字节回起点（exists/size/sha256）。
#
# 现状红点（2026-09-24 覆盖审计 D5，先红后绿）：
#   ① routes/remap 子命令全缺（CLI 落「未知子命令」用法错误，退出码 2）⇒ 多臂模型
#      结构性不可达；
#   ② claudedesktop 写链**恒单条目**（internal/family/claudedesktop ProfileBytes：
#      inferenceModels 恒 1 条）——五条目派生待建；
#   ③ 真机 app 接受面不可自动化（UC-030 A4 预期行为）。
#   本脚本在此状态下真跑并逐断言如实红（verdict FAIL，退出码 1），不得 skip/todo 冒充。
#
# 断言 ↔ UC-030 验收行：
#   TC-UC030-A1 五条目目标（恰五条目 + 别名集恰等 + 逐条目目标/裸名 + 档案字节形态）
#   TC-UC030-A2 逐条目 supports1m/maxEffort 三元（+ effortMap 钳制语义锚回读）
#   TC-UC030-A3 条目独立无兜底（未知别名 4xx + 上游零命中 + 错误路径 4xx）
#   TC-UC030-A4 真机 app 接受（不可自动化格 ⇒ unknown + 人工步骤，不折 pass）
#
# 用法: claude-desktop-five-route.sh
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib.sh
. "$HERE/lib.sh"

unset ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN ANTHROPIC_BASE_URL ANTHROPIC_MODEL 2>/dev/null || true

verify_init claude-desktop-five-route
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

# 五条目契约常量（mp / mf / z / zf /df；语义锚 reports/mixed-five-route-20260923/）。
declare -a ARM_OF=(mimo mimo glm glm direct)
declare -a ALIAS=(claude-fable-5-mp claude-fable-5-mf claude-fable-5-z claude-fable-5-zf claude-fable-5-df)
declare -a TARGET=(mimo-v2.6-pro mimo-v2.6-flash glm-5.3 glm-5.3-flash deepseek-flash)
declare -a S1M=(true true false true true)   # z 的 supports1m 缺席=false 语义（R108 实证）

# ── 证据与三态辅助（失败一律打印「期望/实际/证据路径」；输出有界）──────────────

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

# ── CLI / 数据面调用（输出逐次落独立证据文件）─────────────────────────────────

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

router_post() { # router_post <label> <json-body>：数据面打 /router/v1/messages
  local label="$1" body="$2" base
  EVN=$((EVN + 1))
  printf -v base '%02d-%s' "$EVN" "$label"
  ROUTER_CODE="$(curl -sS -o "$EV/${base}.out" -w '%{http_code}' -m 15 -X POST \
    -H 'Content-Type: application/json' --data-binary "$body" \
    "http://127.0.0.1:${PROXY_PORT}/router/v1/messages" 2>"$EV/${base}.err" || true)"
  ROUTER_LAST="$base"
}
router_path_post() { # router_path_post <label> <path> <json-body>：任意路径负向探针
  local label="$1" path="$2" body="$3" base
  EVN=$((EVN + 1))
  printf -v base '%02d-%s' "$EVN" "$label"
  ROUTER_CODE="$(curl -sS -o "$EV/${base}.out" -w '%{http_code}' -m 15 -X POST \
    -H 'Content-Type: application/json' --data-binary "$body" \
    "http://127.0.0.1:${PROXY_PORT}${path}" 2>"$EV/${base}.err" || true)"
  ROUTER_LAST="$base"
}

bytes_sig() { # 内容签名（存在性 + 逐文件 sha256；cli-rewrite.sh 同法）
  local out="" p
  while IFS= read -r p; do
    [[ -z "$p" ]] && continue
    if [[ -f "$p" ]]; then out="${out}${p}=$(sha256_file "$p")|"; else out="${out}${p}=<absent>|"; fi
  done < <(printf '%s\n' "$@" | LC_ALL=C sort)
  printf '%s' "${out%|}"
}

# ── 夹具：三臂上游桩（一个进程绑 3 口）+ 凭据哨兵（0600）──────────────────────

cat >"$RUN/arm_stub.py" <<'PY'
import json, sys, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PAIRS = list(zip(sys.argv[1::2], sys.argv[2::2]))  # port log port log ...

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
            note(log, {"port": int(port), "path": self.path,
                       "model": str(doc.get("model") or ""), "bytes": n})
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

stub_hits() { # stub_hits <log>：请求条数（无文件/空文件 = 0）
  [[ -s "$1" ]] || { printf '0'; return 0; }
  wc -l <"$1" | tr -d ' '
}
stub_total_hits() { # 三桩命中合计
  local t=0 h
  for h in "${STUB_LOGS[@]}"; do
    t=$((t + $(stub_hits "$h")))
  done
  printf '%s' "$t"
}

new_token() { # new_token <file>：写随机哨兵令牌，chmod 600
  python3 -c 'import secrets, sys, os
p = sys.argv[1]
open(p, "w").write("yzagmgr-test-" + secrets.token_hex(16) + "\n")
os.chmod(p, 0o600)' "$1"
}

UP_IP="$(host_ip || true)"
[[ -n "$UP_IP" ]] || die "本机没有非回环 IPv4：上游端点禁止回环（UC-007 A13 口径），无法起 hermetic 上游桩"

declare -a ARMS=(mimo glm direct)
declare -a STUB_PORTS=() STUB_LOGS=()
for _n in "${ARMS[@]}"; do
  _p="$(pick_free_port)"
  _lg="$RUN/stub-${_n}.log"
  : >"$_lg"
  STUB_PORTS+=("$_p")
  STUB_LOGS+=("$_lg")
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
(( _stub_ready == 1 )) || harness_error "三条请求映射上游桩未就绪（进程存活=$(kill -0 "$STUB_PID" 2>/dev/null && echo yes || echo no)）：见 ${RUN}/arm_stub.out"
echo "arm stubs up pid=$STUB_PID ports=${STUB_PORTS[*]}"

declare -A CRED_FILE=() SUB_ENDPOINT=()
for i in "${!ARMS[@]}"; do
  n="${ARMS[$i]}"
  CRED_FILE[$n]="$RUN/cred-${n}.token"
  new_token "${CRED_FILE[$n]}"
  SUB_ENDPOINT[$n]="http://${UP_IP}:${STUB_PORTS[$i]}"
done

# ── 前置：多臂路由模型登记（UC-023/028/029 家族；失败 = 判据不成立红的根因）─────

prereq_ok=1
for i in "${!ARMS[@]}"; do
  n="${ARMS[$i]}"
  rc=0; cli_run "route-add-${n}" routes add "$n" --endpoint "${SUB_ENDPOINT[$n]}" --cred-file "${CRED_FILE[$n]}" --json || rc=$?
  if [[ "$rc" != "0" ]]; then
    prereq_ok=0
    echo "note: routes add ${n} 失败（rc=${rc}）：$(cli_out)"
  fi
done
declare -A ARM_REF=()
for i in "${!ALIAS[@]}"; do
  a="${ALIAS[$i]}"; t="${TARGET[$i]}"; arm="${ARM_OF[$i]}"; s1="${S1M[$i]}"
  flags=(--alias "$a" --target "$t")
  if [[ "$s1" == "true" ]]; then flags+=(--supports1m --prefer1m); fi
  rc=0; cli_run "remap-${a}" remap model set "$arm" "${flags[@]}" --json || rc=$?
  if [[ "$rc" != "0" ]]; then
    prereq_ok=0
    echo "note: remap model set ${arm} ${a} 失败（rc=${rc}）：$(cli_out)"
  fi
done
rc=0; cli_run "effort-mimo" remap effort set mimo --from xhigh --to high --json || rc=$?
[[ "$rc" == "0" ]] || { prereq_ok=0; echo "note: remap effort set mimo 失败（rc=${rc}）：$(cli_out)"; }
rc=0; cli_run "effort-glm-xhigh" remap effort set glm --from xhigh --to max --json || rc=$?
[[ "$rc" == "0" ]] || { prereq_ok=0; echo "note: remap effort set glm/xhigh 失败（rc=${rc}）：$(cli_out)"; }
rc=0; cli_run "effort-glm-max" remap effort set glm --from max --to max --json || rc=$?
[[ "$rc" == "0" ]] || { prereq_ok=0; echo "note: remap effort set glm/max 失败（rc=${rc}）：$(cli_out)"; }
rc=0; cli_run "effort-df-force" remap effort force direct --to max --json || rc=$?
[[ "$rc" == "0" ]] || { prereq_ok=0; echo "note: remap effort force df 失败（rc=${rc}）：$(cli_out)"; }
echo "prereq_ok=${prereq_ok}（多映射路由模型三表登记；0 = 判据不成立，连带断言如实红）"

# ── 受管路径发现 + 起点夹具（用户自有内容；restore 必须逐字节回这里）────────────

rc=0; cli_run "tools-show" tools show claude-desktop --json || rc=$?
mpid="$(cli_jget 'next((p["id"] for p in d["data"]["profiles"]["items"] if p.get("kind")=="managed"), "")')"
paths_raw="$(cli_jget '"|".join(p["path"] for p in d["data"]["tool"]["paths"])')"
if [[ "$rc" != "0" || -z "$mpid" || -z "$paths_raw" ]]; then
  die "cli tools show claude-desktop 未给出受管卡/路径（rc=${rc} mpid=${mpid:-<空>}）：$(cli_out)"
fi
IFS='|' read -r -a MANAGED_PATHS <<<"$paths_raw"
PROFILE_FILE=""
META_FILE=""
declare -a MARKER_FILES=()
for p in "${MANAGED_PATHS[@]}"; do
  case "$p" in
    */configLibrary/_meta.json) META_FILE="$p" ;;
    */configLibrary/*.json) PROFILE_FILE="$p" ;;
    *) MARKER_FILES+=("$p") ;;
  esac
done
[[ -n "$PROFILE_FILE" && -n "$META_FILE" ]] || die "受管路径里找不到 3P 档案/元数据（paths=${paths_raw}）"
echo "managed pid=$mpid profile=$PROFILE_FILE meta=$META_FILE markers=${MARKER_FILES[*]}"

# 起点夹具：用户自有字节（非空洞往返的可回起点）。
for p in "${MARKER_FILES[@]}"; do
  mkdir -p "$(dirname "$p")"
  printf '{\n  "mcpServers": { "user-owned": { "command": "keep-me" } }\n}\n' >"$p"
done
mkdir -p "$(dirname "$PROFILE_FILE")"
printf '{\n  "userProfileNote": "pre-existing user bytes"\n}\n' >"$PROFILE_FILE"
printf '{\n  "entries": [ { "id": "user-entry", "name": "User" } ]\n}\n' >"$META_FILE"
START_SIG="$(bytes_sig "${MANAGED_PATHS[@]}")"

# ── 一次 apply（多条目派生的核心被验动作）─────────────────────────────────────

rc=0; cli_run "apply" apply claude-desktop --profile "$mpid" --json || rc=$?
astatus="$(cli_jget 'str(d["data"].get("status",""))')"
apply_ok=0
if [[ "$rc" == "0" && ( "$astatus" == "applied" || "$astatus" == "unchanged" ) ]]; then
  apply_ok=1
else
  echo "note: apply claude-desktop 失败（rc=${rc} status=${astatus}）：$(cli_out)"
fi
MID_SIG="$(bytes_sig "${MANAGED_PATHS[@]}")"

arch_entries() { jget "$(cat "$PROFILE_FILE" 2>/dev/null || true)" "$1"; }

# ── TC-UC030-A1（正·五条目目标）──────────────────────────────────────────────
# 一个 claude-desktop 配置恰含五条目；目标 mp→mimo-v2.6-pro（含 [1m] 变体）、
# mf→mimo-v2.6-flash、z→glm-5.3、zf→glm-5.3-flash、df→deepseek-flash；
# mp 变体承载 = 裸名条目 + supports1m（桌面 schema 拒 [1m] 后缀）。

count="$(arch_entries 'len(d.get("inferenceModels") or [])')"
if [[ "$count" == "5" ]]; then
  pass_ev "TC-UC030-A1[恰五条目] 档案 inferenceModels 恰 5 条（${PROFILE_FILE}）"
else
  fail_ev "TC-UC030-A1[恰五条目] 一个配置恰含五条目" \
    "inferenceModels 长度=5（请求映射模型登记成功后按别名集派生）" \
    "长度=${count:-<解析失败>}（prereq_ok=${prereq_ok}，apply_ok=${apply_ok}；路由模型不完整时契约禁止残缺五条目）" \
    "${EV}/${CLI_LAST}.out 与 ${PROFILE_FILE}"
fi

names_got="$(arch_entries '"|".join(sorted(e.get("name","") for e in (d.get("inferenceModels") or [])))')"
names_want="$(printf '%s\n' "${ALIAS[@]}" | LC_ALL=C sort | paste -sd'|' -)"
if [[ "$names_got" == "$names_want" ]]; then
  pass_ev "TC-UC030-A1[别名集恰等] 条目名集合恰为 ${names_want}（无兜底条目、无多余条目）"
else
  fail_ev "TC-UC030-A1[别名集恰等] 条目集恰等于五别名" \
    "[${names_want}]" "[${names_got:-<空>}]" "${PROFILE_FILE}"
fi

for i in "${!ALIAS[@]}"; do
  a="${ALIAS[$i]}"; t="${TARGET[$i]}"
  lab="$(arch_entries "next((e.get(\"labelOverride\",\"\") for e in (d.get(\"inferenceModels\") or []) if e.get(\"name\")==\"${a}\"), \"<无此条目>\")")"
  ename="$(arch_entries "next((e.get(\"name\",\"\") for e in (d.get(\"inferenceModels\") or []) if e.get(\"name\")==\"${a}\"), \"\")")"
  if [[ "$lab" == "$t" && "$ename" == "$a" && "$a" != *"[1m]"* ]]; then
    if [[ "$a" == "claude-fable-5-mp" ]]; then
      pass_ev "TC-UC030-A1[目标 ${a}] labelOverride=${lab}（[1m] 变体承载=裸名条目+supports1m，条目名无 [1m] 后缀）"
    else
      pass_ev "TC-UC030-A1[目标 ${a}] labelOverride=${lab}"
    fi
  else
    fail_ev "TC-UC030-A1[目标 ${a}] 逐条目目标" \
      "labelOverride=${t}，name=${a}（裸名）" \
      "labelOverride=${lab:-<空>}，name=${ename:-<空>}" "${PROFILE_FILE}"
  fi
done

# 档案字节形态：决定键 + 本地 route 基址 + 自发密钥 + 逐条目键集 ⊆ 接受集。
prov="$(arch_entries 'str(d.get("inferenceProvider",""))')"
baseurl="$(arch_entries 'str(d.get("inferenceGatewayBaseUrl",""))')"
apikey="$(arch_entries 'str(d.get("inferenceGatewayApiKey",""))')"
if [[ "$prov" == "gateway" && "$baseurl" == "http://127.0.0.1:${PROXY_PORT}/claude-desktop" && "$apikey" == ccs-* ]]; then
  pass_ev "TC-UC030-A1[档案字节形态] inferenceProvider=gateway；baseUrl=${baseurl}；apiKey 为 ccs- 自发密钥（真实令牌不落盘）"
else
  fail_ev "TC-UC030-A1[档案字节形态] 决定键/基址/密钥形态" \
    "inferenceProvider=gateway；baseUrl=http://127.0.0.1:${PROXY_PORT}/claude-desktop；apiKey 形如 ccs-*" \
    "inferenceProvider=${prov:-<空>}；baseUrl=${baseurl:-<空>}；apikey=${apikey:0:12}…" "${PROFILE_FILE}"
fi

bad_keys="$(arch_entries '"|".join(sorted({k for e in (d.get("inferenceModels") or []) for k in e if k not in {"name","labelOverride","supports1m","prefer1m","anthropicFamilyTier","maxEffort"}}))')"
if [[ -z "$bad_keys" ]]; then
  pass_ev "TC-UC030-A1[条目键集] 全部条目键 ⊆ app.asar 接受集（无未知键可被 schema 静默丢弃）"
else
  fail_ev "TC-UC030-A1[条目键集] 逐条目键集 ⊆ 接受集" \
    "无接受集外的键" "多余键: ${bad_keys}" "${PROFILE_FILE}"
fi

# ── TC-UC030-A2（正·逐条目 supports1m/maxEffort 三元）─────────────────────────
# 值锚：z 的 supports1m 缺席=false 语义（R108 实证）；maxEffort 恒 max（desktop-effort-oracle
# 五名全 low,medium,high*,xhigh,max——条目上限不得切断阶梯）；effortMap 钳制锚 UC-029。

for i in "${!ALIAS[@]}"; do
  a="${ALIAS[$i]}"; want_s1="${S1M[$i]}"
  got_s1="$(arch_entries "next((e.get(\"supports1m\") for e in (d.get(\"inferenceModels\") or []) if e.get(\"name\")==\"${a}\"), None)")"
  got_p1="$(arch_entries "next((e.get(\"prefer1m\") for e in (d.get(\"inferenceModels\") or []) if e.get(\"name\")==\"${a}\"), None)")"
  got_me="$(arch_entries "next((e.get(\"maxEffort\",\"\") for e in (d.get(\"inferenceModels\") or []) if e.get(\"name\")==\"${a}\"), \"<无此条目>\")")"
  if [[ "$want_s1" == "true" ]]; then
    if [[ "$got_s1" == "true" && "$got_p1" == "true" && "$got_me" == "max" ]]; then
      pass_ev "TC-UC030-A2[三元 ${a}] supports1m=true prefer1m=true maxEffort=max"
    else
      fail_ev "TC-UC030-A2[三元 ${a}] 逐条目三元" \
        "supports1m=true prefer1m=true maxEffort=max" \
        "supports1m=${got_s1:-<空>} prefer1m=${got_p1:-<空>} maxEffort=${got_me:-<空>}" "${PROFILE_FILE}"
    fi
  else
    # z：supports1m/prefer1m 缺席（false 语义，不伪造键）+ maxEffort=max。
    if [[ "$got_s1" == "" && "$got_p1" == "" && "$got_me" == "max" ]]; then
      pass_ev "TC-UC030-A2[三元 ${a}] supports1m/prefer1m 缺席（false 语义，未伪造）maxEffort=max"
    else
      fail_ev "TC-UC030-A2[三元 ${a}] 逐条目三元（z 缺席语义）" \
        "supports1m/prefer1m 键缺席（false 语义）、maxEffort=max" \
        "supports1m=${got_s1:-<缺席>} prefer1m=${got_p1:-<缺席>} maxEffort=${got_me:-<空>}" "${PROFILE_FILE}"
    fi
  fi
done

# effortMap 钳制语义锚（UC-029 口径；per 臂回读）：
#   mimo {xhigh→high}；glm {xhigh→max, max→max}；direct forceEffort=max。
declare -A EFFORT_WANT=(
  [mimo]="xhigh→high"
  [glm]="max→max|xhigh→max"
  [direct]="FORCE:max"
)
for arm in mimo glm direct; do
  rc=0; cli_run "effort-list-${arm}" remap effort list "$arm" --json || rc=$?
  got="$(cli_jget '"|".join(sorted((x.get("from","")+"→"+x.get("to","")) for x in (d.get("data",{}).get("items") or [])))')"
  force="$(cli_jget 'str(d.get("data",{}).get("forceEffort",""))')"
  if [[ "$arm" == "direct" ]]; then
    if [[ "$rc" == "0" && "$force" == "max" ]]; then
      pass_ev "TC-UC030-A2[effortMap ${arm}] force=max（df forceEffort 语义锚 UC-029）"
    else
      fail_ev "TC-UC030-A2[effortMap ${arm}] effort 钳制语义锚" \
        "forceEffort=max" "forceEffort=${force:-<空>}（rc=${rc}，prereq_ok=${prereq_ok}）" "$EV/${CLI_LAST}.out"
    fi
  else
    want="$(printf '%s' "${EFFORT_WANT[$arm]}" | tr '|' '\n' | LC_ALL=C sort | paste -sd'|' -)"
    if [[ "$rc" == "0" && "$got" == "$want" ]]; then
      pass_ev "TC-UC030-A2[effortMap ${arm}] ${got}（UC-029 钳制语义锚）"
    else
      fail_ev "TC-UC030-A2[effortMap ${arm}] effort 钳制语义锚" \
        "items=[${want}]" "items=[${got:-<空>}]（rc=${rc}，prereq_ok=${prereq_ok}）" "$EV/${CLI_LAST}.out"
    fi
  fi
done

# ── 非空洞判据（apply 真的改了盘上字节）───────────────────────────────────────

if [[ "$MID_SIG" != "$START_SIG" ]]; then
  pass_ev "TC-UC030-A1[非空洞] apply 后中间态与起点**不同**（写链真的落了字节）"
else
  fail_ev "TC-UC030-A1[非空洞] apply 后中间态应与起点不同" \
    "中间态签名 ≠ 起点签名" "完全相同（apply_ok=${apply_ok}，什么都没改）" "${EV}/commands.log"
fi

# ── TC-UC030-A3（负·条目独立无兜底）──────────────────────────────────────────
# 未知别名/缺条目 ⇒ 显式 4xx 且全部上游桩零命中（条目集恰=五别名 ⇒ 无兜底条目可落）；
# 错误路径 ⇒ 4xx。

for _l in "${STUB_LOGS[@]}"; do : >"$_l"; done
router_post "a3-unknown-alias" '{"model":"claude-fable-5-zzz","messages":[{"role":"user","content":"hi"}]}'
hits="$(stub_total_hits)"
if [[ "$ROUTER_CODE" =~ ^4[0-9][0-9]$ ]] && [[ "$hits" == "0" ]]; then
  pass_ev "TC-UC030-A3[未知别名] model=claude-fable-5-zzz ⇒ 显式 HTTP ${ROUTER_CODE}，三桩零命中（不落兜底条目）"
else
  fail_ev "TC-UC030-A3[未知别名] 未知别名显式 4xx 且不落兜底" \
    "HTTP 4xx 且上游 0 命中" "HTTP ${ROUTER_CODE}；命中=${hits}" "$EV/${ROUTER_LAST}.out 与 ${STUB_LOGS[*]}"
fi

for _l in "${STUB_LOGS[@]}"; do : >"$_l"; done
router_path_post "a3-bad-path" "/router/v1/no-such-endpoint" '{"model":"claude-fable-5-mp","messages":[{"role":"user","content":"hi"}]}'
hits="$(stub_total_hits)"
if [[ "$ROUTER_CODE" =~ ^4[0-9][0-9]$ ]] && [[ "$hits" == "0" ]]; then
  pass_ev "TC-UC030-A3[错误路径] /router/v1/no-such-endpoint ⇒ HTTP ${ROUTER_CODE}，三桩零命中（错误 host/路径/别名不得命中任何条目）"
else
  fail_ev "TC-UC030-A3[错误路径] 错误路径不得命中任何条目" \
    "HTTP 4xx 且上游 0 命中" "HTTP ${ROUTER_CODE}；命中=${hits}" "$EV/${ROUTER_LAST}.out 与 ${STUB_LOGS[*]}"
fi

# ── restore 回起点（逐字节）───────────────────────────────────────────────────

rc=0; cli_run "restore" restore --json || rc=$?
END_SIG="$(bytes_sig "${MANAGED_PATHS[@]}")"
if [[ "$rc" == "0" && "$END_SIG" == "$START_SIG" ]]; then
  pass_ev "TC-UC030-A1[restore] 裸 restore 后全部受管路径逐字节回起点（签名 $(printf '%s' "$END_SIG" | shasum -a 256 | cut -c1-12)）"
else
  fail_ev "TC-UC030-A1[restore] restore 后逐字节回起点" \
    "rc=0 且终点签名==起点签名" "rc=${rc}；起点=${START_SIG}；终点=${END_SIG}" "$EV/${CLI_LAST}.out"
fi

# ── TC-UC030-A4（边界·真机 app 接受）──────────────────────────────────────────
# 自动化探不到 ⇒ unknown + 人工步骤，**不折 pass**（UC-030 A4 预期行为）。

record_unknown "TC-UC030-A4 真机 Claude Desktop 重启后接受该配置并列出五个路由标签" \
  "需真机 GUI 人工步骤，自动化探不到 app 内标签页" \
  "按 reports/mixed-five-route-20260923/summary.md「用户试用步骤」人工执行（重启 app→配置库切该卡→试用五标签）并留证签收"

printf 'report  claude-desktop-five-route: arms=%s aliases=%s prereq_ok=%s evidence=%s\n' \
  "${ARMS[*]}" "${ALIAS[*]}" "$prereq_ok" "$EV"
verify_finish

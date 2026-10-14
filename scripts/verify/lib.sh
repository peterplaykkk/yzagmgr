#!/usr/bin/env bash
# scripts/verify/lib.sh —— 验收脚本共用工具（被 source，不独立执行）。
#
# 纪律：
#   1. 一切验收默认在**隔离根**下跑；只有 *-real-config.sh 触碰真实 ${HOME}，且必须先快照。
#   2. 依赖未就绪（守护进程/API/CLI 未交付）⇒ 非零退出 + 「BLOCKED:」明确说明；
#      **绝不许假装通过**（本文件与全部 verify 脚本的共同底线）。
#   3. 三态判定 pass / fail / unknown；unknown 单独成节、不得折成 pass。
#
# 退出码约定（供 test-expert 判读，不靠猜）：
#   0 = 全部 pass
#   1 = 存在 fail
#   2 = BLOCKED / 尚未实现 / HARNESS ERROR（前置不成立或**量具自己坏了**，什么都没验）
#   3 = 无 fail，但存在 unknown（不折成 pass）
#
# 用法：
#   . "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
#   verify_init <suite-name>
#   require_daemon
#   ... 断言 ...
#   verify_finish

# shellcheck disable=SC2034

ROOT=""            # yzagmgr 项目根
SUITE=""           # 本次 suite 名（用于报告目录）
RUN=""             # 本次运行的隔离根
DAEMON_PID=""
API_PORT="${YZAGMGR_API_PORT:-17022}"
PROXY_PORT="${YZAGMGR_PROXY_PORT:-17021}"
TOKEN_FILE=""
_PORTS_CHOSEN=0   # choose_isolated_ports 是否已定过端口（定过就不再改，见该函数）
_PORTS_EXPLICIT=0 # 端口是调用方显式指定的（YZAGMGR_API_PORT/YZAGMGR_PROXY_PORT）

declare -a _PASS_LINES=()
declare -a _FAIL_LINES=()
declare -a _UNKNOWN_LINES=()

# ── 基础 ────────────────────────────────────────────────────────────────────

die() { printf 'BLOCKED: %s\n' "$*" >&2; exit 2; }

# harness_error —— **量具自己坏了**，不是产品被证伪，也不是「前置不成立」。
# 例：探针应答的不是本脚本拉起的实例、端口越界、上游桩绑不上。这类情况必须与产品红/绿分开：
# 一旦发生就**响亮**退出，一条 pass 都不许登记 —— 假绿正是从这种缝隙里溜出去的。
# 退出码与 die 同族（2 = 什么都没验），但前缀独立，判读者一眼可分「产品红了」与「量具坏了」。
harness_error() { printf 'HARNESS ERROR: %s\n' "$*" >&2; exit 2; }

verify_init() {
  SUITE="${1:-suite}"
  ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
  if [[ -n "${YZAGMGR_HOME:-}" ]]; then
    : # 仅用于调用方显式复用已隔离的同一 run；顶层验收启动前应清除继承的 YZAGMGR_HOME。
  fi
  RUN="${YZAGMGR_HOME:-$(mktemp -d "${TMPDIR:-/tmp}/yzagmgr-${SUITE}-XXXXXX")}"
  mkdir -p "$RUN"
  export YZAGMGR_HOME="$RUN"
  # EXIT 之外还要挂 INT/TERM/HUP/PIPE：
  #   - `bash script.sh | head -N` 会在 head 退出后让脚本收 SIGPIPE 而死，**EXIT trap 不执行**
  #     ⇒ 守护进程泄漏、占住固定端口，后续每次运行都被端口冲突搞红（实测踩到）。
  #   - PIPE/HUP 下按 141 退出、INT/TERM 下按 130/143 退出，均先把子进程收干净。
  trap '_verify_exit_cleanup "$?"' EXIT
  trap '_verify_cleanup; exit 141' PIPE
  trap '_verify_cleanup; exit 143' HUP TERM
  trap '_verify_cleanup; exit 130' INT
  echo "suite=$SUITE root=$ROOT isolated_home=$RUN"
}

_verify_cleanup() {
  local blocked=0 scope_status
  # 只杀本脚本拉起的守护进程与上游桩；绝不 pkill 用户正在跑的实例。
  if [[ -n "${STUB_PID:-}" ]]; then
    _verify_stop_owned_stub || blocked=1
  fi
  if [[ -n "$DAEMON_PID" ]]; then
    scope_status=0; _verify_ps_scope_quiescent pid "$DAEMON_PID" || scope_status=$?
    if (( scope_status == 0 )); then
      wait "$DAEMON_PID" 2>/dev/null || true
    elif (( scope_status == 1 )); then
      if _verify_owned_pid "$DAEMON_PID" "$RUN/bin/yzagmgr"; then
        if kill "$DAEMON_PID" 2>/dev/null; then
          _verify_wait_child_bounded "$DAEMON_PID" "daemon" || blocked=1
        else
          printf 'cleanup blocked: could not signal verified daemon pid %s\n' "$DAEMON_PID" >&2
          blocked=1
        fi
      else
        printf 'cleanup skipped: pid %s no longer proves private daemon path %s/bin/yzagmgr\n' "$DAEMON_PID" "$RUN" >&2
        blocked=1
      fi
    else
      printf 'cleanup blocked: cannot obtain process snapshot for daemon pid %s\n' "$DAEMON_PID" >&2
      blocked=1
    fi
  fi
  (( blocked == 0 )) || return 2
}

# EXIT cleanup must turn an otherwise successful suite into a blocked result when owned
# processes could not be safely reaped, while preserving any earlier failure status.
_verify_exit_cleanup() {
  local prior_status="$1" cleanup_status=0
  _verify_cleanup || cleanup_status=$?
  if (( prior_status != 0 )); then exit "$prior_status"; fi
  exit "$cleanup_status"
}

# verify_register_stub_pid <pid> <private-script> —— 每个自建桩在启动后登记自己的精确 argv 路径。
# 未登记的 pid 不会被公共 cleanup 处理；登记路径必须是本次 RUN 中的文件。
verify_register_stub_pid() {
  local pid="$1" private_path="$2"
  [[ "$pid" =~ ^[1-9][0-9]*$ && "$private_path" == "$RUN"/* && -f "$private_path" ]] \
    || harness_error "拒绝登记非隔离桩进程 pid=${pid:-空} path=${private_path:-空}"
  STUB_PID="$pid"
  STUB_PRIVATE_PATH="$private_path"
}

_verify_stop_owned_stub() {
  if [[ -z "${STUB_PRIVATE_PATH:-}" ]]; then
    printf 'cleanup skipped: pid %s has no registered private stub path\n' "$STUB_PID" >&2
    return 2
  fi
  local scope_status=0
  _verify_ps_scope_quiescent pid "$STUB_PID" || scope_status=$?
  if (( scope_status == 0 )); then
    wait "$STUB_PID" 2>/dev/null || true
    return 0
  elif (( scope_status == 1 )) && _verify_owned_pid "$STUB_PID" "$STUB_PRIVATE_PATH"; then
    if ! kill "$STUB_PID" 2>/dev/null; then
      printf 'cleanup blocked: could not signal verified stub pid %s path %s\n' "$STUB_PID" "$STUB_PRIVATE_PATH" >&2
      return 2
    fi
    _verify_wait_child_bounded "$STUB_PID" "stub"
  else
    printf 'cleanup blocked: pid %s is live or uncertain and does not prove exact private stub path %s\n' "$STUB_PID" "$STUB_PRIVATE_PATH" >&2
    return 2
  fi
}

# Wait only while the recorded child can be observed. ps failure or a still-running child
# after the bound is a blocked cleanup; never wait without a limit on uncertain ownership.
_verify_wait_child_bounded() {
  local pid="$1" kind="$2" scope_status waited=0
  while (( waited < 100 )); do
    scope_status=0; _verify_ps_scope_quiescent pid "$pid" || scope_status=$?
    if (( scope_status == 0 )); then
      wait "$pid" 2>/dev/null || true
      return 0
    fi
    if (( scope_status == 2 )); then
      printf 'cleanup blocked: cannot obtain process snapshot for %s pid %s after signal\n' "$kind" "$pid" >&2
      return 2
    fi
    sleep 0.1
    waited=$((waited + 1))
  done
  printf 'cleanup blocked: %s pid %s did not exit within 10s\n' "$kind" "$pid" >&2
  return 2
}

# _verify_owned_pid <pid> <private-path> —— 只有本次 RUN 内的命令行仍可核验时才允许发信号。
# PID 记录本身不足以证明所有权：进程退出后 PID 可能复用。调用方必须传入 RUN 内的
# 唯一私有路径，并在每次 signal 前重查 ps；证据不足就放弃清理，不能猜测。
_verify_owned_pid() {
  local pid="$1" private_path="$2" command
  [[ "$pid" =~ ^[1-9][0-9]*$ && "$private_path" == "$RUN"/* ]] || return 1
  command="$(ps -p "$pid" -o command= 2>/dev/null)" || return 1
  [[ -n "$command" && "$command" == *"$private_path"* ]]
}

# run_capped 的进程组 leader 必须仍是带本次私有 wrapper 路径的 setsid 子进程。
_verify_owned_group() {
  local pid="$1" private_path="$2" pgid_raw pgid
  _verify_owned_pid "$pid" "$private_path" || return 1
  pgid_raw="$(ps -p "$pid" -o pgid= 2>/dev/null)" || return 1
  pgid="$(printf '%s' "$pgid_raw" | tr -d '[:space:]')"
  [[ "$pgid" == "$pid" ]]
}

# _verify_ps_scope_quiescent <pid|pgid> <id> —— 完整快照确认目标进程/组已消失或只剩 Z。
# 返回 0=静止，1=仍有可运行成员，2=快照不可用。ps 失败永不折算成“已退出”。
_verify_ps_scope_quiescent() {
  local scope="$1" target="$2" snapshot
  [[ ( "$scope" == pid || "$scope" == pgid ) && "$target" =~ ^[1-9][0-9]*$ ]] || return 2
  snapshot="$(ps -A -o pid= -o pgid= -o stat= 2>/dev/null)" || return 2
  printf '%s\n' "$snapshot" | awk -v scope="$scope" -v target="$target" '
    NF < 3 || $1 !~ /^[0-9]+$/ || $2 !~ /^[0-9]+$/ || $3 == "" { invalid = 1; next }
    {
      id = (scope == "pid") ? $1 : $2
      if (id == target) {
        found = 1
        if ($3 !~ /^Z/) live = 1
      }
    }
    END {
      if (invalid) exit 2
      if (live) exit 1
      exit 0
    }
  '
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "缺少命令 $1${2:+（$2）}"
}

# 守护进程二进制：Go 侧由并行任务交付；未交付即 BLOCKED（不降级、不假跑）。
require_daemon() {
  [[ -x "$ROOT/bin/yzagmgr" ]] || die "bin/yzagmgr 不存在（Go 守护进程尚未交付或未构建）。先执行 make build；守护进程未就绪前本套验收按设计拒绝运行。"
  require_cmd curl ""
}

# require_helper：tray 桌面面已删——保留空实现兼容调用位。
require_helper() { :; }

# ── 端口隔离（并发跑多个 suite 时的必需件）──────────────────────────────────

# valid_port <port> —— 端口在合法区间（1-65535）；非数字同判为假。
# 10# 前缀：bash 会把 `08` 这类前导零当八进制（实测踩到）。
valid_port() { [[ "$1" =~ ^[0-9]+$ ]] && ((10#$1 >= 1 && 10#$1 <= 65535)); }

# port_free <port> —— 该 TCP 端口当前无人监听。
port_free() { ! lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1; }

# listener_pids <port> —— 该端口上 LISTEN 套接字的进程 pid（一行一个，无人监听则空）。
listener_pids() { lsof -nP -iTCP:"$1" -sTCP:LISTEN -t 2>/dev/null || true; }

# pick_free_port —— 让内核分配一个空闲端口（bind :0 后立刻取号）。
# **绝不用算术推导**（如 proxy+100）：算术会越界 —— 实测 proxy 抽到 65487 时桩口 65587，
# python 的 bind 直接抛 `OverflowError: bind(): port must be 0-65535`，整个 suite 退化成
# 一句没有信息量的 BLOCKED（0 条断言、看不出根因）。
pick_free_port() {
  require_python
  python3 -c 'import socket
s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'
}

# _ports_conflict_hint —— 端口冲突时给出「下一步改什么」：显式指定与自动隔离的处置不同。
_ports_conflict_hint() {
  if ((_PORTS_EXPLICIT)); then
    printf '你显式指定了 YZAGMGR_API_PORT/YZAGMGR_PROXY_PORT，请改指空闲端口（两个都要给：没给的那个仍取默认 17022/17021）。'
  else
    printf '端口本已由本函数自动隔离，走到这里说明它被外部抢占了；重跑一次即可（会另取一对空闲端口）。'
  fi
}

# choose_isolated_ports —— 定下本次运行的 API/PROXY 端口。
# **start_isolated_daemon 已自动调用它**，手工再调只为在起守护进程前就打印所选端口；
# 重复调用**不改端口**（同一 suite 里第二次起守护进程时若重选，前后两半打的就不是同一个实例）。
#
# 为什么必需：管理 API 端口被占用时守护进程按契约**直接失败退出**（不换端口），而就绪探针
# 会被「别的实例」应答（同端口、异令牌）—— 那会让整个 suite 对着别人的隔离根做断言
# （实测踩到：tier-matrix 没做隔离，就绪探针从用户**已安装**的 v2 上拿到 200，报出
# 「daemon up」，随后 10 条断言全打在别人的实例上，输出 10 个 unauthorized）。
#
# 三档，按优先级：
#   ① 本函数已调用过 ⇒ 端口不改（见上）；
#   ② 调用方显式给了 YZAGMGR_API_PORT/YZAGMGR_PROXY_PORT ⇒ 原样尊重（并发跑或复现某次运行时要用），
#      只做形态校验；端口若被占也**不**在这里改口，交给 start_isolated_daemon 判成 harness 错误；
#   ③ 默认端口 17022/17021 空闲就用默认值（与手册/默认配置一致），否则另取一对空闲端口。
choose_isolated_ports() {
  require_python
  if ((_PORTS_CHOSEN)); then
    export API_PORT PROXY_PORT
    return 0
  fi
  if [[ -n "${YZAGMGR_API_PORT:-}" || -n "${YZAGMGR_PROXY_PORT:-}" ]]; then
    _PORTS_EXPLICIT=1
    valid_port "$API_PORT" || harness_error "YZAGMGR_API_PORT=${API_PORT} 不是合法端口（1-65535）"
    valid_port "$PROXY_PORT" || harness_error "YZAGMGR_PROXY_PORT=${PROXY_PORT} 不是合法端口（1-65535）"
    [[ "$API_PORT" != "$PROXY_PORT" ]] || harness_error "YZAGMGR_API_PORT 与 YZAGMGR_PROXY_PORT 相同（${API_PORT}），两者必须不同"
  elif port_free 17022 && port_free 17021; then
    API_PORT=17022; PROXY_PORT=17021
  else
    local pair
    pair="$(python3 -c 'import socket
socks = []
ports = []
for _ in range(2):
    s = socket.socket(); s.bind(("127.0.0.1", 0)); socks.append(s); ports.append(s.getsockname()[1])
print(ports[0], ports[1])')"
    API_PORT="${pair%% *}"; PROXY_PORT="${pair##* }"
    echo "note: 默认端口 17022/17021 被占用（并发实例），本次改用 api=${API_PORT} proxy=${PROXY_PORT}"
  fi
  if ! valid_port "$API_PORT" || ! valid_port "$PROXY_PORT"; then
    harness_error "选出的端口不合法（api=${API_PORT} proxy=${PROXY_PORT}）"
  fi
  _PORTS_CHOSEN=1
  export API_PORT PROXY_PORT
}

# require_own_daemon —— 断言「管理面应答的就是本脚本刚拉起的那个实例」。两条腿：
#   ① 令牌腿：**只有本实例知道**的令牌被接受（令牌每次进程启动重新生成、写在本次隔离根下，
#      UC-006 A9）。这是「就绪探针被别的实例应答」这条假绿路径的**唯一可靠否定**：
#      同端口、异实例时探针照样是 200（/api/v1/health 只回状态与 ok），异实例却拒我们的令牌。
#   ② 监听者腿：该端口的 LISTEN 套接字里有本脚本拉起的 pid。给失败原因再加一层定位
#      （是别人占着端口，还是我们的进程根本没起来）。lsof 不可用时（无可判信息）不据此判负。
# 两条腿都不成立时：**绝不登记 pass**，按 harness 错误响亮退出（退出码 2）——脚本不该在
# 「证明不了自己在量谁」的情况下继续做断言。
require_own_daemon() {
  local probe listeners
  probe="$(api GET /tools || true)"
  if ! printf '%s' "$probe" | grep -q '"ok":true'; then
    listeners="$(listener_pids "$API_PORT" | paste -sd, - || true)"
    harness_error "管理 API ${API_PORT} 上的应答方**不认本实例的令牌**（端口 LISTEN pid=${listeners:-<无>}，本脚本的 pid=${DAEMON_PID:-<无>}）：${probe:0:200}｜典型原因：别的实例（用户已安装的 v2 / 另一并发 suite）占着这个端口。此时任何断言都量不到本脚本的实例，禁止继续。"
  fi
  listeners="$(listener_pids "$API_PORT")"
  if [[ -n "$listeners" && -n "$DAEMON_PID" ]] && ! printf '%s\n' "$listeners" | grep -qx "$DAEMON_PID"; then
    harness_error "管理 API ${API_PORT} 的监听者（pid=$(printf '%s' "$listeners" | paste -sd, -)）中没有本脚本拉起的 pid=${DAEMON_PID} —— 令牌恰好被接受也不能据此认定这是自己的实例。"
  fi
}

# ── 隔离守护进程 ────────────────────────────────────────────────────────────

# 拉起一个隔离 HOME 下的守护进程并等待 API 可用，**并证明应答者就是它自己**。
# 端口：默认 17022/17021（internal/config/config.go 的 DefaultAPI/ProxyPort），
# 可用 YZAGMGR_API_PORT / YZAGMGR_PROXY_PORT 覆盖；被占则自动另取空闲端口。
#
# 本函数自带三道闸，调用方**不必**再记得调 choose_isolated_ports / require_own_daemon
# （漏调一次就是一次「量到别人实例」的假绿 —— 实测 tier-matrix.sh 正是这么中招的）：
#   ① choose_isolated_ports：端口先隔离；
#   ② 起进程**之前**确认两个端口此刻空闲，否则直接 harness 错误（不进入就绪循环）；
#   ③ 就绪之后 require_own_daemon：令牌腿 + 监听者腿，证明不了就响亮退出。
start_isolated_daemon() {
  require_daemon
  choose_isolated_ports
  local log="$RUN/daemon.log"
  # ② 端口在此刻必须空闲：就绪探针只知道「这个端口回了 200」，不知道回的是谁。
  # 端口被占时探针会被**别人的实例**应答（同端口、异令牌），于是「daemon up」是句假话。
  local p owner
  for p in "$API_PORT" "$PROXY_PORT"; do
    if ! port_free "$p"; then
      owner="$(listener_pids "$p" | paste -sd, -)"
      harness_error "端口 ${p} 上已有监听进程（pid=${owner:-未知}）：本脚本的实例绑不上它，此时任何应答都来自**别人的实例**（用户已安装的 v2，或另一并发 suite）。$(_ports_conflict_hint)"
    fi
  done
  # 把同一已构建二进制映射到本次 RUN 下的唯一 argv 路径，便于 cleanup 在
  # PID 可能复用时用 ps 证明进程仍属于本 suite；Go 的可执行内容仍来自 ROOT/bin/yzagmgr。
  mkdir -p "$RUN/bin"
  if [[ -L "$RUN/bin/yzagmgr" ]]; then
    [[ "$(readlink "$RUN/bin/yzagmgr")" == "$ROOT/bin/yzagmgr" ]] || harness_error "隔离守护进程路径 ${RUN}/bin/yzagmgr 指向了意外目标"
  elif [[ -e "$RUN/bin/yzagmgr" ]]; then
    harness_error "隔离守护进程路径 ${RUN}/bin/yzagmgr 已存在且不是预期符号链接"
  else
    ln -s "$ROOT/bin/yzagmgr" "$RUN/bin/yzagmgr" || harness_error "无法建立隔离守护进程路径 ${RUN}/bin/yzagmgr"
  fi
  YZAGMGR_HOME="$RUN" YZAGMGR_API_PORT="$API_PORT" YZAGMGR_PROXY_PORT="$PROXY_PORT" \
    "$RUN/bin/yzagmgr" >"$log" 2>&1 &
  DAEMON_PID=$!
  TOKEN_FILE="$RUN/.yzagmgr/data/admin.token"
  local waited=0
  while (( waited < 150 )); do
    # 先确认本脚本拉起的进程**还活着**：cmd/yzagmgr/main.go 是「先写管理令牌、再绑端口」，
    # 于是绑端口失败的那条路径**会留下一个像模像样的令牌文件**——只看令牌文件存在与否
    # 的探针会把它当成就绪（实测：tier-matrix 假绿）。
    if ! kill -0 "$DAEMON_PID" 2>/dev/null; then
      owner="$(listener_pids "$API_PORT" | paste -sd, -)"
      if [[ -n "$owner" ]]; then
        # 我们的进程死了、端口上却有人 ⇒ 是别人占着（端口隔离失败），不是产品缺陷。
        harness_error "本脚本的守护进程（pid=${DAEMON_PID}）已退出，而 ${API_PORT} 上的监听者是 pid=${owner} —— 端口被抢占，不能拿别人的应答当自己的结果。见 ${log}。"
      fi
      die "守护进程启动即退出（pid=${DAEMON_PID}），见 ${log}（末行：$(tail -1 "$log" 2>/dev/null | tr -d '\r')）"
    fi
    # 就绪探针必须打**契约里的路径** `/api/v1/health`。
    # 打裸 `/health` 是错的：它不在契约里，会落进 SPA 静态回落，在 web/dist 存在时
    # 返回 **200 的 HTML** —— 这会让「守护进程就绪」变成一个假绿信号（curl -f 只判状态码）。
    # 因此这里既打对路径，又要求响应体是 `"ok":true` 的 JSON。
    if [[ -s "$TOKEN_FILE" ]] && curl -fsS -m 1 "http://127.0.0.1:${API_PORT}/api/v1/health" 2>/dev/null | grep -q '"ok"[[:space:]]*:[[:space:]]*true'; then
      break
    fi
    sleep 0.1
    waited=$((waited + 1))
  done
  if (( waited >= 150 )); then
    die "15s 内未就绪（health/令牌未出现）。见 ${log}。注：端口注入方式以守护进程交付后的实现为准（TODO 见脚本头）。"
  fi
  # ③ 就绪 != 是自己的实例。证明不了就 harness 错误退出（绝不登记 pass）。
  require_own_daemon
  echo "daemon up pid=$DAEMON_PID api=http://127.0.0.1:${API_PORT} token=$TOKEN_FILE"
}

api() { # api <METHOD> <path> [json-body]
  local method="$1" path="$2" body="${3:-}"
  [[ -n "$TOKEN_FILE" && -s "$TOKEN_FILE" ]] || die "管理令牌不可读: $TOKEN_FILE"
  local -a args=(-sS -X "$method" -H "X-Agmgr-Token: $(cat "$TOKEN_FILE")")
  if [[ -n "$body" ]]; then
    args+=(-H "Content-Type: application/json" --data-binary "$body")
  fi
  curl "${args[@]}" "http://127.0.0.1:${API_PORT}/api/v1${path}"
}

# api_status <METHOD> <path> [json-body] —— 只要 HTTP 状态码（正文丢弃）。
api_status() {
  local method="$1" path="$2" body="${3:-}"
  [[ -n "$TOKEN_FILE" && -s "$TOKEN_FILE" ]] || die "管理令牌不可读: $TOKEN_FILE"
  local -a args=(-sS -o /dev/null -w '%{http_code}' -X "$method" -H "X-Agmgr-Token: $(cat "$TOKEN_FILE")")
  if [[ -n "$body" ]]; then
    args+=(-H "Content-Type: application/json" --data-binary "$body")
  fi
  curl "${args[@]}" "http://127.0.0.1:${API_PORT}/api/v1${path}"
}

# ── 通用证据 / 取值原语（多个 suite 共用；不改变既有行为）────────────────────

require_python() { command -v python3 >/dev/null 2>&1 || die "缺少 python3（本套断言用它解析 JSON 与算 sha256）"; }

# file_meta <path> —— 一次调用给出四段证据：exists/size/sha256/mtime_ns。
# 刻意同时给 mtime：判定「未变更」仍以 sha256 为准（§F-T1/T6），mtime 只作辅助线索。
file_meta() {
  require_python
  python3 - "$1" <<'PY'
import hashlib, os, sys
p = sys.argv[1]
try:
    st = os.stat(p)
except OSError:
    print("exists=0 size=- sha256=- mtime_ns=-")
    raise SystemExit
if not os.path.isfile(p):
    print(f"exists=0-notafile size=- sha256=- mtime_ns={st.st_mtime_ns}")
    raise SystemExit
h = hashlib.sha256(open(p, "rb").read()).hexdigest()
print(f"exists=1 size={st.st_size} sha256={h} mtime_ns={st.st_mtime_ns}")
PY
}

# file_mtime_ns <path> —— mtime 纳秒（缺席为空串）。
file_mtime_ns() {
  python3 -c 'import os, sys
try:
    print(os.stat(sys.argv[1]).st_mtime_ns)
except OSError:
    print("")' "$1"
}

# jget <json-text|-> <python-expr> —— 从 JSON 取字段；表达式里的根对象名为 d。
# 第一参数为 `-` 时读 stdin（便于管道用法）。标量直接打印（bool 打 true/false、
# null/空串打空行），容器打紧凑 JSON。
jget() {
  require_python
  local text="${1:-}"
  if [[ "$text" == "-" ]]; then text="$(cat)"; fi
  printf '%s' "$text" | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    print(""); raise SystemExit
try:
    v = eval(sys.argv[1], {"d": d, "json": json, "len": len, "str": str, "int": int,
                           "sum": sum, "sorted": sorted})
except Exception:
    print(""); raise SystemExit
if isinstance(v, bool):
    print("true" if v else "false")
elif v is None or v == "":
    print("")
elif isinstance(v, (dict, list)):
    print(json.dumps(v, ensure_ascii=False, separators=(",", ":")))
else:
    print(v)
' "${2:?jget 需要 python 表达式}"
}

# api_error_code <json-text> —— 失败信封的 error.code；成功信封返回空串。
api_error_code() { jget "${1:-}" 'd.get("error",{}).get("code","")'; }

# run_capped <secs> <out-file> <err-file> <cmd...> —— 有界运行。
# macOS 无 GNU timeout；超时按 124 返回（与 timeout(1) 同码），否则返回命令自身退出码。
# run_capped <secs> <out-file> <err-file> <cmd...> —— 有界运行。
# macOS 无 GNU timeout；超时按 124 返回（与 timeout(1) 同码），否则返回命令自身退出码。
#
# **必须让子进程自成会话**：部分 CLI 在收到 TERM 时会向自己的**进程组**发信号
# （实测：codex 超时被 TERM 后，同组的守护进程也收到了 SIGTERM 并退出，后续断言全部
# curl: (7)）。用 python 的 os.setsid() 在 fork 之后、exec 之前切开会话，
# 超时时按进程组回收，脚本自身与守护进程不再被殃及。
run_capped() {
  local secs="$1" out="$2" err="$3"; shift 3
  # stdin 也接 /dev/null：有些 CLI 会等 stdin（claude 会等 3s 并打 Warning），
  # 在验收里等 stdin 没有任何意义，还会把「最小非交互调用」拖成超时。
  local runner
  runner="$(mktemp "$RUN/run-capped.XXXXXX")" || harness_error "无法在隔离根创建 run_capped wrapper"
  cat >"$runner" <<'PY'
import os, signal, subprocess, sys
os.setsid()
# Keep the private group leader alive as an identity witness for the bounded TERM grace period;
# the command receives TERM, and a still-running owned group receives KILL after 300 ms.
signal.signal(signal.SIGTERM, signal.SIG_IGN)
raise SystemExit(subprocess.call(sys.argv[1:]))
PY
  python3 "$runner" "$@" >"$out" 2>"$err" </dev/null &
  local pid=$! waited=0 rc scope_status
  while (( waited < secs * 10 )); do
    if ! kill -0 "$pid" 2>/dev/null; then
      scope_status=0; _verify_ps_scope_quiescent pgid "$pid" || scope_status=$?
      if (( scope_status == 0 )); then
        rc=0; wait "$pid" 2>/dev/null || rc=$?
        rm -f "$runner"
        return "$rc"
      fi
      if (( scope_status == 1 )); then
        _run_capped_blocked "$pid" "$runner" "leader exited while a process remains in its recorded process group"
      else
        _run_capped_blocked "$pid" "$runner" "cannot obtain a process-group snapshot after leader exit"
      fi
      return 2
    fi
    sleep 0.1; waited=$((waited + 1))
  done
  if _verify_owned_group "$pid" "$runner"; then
    if ! kill -TERM -"$pid" 2>/dev/null; then
      _run_capped_blocked "$pid" "$runner" "TERM signal failed"
      return 2
    fi
  else
    _run_capped_blocked "$pid" "$runner" "private wrapper path or process group could not be verified before TERM"
    return 2
  fi
  sleep 0.3
  scope_status=0; _verify_ps_scope_quiescent pid "$pid" || scope_status=$?
  if (( scope_status == 0 )); then
    scope_status=0; _verify_ps_scope_quiescent pgid "$pid" || scope_status=$?
    if (( scope_status == 0 )); then
      wait "$pid" 2>/dev/null || true
      rm -f "$runner"
      return 124
    fi
    if (( scope_status == 1 )); then
      _run_capped_blocked "$pid" "$runner" "leader exited after TERM while a process remains in its recorded process group"
    else
      _run_capped_blocked "$pid" "$runner" "cannot obtain a process-group snapshot after leader exit following TERM"
    fi
    return 2
  elif (( scope_status == 2 )); then
    _run_capped_blocked "$pid" "$runner" "cannot inspect leader after TERM"
    return 2
  fi
  if _verify_owned_group "$pid" "$runner"; then
    if ! kill -KILL -"$pid" 2>/dev/null; then
      _run_capped_blocked "$pid" "$runner" "KILL signal failed after ownership recheck"
      return 2
    fi
  else
    _run_capped_blocked "$pid" "$runner" "private wrapper path or process group could not be verified before KILL"
    return 2
  fi
  waited=0
  while (( waited < 100 )); do
    scope_status=0; _verify_ps_scope_quiescent pgid "$pid" || scope_status=$?
    if (( scope_status == 0 )); then
      wait "$pid" 2>/dev/null || true
      rm -f "$runner"
      return 124
    fi
    if (( scope_status == 2 )); then
      _run_capped_blocked "$pid" "$runner" "cannot inspect recorded process group after KILL"
      return 2
    fi
    sleep 0.1
    waited=$((waited + 1))
  done
  _run_capped_blocked "$pid" "$runner" "process group remained after bounded KILL wait"
  return 2
}

_run_capped_blocked() {
  local pid="$1" runner="$2" reason="$3" marker
  marker="$RUN/run-capped-${pid}.blocked"
  printf 'pid=%s\nrunner=%s\nreason=%s\n' "$pid" "$runner" "$reason" >"$marker"
  printf 'HARNESS ERROR: run_capped cleanup blocked for pid %s (%s); private evidence retained at %s\n' \
    "$pid" "$reason" "$marker" >&2
}

# ── 时间戳（GNU 与 BSD date 的差异在这里收口）────────────────────────────────

# rfc3339_epoch <RFC3339> —— 转 Unix 秒。用 python 而不是 `date -j/-d`：
# macOS 的 `date -j -f` 与 GNU 的 `date -d` 不通用，而 API 回显的 `at` 带本机时区偏移
# （如 +08:00），写死 `%Y-%m-%dT%H:%M:%SZ` 的格式串会直接报 illegal time format（实测踩到）。
rfc3339_epoch() {
  python3 -c 'import datetime, sys
s = sys.argv[1]
if s.endswith("Z"):
    s = s[:-1] + "+00:00"
print(int(datetime.datetime.fromisoformat(s).timestamp()))' "$1"
}

# rfc3339_shift <RFC3339> <±秒> —— 偏移后按同一时区形态渲染（保持 +08:00 这类偏移）。
rfc3339_shift() {
  python3 -c 'import datetime, sys
s = sys.argv[1]
if s.endswith("Z"):
    s = s[:-1] + "+00:00"
print((datetime.datetime.fromisoformat(s) + datetime.timedelta(seconds=int(sys.argv[2]))).isoformat())' "$1" "$2"
}

# rfc3339_utc <RFC3339> —— 转成 Z 结尾的 UTC 字面量。
# 为什么必须：API 回显的 `at` 带 `+08:00`，而查询串里的 `+` 会被解码成空格
# （实测：直接拿 `+08:00` 当 from= 会让窗口变成非法时间 ⇒ 服务端按 invalid_request 拒掉）。
rfc3339_utc() {
  python3 -c 'import datetime, sys
s = sys.argv[1]
if s.endswith("Z"):
    s = s[:-1] + "+00:00"
t = datetime.datetime.fromisoformat(s).astimezone(datetime.timezone.utc)
print(t.strftime("%Y-%m-%dT%H:%M:%SZ"))' "$1"
}

# ── 测试种子（策略 + 推理令牌）─────────────────────────────────────────────

# seed_test_policy [origin-ignored] [catalogs-json]
#   origin 形参已废弃并**不再导出**（YZAGMGR_GATEWAY_ORIGIN 已随旧中继删除；
#   家族 Probe 只认回环受管形态）。保留形参只为兼容既有调用位。
#   catalogs-json 是 GET /policy 的 catalogs 对象**原文**（代理按字节搬运，故由调用方给整串）。
seed_test_policy() {
  local catalogs="${2:-{\}}"
  printf '{"version":1,"defaultModel":"deepseek-flash",\n "models":[{"id":"deepseek-flash","name":"deepseek-flash","supportsImages":true}],\n "catalogs":%s}\n' "$catalogs" >"$RUN/policy.json"
  echo "acceptance-token-0000000000000000" >"$RUN/inference-token"
  export YZAGMGR_POLICY_FILE="$RUN/policy.json"
  export YZAGMGR_TOKEN_FILE="$RUN/inference-token"
}

# ── 上游桩（hermetic 上游夹具，proxy/usage/cli-roundtrip 共用）───────────────
#
# 为什么需要它：代理的转发面必须有**可控**上游才能断言「逐字节透传 / 非 2xx 透传 /
# SSE 首帧时间与总长无关」。真实网关只能给出它自己的响应，不可控也不可重复。
#
# 上游地址不得是回环（UC-007 A13 的硬守卫），故桩绑 0.0.0.0、上游填**本机非回环地址**
# （host_ip）。桩的行为（全部由请求内容驱动，故无需按用例改桩）：
#   - 记录每个请求（方法 / 路径 / model / 状态）到 $STUB_LOG；
#   - 模型名形如 `stub-status-<code>` → 返回该状态码（非 2xx 透传的注入点）；
#   - 模型名形如 `stub-sse-<frames>x<ms>` → text/event-stream，逐帧 flush，
#     每帧间隔 <ms>（SSE 首帧时间与响应总长无关的注入点）；
#   - 请求 `…/responses` 且 body 里带 `"stream": true` → **Responses 的 SSE wire**
#     （codex 的客户端只认这条：它按事件流的 `response.completed` 收尾，拿到非 SSE 的 JSON
#     会报 `stream disconnected before completion: stream closed before response.completed`
#     并重试到 rc=1 —— 实测踩到，是「codex 车道永远跑不通」的夹具侧原因）；
#   - 其余按路径返回该 wire 的协议应答（带 usage 数字，供用量对账）。

STUB_PID=""
STUB_PRIVATE_PATH=""
STUB_PORT=""
STUB_LOG=""
stub_port_explicit=0   # 桩端口是调用方指定的（则不擅自换端口重试）还是内核分的

start_stub_upstream() {
  require_python
  # 端口来源两档，**都不做算术推导**（`proxy+100` 会越过 65535 上界：实测 proxy 抽到 65487
  # ⇒ 桩口 65587 ⇒ python bind 抛 OverflowError ⇒ 整个 suite 变成一句没有信息量的 BLOCKED）：
  #   ① 调用方显式给出：只做区间校验，越界当场判成 harness 错误（并写清是谁给的、合法区间是多少）；
  #   ② 缺省：请内核分配一个空闲端口（bind :0），天然落在合法区间内。
  if (($# >= 1)) && [[ -n "${1:-}" ]]; then
    STUB_PORT="$1"
    stub_port_explicit=1
    if ! valid_port "$STUB_PORT"; then
      harness_error "上游桩端口 '${STUB_PORT}' 越界（合法区间 1-65535，当前由调用方显式指定）。不要用算术推导桩口（如 proxy+100）：代理端口接近上界时必然越界。"
    fi
  else
    STUB_PORT="$(pick_free_port)"
    stub_port_explicit=0
  fi
  STUB_LOG="$RUN/stub-upstream.log"
  : >"$STUB_LOG"
  cat >"$RUN/stub_upstream.py" <<'PY'
import json, sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT, LOG = int(sys.argv[1]), sys.argv[2]

def note(line):
    with open(LOG, "a", encoding="utf-8") as f:
        f.write(line + "\n")

def parse(body):
    try:
        return json.loads(body or b"{}")
    except Exception:
        return {}

class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def _send(self, status, payload, ctype="application/json"):
        raw = payload if isinstance(payload, bytes) else json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def _sse(self, model):
        frames, ms = 1, 0
        try:
            _, spec = model.split("stub-sse-", 1)
            n, m = spec.split("x", 1)
            frames, ms = int(n), int(m)
        except Exception:
            pass
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        def frame(obj):
            data = ("data: " + json.dumps(obj) + "\n\n").encode()
            self.wfile.write(b"%x\r\n" % len(data) + data + b"\r\n")
            self.wfile.flush()
        for i in range(frames):
            if i:
                time.sleep(ms / 1000.0)
            frame({"type": "message_delta", "delta": {"stop_reason": None},
                   "usage": {"input_tokens": 3 if i == 0 else 0, "output_tokens": i + 1}})
        self.wfile.write(b"0\r\n\r\n")
        self.wfile.flush()

    def _sse_responses(self, model):
        # OpenAI Responses 的事件流：codex 的客户端按 `response.completed` 收尾，
        # 缺它一律判「stream closed before response.completed」；文本靠 output_text.delta
        # 增量送达（最后一块的 response.usage 同时也是代理嗅探 token 用量的落点，
        # 见 internal/proxy/accounting.go 的「OpenAI Responses」形态）。
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        item = {"id": "msg_stub", "type": "message", "status": "completed", "role": "assistant",
                "content": [{"type": "output_text", "text": "stub-ok", "annotations": []}]}
        done = {"id": "resp_stub", "object": "response", "created_at": 1, "status": "completed",
                "model": model or "deepseek-flash", "output": [item],
                "usage": {"input_tokens": 3, "output_tokens": 2, "total_tokens": 5}}
        created = dict(done, status="in_progress", output=[])
        added = dict(item, status="in_progress", content=[])
        events = [
            ("response.created", {"type": "response.created", "response": created}),
            ("response.output_item.added", {"type": "response.output_item.added",
                                            "output_index": 0, "item": added}),
            ("response.output_text.delta", {"type": "response.output_text.delta",
                                            "item_id": "msg_stub", "output_index": 0,
                                            "content_index": 0, "delta": "stub-ok"}),
            ("response.output_item.done", {"type": "response.output_item.done",
                                           "output_index": 0, "item": item}),
            ("response.completed", {"type": "response.completed", "response": done}),
        ]
        for name, obj in events:
            data = ("event: " + name + "\ndata: " + json.dumps(obj) + "\n\n").encode()
            self.wfile.write(b"%x\r\n" % len(data) + data + b"\r\n")
            self.wfile.flush()
        self.wfile.write(b"0\r\n\r\n")
        self.wfile.flush()

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(n)
        doc = parse(body)
        model = str(doc.get("model") or "")
        status = 200
        if model.startswith("stub-status-"):
            try:
                status = int(model.split("stub-status-", 1)[1])
            except Exception:
                status = 500
        note(f"POST {self.path} model={model} status={status} bytes={n}")
        if status != 200:
            self._send(status, {"error": {"code": "stub-injected", "message": f"注入状态 {status}"}})
            return
        if "stub-sse-" in model:
            self._sse(model)
            return
        if "responses" in self.path and doc.get("stream"):
            self._sse_responses(model)
            return
        if "messages" in self.path:
            out = {"id": "msg_stub", "type": "message", "role": "assistant", "model": "deepseek-flash",
                   "content": [{"type": "text", "text": "stub-ok"}], "stop_reason": "end_turn",
                   "stop_sequence": None, "usage": {"input_tokens": 3, "output_tokens": 2}}
        elif "responses" in self.path:
            out = {"id": "resp_stub", "object": "response", "created_at": 1, "status": "completed",
                   "model": "deepseek-flash",
                   "output": [{"type": "message", "id": "msg_1", "role": "assistant",
                               "status": "completed",
                               "content": [{"type": "output_text", "text": "stub-ok", "annotations": []}]}],
                   "usage": {"input_tokens": 3, "output_tokens": 2, "total_tokens": 5}}
        else:
            out = {"id": "chatcmpl-stub", "object": "chat.completion", "created": 1,
                   "model": "deepseek-flash",
                   "choices": [{"index": 0, "message": {"role": "assistant", "content": "stub-ok"},
                                "finish_reason": "stop"}],
                   "usage": {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}}
        self._send(200, out)

    def do_GET(self):
        note(f"GET {self.path}")
        self._send(200, {"data": []})

ThreadingHTTPServer(("0.0.0.0", PORT), H).serve_forever()
PY
  # 起桩：进程**死掉**（绑定类错误）与**没起来但还活着**（慢启动）分开处理，
  # 前者换端口重试（仅当端口是内核分的；调用方显式给了端口就不擅自改口）。
  local attempt=0
  while (( attempt < 3 )); do
    attempt=$((attempt + 1))
    python3 "$RUN/stub_upstream.py" "$STUB_PORT" "$STUB_LOG" >"$RUN/stub-upstream.out" 2>&1 &
    verify_register_stub_pid "$!" "$RUN/stub_upstream.py"
    local waited=0 ready=0 died=0
    while (( waited < 50 )); do
      if curl -fsS -m 1 -o /dev/null "http://127.0.0.1:${STUB_PORT}/__ping" 2>/dev/null; then
        ready=1; break
      fi
      kill -0 "$STUB_PID" 2>/dev/null || { died=1; break; }
      sleep 0.1; waited=$((waited + 1))
    done
    if (( ready )); then
      echo "stub upstream pid=$STUB_PID port=$STUB_PORT log=$STUB_LOG"
      return 0
    fi
    local why
    why="$(_stub_fail_reason)"
    if (( died )) && (( stub_port_explicit == 0 )) && (( attempt < 3 )); then
      echo "note: 上游桩在端口 ${STUB_PORT} 上起不来（${why}）—— 换一个内核另分的空闲端口重试"
      STUB_PORT="$(pick_free_port)"
      continue
    fi
    if (( died )); then
      # 桩绑不上是**验收夹具自身**的问题（端口越界/被占用），不是产品缺陷：
      # 原状是一句「上游桩启动即退出，见 <文件>」的 BLOCKED，根因埋在文件里（实测：越界端口
      # 只留下一句 OverflowError，判读者会以为是产品问题）。这里把根因抬到台面上并报 harness 错误。
      harness_error "上游桩起不来（端口 ${STUB_PORT}，第 ${attempt} 次）：${why}｜这是验收夹具自身的问题，不是产品缺陷。完整输出：${RUN}/stub-upstream.out"
    fi
    harness_error "上游桩 5s 内未就绪（端口 ${STUB_PORT}）：进程仍活着但探针取不到 200。见 ${RUN}/stub-upstream.out"
  done
  harness_error "上游桩 3 次都未能绑定端口（最后一次 ${STUB_PORT}）。见 ${RUN}/stub-upstream.out"
}

# _stub_fail_reason —— 从桩的 stderr 里摘一行根因：绑定类错误的原文最有判读价值
# （OverflowError = 端口越界；`OSError: [Errno 48] Address already in use` = 端口被占）。
# 先挑具体错误，再退到「含 Error 的行」，最后才取末行 —— 直接取首行只会拿到 `Traceback ...`。
_stub_fail_reason() {
  local out="$RUN/stub-upstream.out" line
  line="$(grep -m1 -E 'OverflowError|Address already in use|PermissionError|OSError' "$out" 2>/dev/null || true)"
  if [[ -z "$line" ]]; then
    line="$(grep -m1 -E 'Error|Traceback' "$out" 2>/dev/null || true)"
  fi
  printf '%s' "${line:-$(tail -1 "$out" 2>/dev/null | tr -d '\r')}"
}

# host_ip —— 本机非回环 IPv4。桩上游必须填它（上游不得是回环，UC-007 A13）。
host_ip() {
  local ip="" iface
  for iface in en0 en1 en2 en3; do
    ip="$(ipconfig getifaddr "$iface" 2>/dev/null || true)"
    if [[ -n "$ip" ]]; then printf '%s' "$ip"; return 0; fi
  done
  ip="$(python3 -c 'import socket
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
try:
    s.connect(("192.0.2.106", 80)); print(s.getsockname()[0])
except Exception:
    print("")' 2>/dev/null || true)"
  if [[ -n "$ip" && "$ip" != 127.* ]]; then printf '%s' "$ip"; return 0; fi
  return 1
}

stub_reset() { : >"$STUB_LOG"; }
stub_hits() { # stub_hits <grep-pattern>；无匹配输出单个 0
  # 注意：`grep -c` 无匹配时**既打印 0 又返回 1**，写成 `grep -c ... || echo 0` 会输出
  # "0\n0"（实测踩到）。故先取输出再兜底。
  [[ -f "$STUB_LOG" ]] || { printf '0'; return 0; }
  local n
  n="$(grep -c -- "$1" "$STUB_LOG" 2>/dev/null)" || true
  printf '%s' "${n:-0}"
}

# data_plane_status <path> —— 数据面（不经 /api/v1、不鉴权）的 HTTP 状态码。
data_plane_status() {
  curl -sS -o /dev/null -w '%{http_code}' -m 10 "http://127.0.0.1:${PROXY_PORT}$1"
}

# data_plane_body <METHOD> <path> [body] [extra-curl-args...] —— 数据面正文。
data_plane_body() {
  local method="$1" path="$2" body="${3:-}"; shift 3 || true
  local -a args=(-sS -m 20 -X "$method")
  if [[ -n "$body" ]]; then
    args+=(-H "Content-Type: application/json" --data-binary "$body")
  fi
  curl "${args[@]}" "$@" "http://127.0.0.1:${PROXY_PORT}${path}"
}

# ── 三态登记 ────────────────────────────────────────────────────────────────

record_pass() { _PASS_LINES+=("$1"); printf 'pass    %s\n' "$1"; }
record_fail() { _FAIL_LINES+=("$1"); printf 'FAIL    %s\n' "$1"; }
record_unknown() { # record_unknown <assertion-id> <原因> <解除条件>
  local line="$1；原因=$2；解除条件=$3"
  _UNKNOWN_LINES+=("$line")
  printf 'unknown %s\n' "$line"
}

# 断言辅助：expect_eq <name> <actual> <expected>
expect_eq() {
  local name="$1" actual="$2" expected="$3"
  if [[ "$actual" == "$expected" ]]; then
    record_pass "$name"
  else
    record_fail "${name}（actual=$actual expected=${expected}）"
  fi
}

sha256_file() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    sha256sum "$1" | awk '{print $1}'
  fi
}

# 收尾：打印机器可读摘要并按退出码约定退出。
verify_finish() {
  printf -- '---\n'
  printf '%s summary: pass=%d fail=%d unknown=%d\n' "$SUITE" "${#_PASS_LINES[@]}" "${#_FAIL_LINES[@]}" "${#_UNKNOWN_LINES[@]}"
  if ((${#_FAIL_LINES[@]} > 0)); then
    printf 'verdict: FAIL\n'
    exit 1
  fi
  if ((${#_UNKNOWN_LINES[@]} > 0)); then
    printf 'verdict: UNKNOWN（不含 fail，但按纪律不得折成 pass）\n'
    exit 3
  fi
  if ((${#_PASS_LINES[@]} == 0)); then
    printf 'verdict: BLOCKED（零断言，不允许记 pass）\n'
    exit 2
  fi
  printf 'verdict: PASS\n'
  exit 0
}

# 骨架脚本的统一出口：前置成立但断言尚未实现时，明确非零退出。
verify_not_implemented() {
  local manual_ref="$1"; shift
  printf 'NOT IMPLEMENTED: 本脚本仍是骨架（断言未实现），验收手册: %s\n' "$manual_ref" >&2
  for todo in "$@"; do
    printf '  TODO: %s\n' "$todo" >&2
  done
  printf '拒绝以任何形式假装通过（退出码 2）。\n' >&2
  exit 2
}

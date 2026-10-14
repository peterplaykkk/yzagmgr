#!/usr/bin/env bash
# scripts/verify/cli-roundtrip.sh —— 真实 CLI 往返（UC-005 的真机 lane）。
#
# 验收手册: doc/acceptance-steps.md §4。
#
# 前置（未就绪即 BLOCKED，退出码 2）：bin/yzagmgr；种子策略 + 令牌。
#
# 两段判定，**分开登记、不互相顶替**：
#   - `config:` 配置级：官方档 ↔ 网关档交替 N 轮，每轮**从盘上读决定键**判档位
#     （自带独立分类器，不复用产品 Probe()），终点字节 == 起点。
#     有官方种子的家（claude/codex）走官方卡；additive 家（opencode/pi）没有官方卡，
#     「回到官方档」= 恢复初始快照（与 tier-roundtrip.sh 同一语义，此处按 CLI 视角复跑）。
#   - `run:` 真跑级：每轮用该 CLI 的**最小非交互调用**真跑一次，判定分三态：
#       pass    = 拿到模型应答（上游 2xx，输出里出现应答标记）；
#       unknown = CLI 没跑起来（隔离配置目录没有登录态）/ 上游没有可用凭据或按设计不可达；
#       fail    = 跑起来了但**没有采纳我们写的配置**（例如用了别的端点/模型）。
#     **探不到就记 unknown，绝不折成 pass**（recipe §D）。
#
# 为了让 run 级可判定，claude/codex 两条车道额外把**上游**指到 hermetic 上游桩：
#   - claude：受管档写的是 `http://127.0.0.1:<proxy>/claude`，只有 CLI **真的采纳**了这份
#     settings.json，请求才会到本地代理、再由代理打到桩并拿到 200 —— 这条链路正是「配置被
#     CLI 消费」的强证据。凭据用占位符（`ANTHROPIC_API_KEY=PROXY_MANAGED`，真实令牌由代理
#     换入），不是任何真实密钥。
#   - codex：受管档的 base_url 是**网关直连**（家族契约），而隔离配置目录按设计不放凭据
#     副本 ⇒ 只给 CODEX_HOME 必被网关回 401（实测的失败形态）。故 run 级按产品的物化启动
#     命令（internal/api/materialize.go 的 codexLaunchCommand）加一条 `-c` 覆盖，把 base_url
#     指到本地代理的 /openai/v1，凭据由代理注入；该前缀的上游同样指向桩。
#
# 用法: cli-roundtrip.sh [--clis claude,codex,opencode,pi] [--rounds 3] [--origin URL]
#   --origin 默认用 hermetic 测试域（example.com 保留域、不需可达）；原真机 lane 说明：
#   https://gateway.example.com 并配合真实推理令牌，让 run 级走真网关。
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib.sh
. "$HERE/lib.sh"

CLIS="claude,codex,opencode,pi"
ROUNDS=3
ORIGIN="https://gateway.example.com"
while (($# > 0)); do
  case "$1" in
    --clis) shift; CLIS="${1:-}" ;;
    --rounds) shift; ROUNDS="${1:-3}" ;;
    --origin) shift; ORIGIN="${1:-}" ;;
    *) die "未知参数 $1" ;;
  esac
  shift
done

verify_init cli-roundtrip
seed_test_policy "$ORIGIN"
choose_isolated_ports
start_isolated_daemon
require_own_daemon

# Claude/Codex 的代理链需要上游桩；只验 OpenCode/Pi 时不启动多余监听器。
STUB_URL=""
if [[ ",${CLIS}," == *",claude,"* || ",${CLIS}," == *",codex,"* ]]; then
  UP_IP="$(host_ip || true)"
  [[ -n "$UP_IP" ]] || die "本机没有非回环 IPv4：上游守卫禁止回环（UC-007 A13），无法起上游桩"
  # 桩口由 lib.sh 请内核分配（不能用 proxy+100，否则高端口会越界）。
  start_stub_upstream
  STUB_URL="http://${UP_IP}:${STUB_PORT}"
fi

# ── 独立档位分类器（只读决定键；不复用产品实现）────────────────────────────
cat >"$RUN/tierof.py" <<'TIEROF'
import json, re, sys, pathlib

FAMILY = sys.argv[1]
PATHS = [p for p in sys.argv[2:] if p]

def pick(*needles):
    best = ""
    for p in PATHS:
        if any(n in p for n in needles):
            best = p
    return best

def read(p):
    try:
        return pathlib.Path(p).read_text(encoding="utf-8")
    except OSError:
        return None

def jload(t):
    try:
        return json.loads(t)
    except Exception:
        return None

def classify():
    if FAMILY == "claude":
        t = read(pick(".claude/settings.json", ".claude/claude.json"))
        if t is None:
            return "official"
        v = (jload(t) or {}).get("env", {}).get("ANTHROPIC_BASE_URL")
        if not v:
            return "official"
        return "gateway" if ("127.0.0.1" in v) else "unknown"
    if FAMILY == "codex":
        t = read(pick(".codex/config.toml"))
        if t is None:
            return "official"
        m = re.search(r'^\s*model_provider\s*=\s*"([^"]*)"', t, re.M)
        return "gateway" if (m and m.group(1) == "custom") else "official"
    if FAMILY in ("opencode", "pi", "openclaw", "hermes", "gemini", "grokbuild"):
        needle = {"opencode": "opencode.json", "pi": "models.json", "openclaw": "openclaw.json",
                  "hermes": "config.yaml", "gemini": ".env", "grokbuild": "config.toml"}[FAMILY]
        t = read(pick(needle))
        if t is None:
            return "absent"
        return "gateway" if re.search(r'yzagmgr-[A-Za-z0-9_]', t) else "official"
    return "unknown"

print(classify())
TIEROF

tierof() { local fam="$1"; shift; python3 "$RUN/tierof.py" "$fam" "$@"; }
managed_card() { api GET "/tools/$1/profiles" | jget - 'next((p["id"] for p in d["data"]["items"] if p.get("kind")=="managed"), "")'; }
official_card() { api GET "/tools/$1/profiles" | jget - 'next((p["id"] for p in d["data"]["items"] if p.get("kind")=="official"), "")'; }
fam_paths() { api GET "/tools/$1" | jget - '"|".join(p["path"] for p in d["data"]["paths"])'; }

bytes_sig() {
  local out="" p
  while IFS= read -r p; do
    [[ -z "$p" ]] && continue
    if [[ -f "$p" ]]; then out="${out}${p}=$(sha256_file "$p")|"; else out="${out}${p}=<absent>|"; fi
  done < <(printf '%s\n' "$@" | LC_ALL=C sort)
  printf '%s' "${out%|}"
}

apply_card() {
  local fam="$1" pid="$2" pv pvid ap
  pv="$(api POST "/tools/${fam}/preview" "{\"profileId\":\"${pid}\"}")"
  pvid="$(jget "$pv" 'd["data"]["previewId"]')"
  [[ -n "$pvid" ]] || die "${fam} preview 未返回 previewId：${pv:0:200}"
  ap="$(api POST "/tools/${fam}/apply" "{\"previewId\":\"${pvid}\"}")"
  printf '%s' "$ap" | grep -q '"ok":true' || die "${fam} apply 失败：${ap:0:240}"
}

restore_initial() {
  local fam="$1" bk pv pvid ap
  bk="$(api GET "/tools/${fam}/backups?limit=50" | jget - 'next((b["backupId"] for b in d["data"]["items"] if b.get("initial")), "")')"
  [[ -n "$bk" ]] || die "${fam} 取不到初始快照"
  pv="$(api POST "/tools/${fam}/restore/preview" "{\"backupId\":\"${bk}\"}")"
  pvid="$(jget "$pv" 'd["data"]["previewId"]')"
  [[ -n "$pvid" ]] || die "${fam} restore/preview 失败：${pv:0:200}"
  ap="$(api POST "/tools/${fam}/restore/apply" "{\"previewId\":\"${pvid}\"}")"
  printf '%s' "$ap" | grep -q '"ok":true' || die "${fam} restore/apply 失败：${ap:0:240}"
}

# ── CLI 侧的隔离变量与最小非交互调用（两层必须指向同一物理 $RUN，recipe §B.2）──
run_cli() { # run_cli <fam> <out> <err> → 返回退出码（124 = 超时）
  local fam="$1" out="$2" err="$3"
  case "$fam" in
    claude)
      # 脚手架显式给出 route 两键 + 档名 —— 两条都是**前置**，不是产品行为：
      #   ① 隔离 CLAUDE_CONFIG_DIR 里没有登录态（登录走钥匙串），CLI 在读到 route 配置
      #      **之前**就按登录门挡下（`Not logged in · Please run /login`，一个请求都不发）；
      #   ② 本机 v1（历史旧版 .app）给进程树注入了整套 ANTHROPIC_* 管理 env，
      #      而**进程 env 会盖过 settings.json**（实测），所以不能靠继承。
      # 本格因此证明的是：**给定 route 两键时，真 CLI 的请求确实穿过 v2 本地代理，
      # 且代理做了模型名映射与记账**。「CLI 自己会不会从我们写的 settings.json 读 route」
      # 是另一条主张，需要真实登录态才能验 —— 见脚本收尾的边界 unknown。
      tier_env="$(jget "$(cat "$RUN/.claude/settings.json" 2>/dev/null || echo '{}')" '((d.get("env") or {}).get("ANTHROPIC_MODEL") or "")')"
      run_capped 30 "$out" "$err" env -u ANTHROPIC_API_KEY \
        ANTHROPIC_BASE_URL="http://127.0.0.1:${PROXY_PORT}/claude" \
        ANTHROPIC_AUTH_TOKEN="PROXY_MANAGED" \
        ANTHROPIC_MODEL="$tier_env" \
        CLAUDE_CONFIG_DIR="$RUN/.claude" \
        claude -p "reply with the single word stub-ok" ;;
    codex)
      # **只给 CODEX_HOME 必 401**（真机实测）：受管 config.toml 的 base_url 是**网关直连**
      # （家族契约 §A 行 3），而隔离配置目录里没有任何凭据副本（UC-019 的把凭据留在本机、
      # 由代理注入）⇒ 请求带着空凭据直发网关，网关回 `401 Unauthorized: Invalid token`
      # （原状实测：rc=124 超时复连 / 401，且**本地代理新增请求=0**）。
      # 正确形态与产品自己的物化启动命令**同源**：internal/api/materialize.go 的
      # codexLaunchCommand = (s.localRouteBase() + "/" + proxy.PrefixOpenAI + "/v1")，
      # 其中 localRouteBase() = "http://127.0.0.1:" + ProxyPort（internal/api/gatewayinput.go），
      # PrefixOpenAI = "openai"（internal/proxy/prefix.go）。故这里把同一拼法接上**本次运行的
      # 隔离代理端口**（不写死端口字面量），用 codex 自己的 `-c` 覆盖把请求导到本地代理：
      # 凭据由代理在转发时注入（与 claude 的 route 两键同口径）。
      run_capped 30 "$out" "$err" env CODEX_HOME="$RUN/.codex" \
        codex exec --skip-git-repo-check \
        -c "model_providers.custom.base_url=http://127.0.0.1:${PROXY_PORT}/openai/v1" \
        "reply with the single word ok" ;;
    opencode)
      run_capped 30 "$out" "$err" env XDG_CONFIG_HOME="$RUN/.config" XDG_DATA_HOME="$RUN/.local/share" \
        opencode run "reply with the single word ok" ;;
    pi)
      # --mode json：pi 的事件流逐条带 `"provider":"yzagmgr-<uid>","model":"<上游名>"`
      # —— 受管 provider id 只存在于**我们写的 models.json** 里，故这是「文件被 pi 解析」
      # 的直接回显（B 手段，recipe §3）。文本模式只会给一句 Connection error，无可判读信号。
      run_capped 30 "$out" "$err" env PI_CODING_AGENT_DIR="$RUN/.pi/agent" \
        pi -p --mode json "reply with the single word ok" ;;
    *)
      return 127 ;;
  esac
}

records_for() { # records_for <prefix...>：这些**数据面前缀**的用量记录总数（证明请求真的过了本地代理）
  local filter
  filter="$(printf '"%s",' "$@")"
  api GET "/usage/requests?limit=200" | jget - "sum(1 for b in d['data']['items'] if b['prefix'] in (${filter%,}))"
}

# ── 逐 CLI 往返 ────────────────────────────────────────────────────────────
IFS=',' read -r -a wanted <<<"$CLIS"
for fam in "${wanted[@]}"; do
  # 记账按**数据面前缀**落库，而 codex 的接管形态走 OpenAI 兼容别名（/openai/v1，见 run_cli
  # 的 -c 覆盖）⇒ 前缀集合不能只取家族名，否则真跑通了、「本地代理新增请求」仍是 0，
  # 证据自相矛盾（原状实测就是这么一条 0）。
  case "$fam" in
    codex) rec_prefixes=(codex openai) ;;
    *)     rec_prefixes=("$fam") ;;
  esac
  mpid="$(managed_card "$fam")"
  [[ -n "$mpid" ]] || die "${fam} 取不到受管卡"
  opid="$(official_card "$fam")"
  paths_raw="$(fam_paths "$fam")"
  IFS='|' read -r -a plist <<<"$paths_raw"
  primary="${plist[0]}"

  cli_path="$(command -v "$fam" 2>/dev/null || true)"
  [[ "$fam" == "claude-desktop" ]] && cli_path=""

  # additive 家族先放「用户自有内容」夹具（恢复初始快照才有可回的起点）。
  if [[ -z "$opid" ]]; then
    mkdir -p "$(dirname "$primary")"
    case "$fam" in
      opencode) printf '{\n  "provider": {\n    "user-owned": {\n      "options": { "baseURL": "https://user.example/v1", "apiKey": "user-key" }\n    }\n  }\n}\n' >"$primary" ;;
      pi) printf '{\n  "providers": {\n    "user-owned": {\n      "models": [ { "id": "user-model", "baseUrl": "https://user.example/v1" } ]\n    }\n  }\n}\n' >"$primary" ;;
      *) printf 'user-owned: https://user.example/v1\n' >"$primary" ;;
    esac
  fi

  # 归一化：先各走一遍两种档位，使「官方档」落到稳定形态（理由同 tier-roundtrip.sh）。
  apply_card "$fam" "$mpid"
  if [[ -n "$opid" ]]; then apply_card "$fam" "$opid"; else restore_initial "$fam"; fi
  start_sig="$(bytes_sig "${plist[@]}")"
  start_tier="$(tierof "$fam" "${plist[@]}")"

  # claude/codex 车道的请求都要**经本地代理**，故先在该车道上游挂上桩：这样「CLI 真的
  # 采纳了我们写的配置」有一个可判定的 200 终点（仅影响本轮 run 级）。
  #   - claude：请求进 /claude 前缀（settings.json 的 route 两键）；
  #   - codex：请求进 /openai 前缀（run_cli 的 `-c` 覆盖把 base_url 指到本地代理，
  #     拼法取自产品物化命令）。不挂桩的话代理会转发到 hermetic origin（按设计不可达），
  #     run 级就只能记 unknown —— 那就浪费了「接管形态已被采纳」这条本可判定的结论。
  case "$fam" in
    claude) api PUT "/proxy/routes/claude" "{\"upstream\":\"${STUB_URL}\"}" >/dev/null ;;
    codex)  api PUT "/proxy/routes/openai" "{\"upstream\":\"${STUB_URL}\"}" >/dev/null ;;
  esac

  config_ok=1
  byte_ok=1
  run_state="unknown"
  run_reason=""
  run_evidence=""
  run_ran=0
  run_this_round=0
  rounds=0
  for ((r = 1; r <= ROUNDS; r++)); do
    apply_card "$fam" "$mpid"
    gt="$(tierof "$fam" "${plist[@]}")"
    api_tier="$(api GET "/tools/$fam" | jget - 'd["data"]["tier"]')"
    rounds=$((rounds + 1))
    run_this_round=0   # 每轮重置：断言只认本轮真跑出的证据，不拿上一轮的记录冒充
    if [[ "$gt" != "gateway" || "$api_tier" != "gateway" ]]; then
      record_fail "${fam} 第${r}轮 网关档未落盘（盘上=${gt} API tier=${api_tier}）"
      config_ok=0
    fi

    # 真跑：只在本机装了该 CLI 时才跑（探不到即 unknown，不折成 pass）。
    # 前置不成立（无登录态 / 上游不可达 / 令牌缺失）的车道：第 1 轮登记后不再重复真跑
    # （同一前置不随轮次改变，重复只会把 30s 超时乘 N 轮）；已跑通的车道每轮照跑。
    if [[ -z "$cli_path" ]]; then
      run_state="unknown"
      run_reason="该 CLI 不在 PATH 上（本机未安装）"
      run_evidence="command -v ${fam} 为空"
    elif [[ "$run_ran" == "1" && "$run_state" != "pass" ]]; then
      # 上一轮真跑登记为 unknown（前置不成立：无登录态/上游不可达）⇒ 同一前置不会随轮次
      # 改变，本轮只复核配置级，不重复那 30s 超时。已 pass 的车道（claude route 级）则每轮
      # 照手册再跑一次 —— 它便宜（秒级）且是本格的强证据。
      : # 不重复真跑（本轮配置级断言照跑）
    else
      before_rec="$(records_for "${rec_prefixes[@]}")"
      rc=0
      run_cli "$fam" "$RUN/$fam.out" "$RUN/$fam.err" || rc=$?
      after_rec="$(records_for "${rec_prefixes[@]}")"
      out_txt="$(cat "$RUN/$fam.out" "$RUN/$fam.err" 2>/dev/null || true)"
      out_first="${out_txt%%$'\n'*}"
      req_n=$((after_rec - before_rec))
      run_ran=1
      run_this_round=1
      run_evidence="rc=${rc} 本地代理新增请求=${req_n} 输出首行=${out_first:0:120}"
      if printf '%s' "$out_txt" | grep -q "stub-ok"; then
        run_state="pass"
        run_reason=""
      elif (( rc == 124 )); then
        run_state="unknown"
        run_reason="CLI 在 30s 内未结束（隔离配置下等在网络上）"
      elif printf '%s' "$out_txt" | grep -qiE "not logged in|please run /login|authentication|401|403|未登录|credential"; then
        run_state="unknown"
        run_reason="CLI 在隔离配置目录里没有可用凭据（输出里出现鉴权/登录类信息）；这属真机 lane 的前置不成立，不是配置级的失败"
      elif printf '%s' "$out_txt" | grep -qiE "connection error|waiting for network|unreachable|connect|network"; then
        run_state="unknown"
        run_reason="CLI 采纳了配置但上游不可达（hermetic origin 按设计不可达；真机 lane 需真实网关与令牌）"
      elif (( rc == 0 )) && printf '%s' "$out_txt" | grep -q "deepseek-flash"; then
        # 输出里出现了我们写的模型名 ⇒ 配置确实被这个 CLI 读进去了（B 手段：CLI 自己解析了文件）。
        run_state="unknown"
        run_reason="CLI 已按我们的配置启动（输出含我们写的模型名），但没能取到应答"
      else
        run_state="unknown"
        run_reason="CLI 真跑未取到应答，且输出里没有可判别的信号"
      fi
    fi


    # claude 的 route 链路断言（三条都可判定，与「settings.json 是否被 CLI 采纳」分开记）。
    if [[ "$fam" == "claude" && "$run_this_round" == "1" ]]; then
      rec="$(api GET "/usage/requests?limit=5" | jget - 'json.dumps(next((b for b in d["data"]["items"] if b["prefix"]=="claude"), None), ensure_ascii=False, sort_keys=True) if any(b["prefix"]=="claude" for b in d["data"]["items"]) else ""')"
      if [[ -n "$rec" && "${req_n:-0}" -ge 1 ]]; then
        record_pass "claude 真跑级：真 CLI 的请求到达 v2 本地代理（用量新增 ${req_n} 条，prefix=claude status=$(jget "$rec" 'd["status"]') streamed=$(jget "$rec" 'd["streamed"]')）"
      else
        record_fail "claude 真跑级：请求未到达本地代理（新增 ${req_n:-0} 条）——给定 route 两键后应当至少有一条"
      fi
      mr="$(jget "$rec" 'str(d.get("modelRequested",""))')"
      mu="$(jget "$rec" 'str(d.get("modelUpstream",""))')"
      want_tier="${tier_env%\[1m\]}"
      if [[ -n "$rec" && "$mr" == "$want_tier"* && -n "$mu" && "$mu" != "$mr" ]]; then
        record_pass "claude 真跑级：代理做了模型名映射 modelRequested=${mr} → modelUpstream=${mu}（档名→上游名，UC-017 A3）"
      else
        record_fail "claude 真跑级：模型名映射不符 modelRequested=${mr:-<无>} modelUpstream=${mu:-<无>}（期望档名 ${want_tier} → 上游名）"
      fi
      # C 路 provider 记账面已随旧中继删除（design §1.1/§2.4）：该断言注销，
      # 真跑级 provider 口径待数据面轨在 P4 重做后回填（三态纪律：unknown ≠ pass）。
      record_unknown "claude 真跑级 provider 记账" "九前缀 C 路记账面已删除（design §2.4）" "解除=数据面轨重做真跑 provider 口径（P4/T7）"
    fi

    # 其它三家（codex/opencode/pi）的受管形态是**直连网关**（不经本地代理），
    # 因此没有 route 链路可断言。但它们的**自身输出**会回显我们写进配置文件的
    # provider/model（recipe §3 的 B 手段：文件被该 CLI 解析）—— 这是可判定的。
    if [[ "$run_this_round" == "1" && -n "${out_txt:-}" ]]; then
      case "$fam" in
        codex)
          if printf '%s' "$out_txt" | grep -q "deepseek-flash" && printf '%s' "$out_txt" | grep -qi "custom"; then
            record_pass "codex 真跑级：CLI 自身输出回显了我们写的 provider/model（provider=custom / model=deepseek-flash）⇒ 该 CLI 真的解析了我们写的 config.toml"
          fi
          # 接管形态的强证据（与 claude 的 route 三连同口径）：请求**确实**经本地代理到了桩。
          # 这是「只给 CODEX_HOME 会 401」那条老路的否定 —— 那一次本地代理新增请求恒为 0，
          # 因为凭据链根本没有本地代理这一环。
          if (( ${req_n:-0} >= 1 )) && printf '%s' "$out_txt" | grep -q "stub-ok"; then
            rec="$(api GET "/usage/requests?limit=5" | jget - 'json.dumps(next((b for b in d["data"]["items"] if b["prefix"]=="openai" and b["app"]=="codex"), None), ensure_ascii=False, sort_keys=True) if any(b["prefix"]=="openai" for b in d["data"]["items"]) else ""')"
            record_pass "codex 真跑级：请求到达 v2 本地代理（prefix=openai app=codex，用量新增 ${req_n} 条 status=$(jget "$rec" 'd["status"]')）⇒ CLI 采纳了 -c 覆盖的接管形态，凭据由代理注入（不再直发网关 401）"
          fi ;;
        opencode)
          if printf '%s' "$out_txt" | grep -q "deepseek-flash"; then
            record_pass "opencode 真跑级：CLI 自身输出回显了我们写的 model（deepseek-flash）⇒ 该 CLI 真的解析了我们写的 opencode.json"
          fi ;;
        pi)
          # ① 事件流回显：受管 provider id（yzagmgr-<uid>）只存在于我们写的 models.json。
          if printf '%s' "$out_txt" | grep -q '"provider":"yzagmgr-' && printf '%s' "$out_txt" | grep -q '"model":"'; then
            prov_echo="$(printf '%s' "$out_txt" | grep -o '"provider":"yzagmgr-[^"]*"' | head -1)"
            record_pass "pi 真跑级：事件流回显了我们写的受管 provider 与 model（${prov_echo} 等）⇒ 该 CLI 真的解析了我们写的 models.json"
          fi
          # ② 结构探针（离线、确定）：pi auth 必须从该文件解出受管条目的**凭据位**。
          #    只比较 sha256，不回显凭据明文（证据纪律）。
          pi_prov="$(jget "$(cat "$RUN/.pi/agent/models.json" 2>/dev/null || echo '{}')" 'next((k for k in (d.get("providers") or {}) if k.startswith("yzagmgr-")), "")')"
          file_key_sha="$(jget "$(cat "$RUN/.pi/agent/models.json" 2>/dev/null || echo '{}')" 'next((__import__("hashlib").sha256(((v.get("apiKey") or "")).encode()).hexdigest() for k, v in (d.get("providers") or {}).items() if k.startswith("yzagmgr-")), "")')"
          if [[ -n "$pi_prov" ]]; then
            run_capped 20 "$RUN/pi.auth.out" "$RUN/pi.auth.err" \
              env PI_CODING_AGENT_DIR="$RUN/.pi/agent" pi auth print-api-key --provider "$pi_prov"
            auth_rc=$?
            want_sha="$(python3 -c 'import hashlib,sys; print(hashlib.sha256(sys.stdin.read().strip().encode()).hexdigest())' <"$RUN/pi.auth.out" 2>/dev/null || echo "")"
            # 只比 sha256，不回显凭据明文（证据纪律：含密钥的文件只留决定键/指纹）。
            if (( auth_rc == 0 )) && [[ -n "$file_key_sha" && "$want_sha" == "$file_key_sha" ]]; then
              record_pass "pi 真跑级：pi auth print-api-key --provider ${pi_prov} 解出的凭据位与我们写在 models.json 的 apiKey **逐字节一致**（只比 sha256，不回显明文）⇒ 该 CLI 真的读了我们写的条目"
            else
              record_fail "pi 真跑级：pi auth 未解出受管条目的凭据位（rc=${auth_rc} 文件 apiKey 指纹=${file_key_sha:0:12} 实际输出指纹=${want_sha:0:12}）"
            fi
          fi ;;
        *) : ;;
      esac
    fi

    if [[ -n "$opid" ]]; then
      apply_card "$fam" "$opid"
      bt="$(tierof "$fam" "${plist[@]}")"
      if [[ "$bt" != "official" ]]; then
        record_fail "${fam} 第${r}轮 未回官方档（盘上=${bt}）"
        config_ok=0
      fi
    else
      restore_initial "$fam"
      bt="$(tierof "$fam" "${plist[@]}")"
      if [[ "$bt" == "gateway" ]]; then
        record_fail "${fam} 第${r}轮 恢复初始快照后托管条目仍在（盘上=${bt}）"
        config_ok=0
      fi
      if [[ "$(bytes_sig "${plist[@]}")" != "$start_sig" ]]; then
        record_fail "${fam} 第${r}轮 恢复后未回到起点字节"
        byte_ok=0
      fi
    fi
  done

  end_sig="$(bytes_sig "${plist[@]}")"
  if [[ "$end_sig" != "$start_sig" ]]; then
    byte_ok=0
    record_fail "${fam} 往返终点 != 起点（起点=${start_sig} 终点=${end_sig}）"
  fi

  if (( config_ok == 1 )); then
    record_pass "${fam} config:pass —— ${ROUNDS} 轮（${rounds}×2 次单向切换）每轮从盘上读出正确档位（起点分类=${start_tier}）"
  fi
  if (( byte_ok == 1 )); then
    record_pass "${fam} byte-exact:pass —— 往返终点与起点逐字节一致（${ROUNDS} 轮，签名 $(printf '%s' "$end_sig" | shasum -a 256 | cut -c1-12)）"
  fi

  case "$run_state" in
    pass)
      record_pass "${fam} run:pass —— CLI 真跑取到应答；证据：${run_evidence}" ;;
    unknown)
      # 注意 `${run_reason}` 必须带花括号：后面紧跟全角「；」时，bash 5.3 会把全角字符
      # 并进变量名（$run_reason；→ 未绑定的变量，实测踩到）。
      record_unknown "${fam} run（真跑级）" "${run_reason}；证据：${run_evidence}" \
        "在隔离配置目录里提供可用凭据（claude/codex 的真实登录态或真实推理令牌）与可达上游后重跑；配置级已在此断言"
      ;;
    *)
      record_fail "${fam} run —— ${run_reason}；证据：${run_evidence}" ;;
  esac
  # 每个 CLI 一行摘要（与手册 §4 的期望输出形状对齐，便于人工比对）。
  printf 'report  %s config:%s run:%s rounds:%d byte-exact:%s\n' \
    "$fam" "$([[ $config_ok == 1 ]] && echo pass || echo fail)" \
    "$([[ $run_state == pass ]] && echo pass || echo $run_state)" \
    "$((rounds * 2))" "$([[ $byte_ok == 1 ]] && echo pass || echo fail)"
done

# ── 本机未安装的四族：配置级照跑已在 tier-roundtrip.sh 覆盖，这里如实登记 ──
for fam in gemini grokbuild hermes openclaw; do
  if command -v "$fam" >/dev/null 2>&1; then
    record_pass "${fam} CLI 在 PATH 上（可用 --clis ${fam} 纳入真跑）"
  else
    record_unknown "${fam} run（真跑级）" \
      "本机未安装 ${fam}（command -v 为空）；配置级由 tier-roundtrip.sh 与 --clis 参数覆盖" \
      "安装 ${fam} 后以 --clis ${fam} 重跑"
  fi
done

# ── 边界：本格验到哪、没验到哪 ─────────────────────────────────────────────
# claude 的 route 级已验（真 CLI 请求到达本地代理 + 模型映射 + 记账），但这**不**证明
# 「CLI 会从我们写的 settings.json 里读 route」——那需要隔离目录里有可用登录态。
record_unknown "cli-roundtrip/claude「settings.json 被 CLI 采纳」" \
  "隔离 CLAUDE_CONFIG_DIR 无登录态（登录走钥匙串），CLI 在读取 route 配置**之前**就按登录门挡下（输出：Not logged in · Please run /login），故本轮是**脚手架显式给出 route 两键**驱动的；「CLI 自采纳 settings.json」这条主张无法由此判定" \
  "在隔离配置目录里具备可用登录态（如可复用的测试凭据）后，去掉显式 route 两键重跑 —— 那时只剩 settings.json 一条来源，才可判定"

verify_finish

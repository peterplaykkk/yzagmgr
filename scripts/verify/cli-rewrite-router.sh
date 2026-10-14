#!/usr/bin/env bash
# scripts/verify/cli-rewrite-router.sh —— UC-024 四 CLI 经 yzagmgr-cli 切本地 router 形态。
#
# 对 claude、opencode、pi、codex **四族**做 router 形态的 preview / apply / restore 往返：
#
#   逐家 preview 无副作用（含 mtime）→ apply 后盘上决定键 = router 形态（回环基址 +
#   对应前缀）→ 中间态与起点不同（非空洞）→ restore 后逐字节回起点（两形态都验）
#
# 与 cli-rewrite.sh（UC-022 A3：网关直连档往返）**并列**而非取代；UC-024 的落点在本脚本，
# cli-rewrite.sh 保持其 UC-022 证据角色不变。
#
# ── 本脚本冻结的可执行契约（能力未交付 ⇒ 逐断言如实红；实现方按此对齐后本脚本转绿）──
# 形态发现（沿 UC-022 同一 REST 面，零新端点）：
#   `yzagmgr tools show <tool> --json` 的 `data.profiles.items` 含恰一张 **kind="router"**
#   的档位卡（pid 动态取，不写死；命名建议 `yzagmgr-router`）。router 形态的 preview/apply
#   走 UC-022 既有子命令：`yzagmgr preview|apply <tool> --profile <pid> --json`——「一键」=
#   每家一条 apply、无中间编辑步骤；restore 沿 UC-022 双形态。
# router 形态决定键（**回环基址 + 对应前缀**；回环基址 = http://127.0.0.1:<PROXY_PORT>）：
#   claude   env.ANTHROPIC_BASE_URL              == {回环}/router
#   opencode provider["yzagmgr-*"].options.baseURL  == {回环}/router/v1
#   pi       providers["yzagmgr-*"].models[0].baseUrl == {回环}/router/v1
#   codex    [model_providers.custom].base_url   == {回环}/router/v1   ← **落盘形态**
#   取值依据（不另造第二套命名）：
#     - 数据面契约 api-v1.md §7.1：POST /router/v1/messages（原 router-three-arm.sh 已删）；
#       claude 的请求路径 = {base}/v1/messages ⇒ base=…/router 与该路径逐字一致；
#     - codex/opencode/pi 的 `{base}/v1/…` 约定 ⇒ 请求落 /router/v1/{responses,
#       chat/completions}，同一 /router/v1 名下的兄弟 wire（proxy 侧 `arm.Endpoint+rest`
#       直通，wire 由路径表达）；网关形态里的 `/openai/v1` 之 `openai` 是**网关车道名**，
#       router 数据面无车道段；
#     - 选臂 = 请求体 `model` = 命名路由名/alias（api-v1.md §7.1），不在 URL 里发明臂段。
#   codex **落盘形态**：base_url 直接写进 live `config.toml`（`[model_providers.custom]`），
#   不是物化启动命令的 `-c` 覆盖（materialize.go codexLaunchCommand 的 `-c` 形态不算落盘）；
#   并按 A2 做「可被 codex 读取」的**配置级**断言（TOML 可解析 + 段键齐全）。
# 幂等（A4）：对已是 router 形态的 live 配置重复 apply 返回 `status:"unchanged"`、退出码 0、
#   全量 file_meta（exists/size/sha256/mtime_ns）不变（零多余写盘）；备份记账按 UC-003 A7
#   属预期——unchanged 事务仍记写前备份（与 v1 一致，api-v1.md §4.3「backupIds 不因幂等
#   复用而清空」、tx_test.go 钉死），backupIds/backups 总数**不作**幂等判据。
# 错误（A5）：对不存在/未登记的工具 id 执行 apply ⇒ 退出码 2（用法）或 1（业务），
#   全部受管路径 file_meta 零变化。
#
# 非空洞判据（缺一即本脚本失败）：apply 后中间态签名**必须**与起点不同；决定键由**独立
# 分类器**（routeform.py，只读盘上字节，不复用产品 Probe()）判定；preview 零副作用比较
# file_meta 全量（exists/size/sha256/mtime_ns）。
#
# 边界与纪律：
#   - 全程 YZAGMGR_HOME 隔离根：绝不触碰用户真实的 ~/.pi、~/.config/opencode、~/.claude、~/.codex；
#   - 被验操作一律经 bin/yzagmgr CLI；唯一的 curl 是 lib.sh 的守护进程管理助手；
#   - 版本探测白名单置空（YZAGMGR_TEST_VERSION_PROBE_FAMILIES 空值语义）：本格只验配置
#     文件 rewrite，**不执行任何真实 CLI 二进制**。
#
# 断言 ↔ UC-024 验收行：
#   TC-UC024-A1 四族一键指向（router 档位卡在册 + preview/apply 后盘上决定键 = 回环基址
#               + 对应前缀 + 独立分类器判 route + 中间态≠起点；四家集体同为 router 形态）
#   TC-UC024-A2 codex 落盘形态（config.toml 内 [model_providers.custom].base_url 指向
#               本地 router；TOML 可解析、可被 codex 读取——配置级断言，非 -c 覆盖）
#   TC-UC024-A3 往返字节回起点（preview 无副作用；restore 双形态——claude 裸 restore、
#               opencode/pi/codex restore --backup <初始快照>——四族逐字节回起点；
#               claude 裸 restore 必须落在「该次写入的写前备份 = 全库最新备份」的窗口内，
#               见主流程 3 的顺序纪律）
#   TC-UC024-A4 幂等（重复 apply = unchanged + 零多余写盘；备份记账按 UC-003 A7 属预期）
#   TC-UC024-A5 目标不存在（apply 未知工具 id ⇒ 退出码 2/1 + 盘上零变化）
#
# 用法: cli-rewrite-router.sh [--families opencode,pi,codex,claude]
#   往返次序即 --families 顺序；claude 必须**殿后 apply**——其 A3 裸 restore 腿在 apply 后
#   **立即**执行（裸 restore = 恢复全库最新那份备份，doc/cli.md「restore 的两种形态」的
#   选择规则，须精确命中该次写入的写前备份 = 全库最新那条），并**先于** A4 的重复 apply：
#   UC-003 A7 钉死 unchanged 仍记备份，重复 apply 会落下一条更新的写前备份（内容=
#   router 形态），此后裸 restore 只会恢复那条、按契约不动盘。往返归位后重新 apply 回
#   router 形态，再跑 A4 与集体断言。
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib.sh
. "$HERE/lib.sh"

FAMILIES="opencode,pi,codex,claude"
while (($# > 0)); do
  case "$1" in
    --families) shift; FAMILIES="${1:-}" ;;
    *) die "未知参数 $1" ;;
  esac
  shift
done

# 清掉会污染被测进程的宿主注入（cli-rewrite.sh 同款纪律）。
unset ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN ANTHROPIC_BASE_URL ANTHROPIC_MODEL 2>/dev/null || true
unset CLAUDE_CONFIG_DIR PI_CODING_AGENT_DIR XDG_CONFIG_HOME XDG_DATA_HOME CODEX_HOME 2>/dev/null || true

verify_init cli-rewrite-router
seed_test_policy "https://gateway.example.com"
# 版本探测白名单置空 = 跳过一切真实 CLI 探测（YZAGMGR_TEST_VERSION_PROBE_FAMILIES 的空值语义）。
export YZAGMGR_TEST_VERSION_PROBE_FAMILIES=""
start_isolated_daemon

YZAGMGR="$ROOT/bin/yzagmgr"
[[ -x "$YZAGMGR" ]] || die "bin/yzagmgr 不存在（CLI 未构建）。先执行 make build。"
require_python

EV="$RUN/evidence"
mkdir -p "$EV"
EVN=0
CLI_LAST=""
ROUTER_BASE="http://127.0.0.1:${PROXY_PORT}/router"

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

# ── CLI 调用助手（被验操作只经 CLI；输出逐次落独立证据文件）──────────────────

cli_run() { # cli_run <label> <yzagmgr 参数...>：返回 CLI 退出码；stdout/stderr 落 $EV
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

# ── 独立形态分类器（只读决定键字节；不复用产品 Probe()）──────────────────────
#
# routeform.py 两用：
#   classify <family> <path...>          ⇒ route|gateway|official|absent|invalid
#   decide   <family> <path...>          ⇒ 决定键取值（无则空行）
#   codex-readable <path>                ⇒ readable:<method>|unreadable:<原因>
cat >"$RUN/routeform.py" <<'ROUTEFORM'
import json, re, sys, pathlib

def pick(paths, *needles):
    best = ""
    for p in paths:
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

def toml_load(t):
    try:
        import tomllib  # py3.11+；Mac 自带 3.9 无 tomllib，走正则回落
        return ("tomllib", tomllib.loads(t))
    except Exception:
        pass
    # 结构化正则回落：[section] + key = "value"（仅取本断言需要的键位）
    doc, cur = {}, ""
    for line in t.splitlines():
        m = re.match(r"\s*\[([^\]]+)\]\s*$", line)
        if m:
            cur = m.group(1).strip()
            doc.setdefault(cur, {})
            continue
        m = re.match(r"\s*([A-Za-z0-9_.-]+)\s*=\s*(.+?)\s*$", line)
        if m:
            raw = m.group(2)
            vm = re.match(r'"([^"]*)"', raw)
            val = vm.group(1) if vm else raw
            if cur == "":
                doc.setdefault("", {})[m.group(1)] = val
            else:
                doc.setdefault(cur, {})[m.group(1)] = val
    return ("regex", doc)

def decide(family, paths):
    if family == "claude":
        t = read(pick(paths, ".claude/settings.json", ".claude/claude.json", "settings.json"))
        if t is None:
            return None
        return ((jload(t) or {}).get("env", {}) or {}).get("ANTHROPIC_BASE_URL")
    if family == "opencode":
        t = read(pick(paths, "opencode.json"))
        if t is None:
            return None
        prov = (jload(t) or {}).get("provider", {}) or {}
        for k, v in prov.items():
            if k.startswith("yzagmgr-"):
                return ((v or {}).get("options", {}) or {}).get("baseURL")
        return None
    if family == "pi":
        t = read(pick(paths, "models.json"))
        if t is None:
            return None
        prov = (jload(t) or {}).get("providers", {}) or {}
        for k, v in prov.items():
            if not k.startswith("yzagmgr-"):
                continue
            if v.get("baseUrl"):
                return v.get("baseUrl")
            models = v.get("models") or []
            if models:
                return (models[0] or {}).get("baseUrl")
        return None
    if family == "codex":
        t = read(pick(paths, "config.toml"))
        if t is None:
            return None
        method, doc = toml_load(t)
        if method == "tomllib":
            return ((doc.get("model_providers") or {}).get("custom") or {}).get("base_url")
        return (doc.get("model_providers.custom", {}) or {}).get("base_url") \
            or (doc.get('model_providers."custom"', {}) or {}).get("base_url")
    return None

def classify(value, present):
    if not present:
        return "absent"
    if value is None or value == "":
        return "official"
    try:
        from urllib.parse import urlparse
        host = urlparse(value).hostname or ""
    except Exception:
        return "invalid"
    if host in ("127.0.0.1", "::1"):
        return "route" if re.match(r"^https?://127\.0\.0\.1(:\d+)?/router(/|$)", value) else "gateway"
    return "official"

def codex_readable(path):
    t = read(path)
    if t is None:
        return "unreadable:文件不存在"
    method, doc = toml_load(t)
    if method == "regex" and not re.search(r"^\s*\[model_providers\.[\"']?custom[\"']?\]\s*$", t, re.M):
        return "unreadable:缺 [model_providers.custom] 段"
    top = doc.get("", {}) or {}
    custom = doc.get("model_providers.custom", {}) or doc.get('model_providers."custom"', {}) or {}
    if method == "tomllib":
        prov = (doc.get("model_providers") or {}).get("custom") or {}
        top, custom = doc, prov
        if not prov:
            return "unreadable:缺 [model_providers.custom] 段"
        if top.get("model_provider") != "custom":
            return "unreadable:顶层 model_provider != custom（codex 不会选中托管 provider）"
        if not custom.get("base_url"):
            return "unreadable:[model_providers.custom].base_url 缺失"
        return "readable:tomllib"
    if top.get("model_provider") != "custom":
        return "unreadable:顶层 model_provider != custom（codex 不会选中托管 provider）"
    if not custom.get("base_url"):
        return "unreadable:[model_providers.custom].base_url 缺失"
    return "readable:regex"

cmd = sys.argv[1]
if cmd == "classify":
    paths = [p for p in sys.argv[3:] if p]
    value = decide(sys.argv[2], paths)
    present = any(pathlib.Path(p).exists() for p in paths)
    print(classify(value, present))
elif cmd == "decide":
    v = decide(sys.argv[2], [p for p in sys.argv[3:] if p])
    print(v if v is not None else "")
elif cmd == "codex-readable":
    print(codex_readable(sys.argv[2]))
else:
    print("unknown-cmd")
ROUTEFORM

routeform() { python3 "$RUN/routeform.py" "$@"; }

# ── 签名助手（与 cli-rewrite.sh 同法）────────────────────────────────────────

bytes_sig() {
  local out="" p
  while IFS= read -r p; do
    [[ -z "$p" ]] && continue
    if [[ -f "$p" ]]; then out="${out}${p}=$(sha256_file "$p")|"; else out="${out}${p}=<absent>|"; fi
  done < <(printf '%s\n' "$@" | LC_ALL=C sort)
  printf '%s' "${out%|}"
}
meta_sig() {
  local out="" p
  while IFS= read -r p; do
    [[ -z "$p" ]] && continue
    out="${out}${p}=$(file_meta "$p")|"
  done < <(printf '%s\n' "$@" | LC_ALL=C sort)
  printf '%s' "${out%|}"
}

# router 形态期望决定值（回环基址 + 对应前缀；依据见头注释契约段）。
expected_base() {
  case "$1" in
    claude) printf '%s' "$ROUTER_BASE" ;;
    opencode|pi|codex) printf '%s' "${ROUTER_BASE}/v1" ;;
    *) printf '' ;;
  esac
}

# ── 起点夹具（用户自有内容；只写 CLI 报告的受管主路径）────────────────────────

write_fixture() { # write_fixture <fam> <primary>
  local fam="$1" p="$2"
  mkdir -p "$(dirname "$p")"
  case "$fam" in
    opencode) printf '{\n  "provider": {\n    "user-owned": {\n      "options": { "baseURL": "https://user.example/v1", "apiKey": "user-key" }\n    }\n  }\n}\n' >"$p" ;;
    pi) printf '{\n  "providers": {\n    "user-owned": {\n      "models": [ { "id": "user-model", "baseUrl": "https://user.example/v1" } ]\n    }\n  }\n}\n' >"$p" ;;
    claude) printf '{\n  "model": "user-model",\n  "env": { "USER_KEEP_ME": "1" }\n}\n' >"$p" ;;
    codex) printf '# user-owned codex config\n[mcp_servers]\nuser-tool = { command = "echo" }\n\n[profiles.user]\nmodel = "user-model"\n' >"$p" ;;
    *) printf 'user-owned: https://user.example/v1\n' >"$p" ;;
  esac
}

# ── 逐家族收集工具面信息 + 起点夹具 ───────────────────────────────────────────

IFS=',' read -r -a wanted <<<"$FAMILIES"
declare -a PLIST=()
declare -A PIDS=() START_SIG=() START_META=() START_CLASS=() PRIM=()

discover_ok=1
for fam in "${wanted[@]}"; do
  rc=0; cli_run "show-${fam}" tools show "$fam" --json || rc=$?
  paths_raw="$(cli_jget '"|".join(p["path"] for p in d["data"]["tool"]["paths"])')"
  rpid="$(cli_jget 'next((p["id"] for p in d["data"]["profiles"]["items"] if p.get("kind")=="router"), "")')"
  if [[ "$rc" != "0" || -z "$paths_raw" ]]; then
    fail_ev "TC-UC024-A1[${fam}] tools show 受管路径" \
      "退出码 0 且 data.tool.paths 非空" \
      "退出码 ${rc}，paths=${paths_raw:-<空>}，out=$(cli_out)" \
      "$EV/${CLI_LAST}.out"
    continue
  fi
  IFS='|' read -r -a plist <<<"$paths_raw"
  PLIST+=("${plist[@]}")
  PRIM[$fam]="${plist[0]}"
  write_fixture "$fam" "${plist[0]}"
  START_SIG[$fam]="$(bytes_sig "${plist[@]}")"
  START_META[$fam]="$(meta_sig "${plist[@]}")"
  START_CLASS[$fam]="$(routeform classify "$fam" "${plist[@]}")"

  if [[ -n "$rpid" ]]; then
    PIDS[$fam]="$rpid"
    pass_ev "TC-UC024-A1[${fam}] router 档位卡在册：kind=router，pid=${rpid}（受管路径 $(printf '%s' "$paths_raw" | tr '|' ' ')）"
  else
    PIDS[$fam]="router"
    discover_ok=0
    fail_ev "TC-UC024-A1[${fam}] router 档位卡在册" \
      "data.profiles.items 含恰一张 kind=router 的档位卡（router 形态的一键指向入口）" \
      "无 kind=router 条目（profiles kinds=$(cli_jget '",".join(sorted({p.get("kind","") for p in d["data"]["profiles"]["items"]}))')）；后续腿按冻结 pid 字面量 router 真跑取证据" \
      "$EV/${CLI_LAST}.out"
  fi
done

# ── 主流程 1：逐家 preview（A3：纯计算不落盘——全量元数据一字不变）────────────

for fam in "${wanted[@]}"; do
  [[ -n "${PRIM[$fam]:-}" ]] || continue
  rc=0; cli_run "show2-${fam}" tools show "$fam" --json || rc=$?
  paths_raw="$(cli_jget '"|".join(p["path"] for p in d["data"]["tool"]["paths"])')"
  IFS='|' read -r -a plist <<<"$paths_raw"

  rc=0; cli_run "preview-${fam}" preview "$fam" --profile "${PIDS[$fam]}" --json || rc=$?
  pvid="$(cli_jget 'str(d["data"].get("previewId",""))')"
  pfiles="$(cli_jget 'len(d["data"].get("files") or [])')"
  if [[ "$rc" != "0" || -z "$pvid" || "$pfiles" == "0" ]]; then
    fail_ev "TC-UC024-A3[${fam}] preview 变更计划" \
      "退出码 0 且 previewId 返回、计划含 ≥1 文件（router 形态）" \
      "退出码 ${rc}，previewId=${pvid:-<空>}，files=${pfiles}，out=$(cli_out)" \
      "$EV/${CLI_LAST}.out"
    fail_ev "TC-UC024-A3[${fam}] preview 无副作用" \
      "全量 file_meta（exists/size/sha256/mtime_ns）一字不变" \
      "preview 未产出计划，副作用判据以起点为准记录：meta 与起点一致=$([[ "$(meta_sig "${plist[@]}")" == "${START_META[$fam]}" ]] && echo yes || echo no)" \
      "$EV/${CLI_LAST}.out"
    continue
  fi
  if [[ "$(meta_sig "${plist[@]}")" == "${START_META[$fam]}" ]]; then
    pass_ev "TC-UC024-A3[${fam}] preview 无副作用：受管路径 exists/size/sha256/mtime_ns 与预览前逐字节一致"
  else
    fail_ev "TC-UC024-A3[${fam}] preview 零副作用" \
      "全量 file_meta（exists/size/sha256/mtime_ns）一字不变" \
      "预览后盘上元数据被改（mtime/内容/存在性）" \
      "$EV/${CLI_LAST}.out"
  fi
done

# ── 主流程 2：逐家 apply（A1 router 形态 + 非空洞；codex 叠 A2）+ A4 幂等 ────

for fam in "${wanted[@]}"; do
  [[ -n "${PRIM[$fam]:-}" ]] || continue
  rc=0; cli_run "show3-${fam}" tools show "$fam" --json || rc=$?
  paths_raw="$(cli_jget '"|".join(p["path"] for p in d["data"]["tool"]["paths"])')"
  IFS='|' read -r -a plist <<<"$paths_raw"
  want_base="$(expected_base "$fam")"

  rc=0; cli_run "apply-${fam}" apply "$fam" --profile "${PIDS[$fam]}" --json || rc=$?
  astatus="$(cli_jget 'str(d["data"].get("status",""))')"
  if [[ "$rc" != "0" || "$astatus" != "applied" ]]; then
    fail_ev "TC-UC024-A1[${fam}] apply 切 router 形态" \
      "退出码 0 且 status=applied（写前备份语义，backupIds 非空）" \
      "退出码 ${rc}，status=${astatus:-<空>}，out=$(cli_out)" \
      "$EV/${CLI_LAST}.out"
    # 级联登记：依赖 apply 落盘的判据一并如实红，断言清单与 UC 行 1:1 对应。
    fail_ev "TC-UC024-A1[${fam}] apply 后决定键 = 回环基址 + 对应前缀" \
      "$(expected_base "$fam")" \
      "apply 未落盘，判据不成立（先看 apply 断言红项）" \
      "$EV/${CLI_LAST}.out"
    fail_ev "TC-UC024-A1[${fam}] 独立分类器判 route" \
      "route（受管条目指向请求映射前缀）" \
      "apply 未落盘，分类仍为起点侧（$(routeform classify "$fam" "${plist[@]}")）" \
      "${plist[*]}"
    fail_ev "TC-UC024-A1[${fam}] 非空洞判据" \
      "apply 后中间态签名 ≠ 起点签名" \
      "apply 未落盘，中间态 == 起点（空洞）" \
      "${plist[*]}"
    if [[ "$fam" == "codex" ]]; then
      fail_ev "TC-UC024-A2 codex 落盘形态（config.toml 直接落盘 + 可被 codex 读取）" \
        "TOML 可解析、顶层 model_provider=custom、[model_providers.custom].base_url=$(expected_base codex)" \
        "apply 未落盘，落盘形态判据不成立（先看 apply 断言红项）" \
        "${plist[0]}"
    fi
    fail_ev "TC-UC024-A4[${fam}] 幂等（unchanged + 零多余写盘）" \
      "退出码 0 且 status=unchanged 且全量 file_meta 不变（零多余写盘）｜备份记账按 UC-003 A7 属预期，不作判据" \
      "首刀 apply 未落盘，幂等判据不成立（先看 apply 断言红项）" \
      "$EV/${CLI_LAST}.out"
    continue
  fi

  # A1：盘上决定键 = 回环基址 + 对应前缀（独立分类器 + 精确取值双判）。
  got="$(routeform decide "$fam" "${plist[@]}")"
  cls="$(routeform classify "$fam" "${plist[@]}")"
  if [[ "$got" == "$want_base" ]]; then
    pass_ev "TC-UC024-A1[${fam}] apply 后盘上决定键 = router 形态：${got}（回环基址 http://127.0.0.1:${PROXY_PORT} + 对应前缀）"
  else
    fail_ev "TC-UC024-A1[${fam}] apply 后决定键 = 回环基址 + 对应前缀" \
      "${want_base}" \
      "${got:-<空>}（分类=${cls}）" \
      "${plist[*]}"
  fi
  if [[ "$cls" == "route" ]]; then
    pass_ev "TC-UC024-A1[${fam}] 独立分类器判 route（起点分类=${START_CLASS[$fam]}）"
  else
    fail_ev "TC-UC024-A1[${fam}] 独立分类器判 route" \
      "route（受管条目指向请求映射前缀）" \
      "${cls}" \
      "${plist[*]}"
  fi

  # 非空洞判据：中间态与起点不同。
  mid="$(bytes_sig "${plist[@]}")"
  if [[ "$mid" != "${START_SIG[$fam]}" ]]; then
    pass_ev "TC-UC024-A1[${fam}] 非空洞判据：apply 后中间态与起点**不同**（router 形态真的改了盘上字节）"
  else
    fail_ev "TC-UC024-A1[${fam}] 非空洞判据" \
      "apply 后中间态签名 ≠ 起点签名" \
      "两者完全相同 —— 空洞往返，什么都没改" \
      "${plist[*]}"
  fi

  # A2（codex 落盘形态 + 配置级可读）。
  if [[ "$fam" == "codex" ]]; then
    cfg="${plist[0]}"
    for p in "${plist[@]}"; do [[ "$p" == *config.toml ]] && cfg="$p"; done
    readable="$(routeform codex-readable "$cfg")"
    if [[ "$readable" == readable:* && "$got" == "$want_base" ]]; then
      pass_ev "TC-UC024-A2 codex 落盘形态：config.toml 内 [model_providers.custom].base_url=${got} 直接落盘（非 -c 覆盖），且可被 codex 读取（${readable}）"
    else
      fail_ev "TC-UC024-A2 codex 落盘形态（config.toml 直接落盘 + 可被 codex 读取）" \
        "TOML 可解析、顶层 model_provider=custom、[model_providers.custom].base_url=${want_base}" \
        "${readable}；base_url=${got:-<空>}" \
        "$cfg"
    fi
  fi

  # A3[claude] 裸 restore 腿（顺序纪律见头注释「用法」段）：
  # 裸 restore 的契约 = **恢复全库最新那份备份**（doc/cli.md「restore 的两种形态」，按备份
  # 编号分配序精确排序）；要让它「逐字节撤销本次写入、回起点」，必须在本次写入的写前备份
  # 仍是全库最新那条的窗口内执行——即**先于** A4 的重复 apply（UC-003 A7：unchanged 仍记
  # 备份，重复 apply 会插入一条更新的写前备份，其内容=router 形态，此后裸 restore 恢复它
  # 只会得到「unchanged 不动盘」，那是契约行为而非回起点通道）。
  if [[ "$fam" == "claude" ]]; then
    rc=0; cli_run "restore-${fam}" restore --json || rc=$?
    restored_via="裸 restore（撤销最近一次写入，目标=$(cli_jget 'str(d["data"].get("backupId",""))')）"
    if [[ "$rc" != "0" ]]; then
      fail_ev "TC-UC024-A3[${fam}] restore 后逐字节回起点" \
        "退出码 0（${restored_via}）" \
        "退出码 ${rc}，out=$(cli_out)" \
        "$EV/${CLI_LAST}.out"
      fail_ev "TC-UC024-A3[${fam}] restore 后托管条目摘除" \
        "分类 != route（router 形态条目被撤销）" \
        "restore 未执行成功，router 形态仍在（先看 restore 断言红项）" \
        "${plist[*]}"
      # restore 失败时盘上仍是 router 形态，A4/集体的前置成立，直接落 A4。
    else
      end_sig="$(bytes_sig "${plist[@]}")"
      end_class="$(routeform classify "$fam" "${plist[@]}")"
      if [[ "$end_sig" == "${START_SIG[$fam]}" ]]; then
        pass_ev "TC-UC024-A3[${fam}] restore 后逐字节回到起点（经 ${restored_via}，签名 $(printf '%s' "$end_sig" | shasum -a 256 | cut -c1-12)）"
      else
        fail_ev "TC-UC024-A3[${fam}] restore 后逐字节回起点" \
          "终点签名 == 起点签名（${START_SIG[$fam]}）" \
          "终点签名 ${end_sig}" \
          "${plist[*]}"
      fi
      if [[ "$end_class" != "route" ]]; then
        pass_ev "TC-UC024-A3[${fam}] restore 后回到起点侧（分类=${end_class}，起点=${START_CLASS[$fam]}）"
      else
        fail_ev "TC-UC024-A3[${fam}] restore 后托管条目摘除" \
          "分类 != route（router 形态条目被撤销）" \
          "分类=route（router 形态仍在）" \
          "${plist[*]}"
      fi
      # 复位：A4 与集体断言要求 router 形态——重新 apply（判据=分类回 route）。
      rc=0; cli_run "reapply-${fam}" apply "$fam" --profile "${PIDS[$fam]}" --json || rc=$?
      rstatus="$(cli_jget 'str(d["data"].get("status",""))')"
      cls2="$(routeform classify "$fam" "${plist[@]}")"
      if [[ "$rc" != "0" || "$cls2" != "route" ]]; then
        fail_ev "TC-UC024-A1[${fam}] A3 往返后复位 router 形态" \
          "裸 restore 往返后重新 apply ⇒ 退出码 0 且分类回 route（A4/集体前置）" \
          "退出码 ${rc}，status=${rstatus:-<空>}，分类=${cls2}，out=$(cli_out)" \
          "$EV/${CLI_LAST}.out"
        fail_ev "TC-UC024-A4[${fam}] 幂等（unchanged + 零多余写盘）" \
          "复位后重复 apply = unchanged + 零多余写盘" \
          "复位未回到 router 形态（先看复位断言红项），幂等判据不成立" \
          "$EV/${CLI_LAST}.out"
        continue
      fi
    fi
  fi

  # A4 幂等：重复 apply = unchanged + 零多余写盘（file_meta 含 mtime_ns 全量不变）。
  # 备份记账按 UC-003 A7 属预期（unchanged 事务仍记写前备份，与 v1 一致；api-v1.md §4.3
  # 「backupIds 不因幂等复用而清空」），backupIds/backups 总数不作幂等判据。
  meta_before="$(meta_sig "${plist[@]}")"
  rc=0; cli_run "apply2-${fam}" apply "$fam" --profile "${PIDS[$fam]}" --json || rc=$?
  astatus2="$(cli_jget 'str(d["data"].get("status",""))')"
  meta_after="$(meta_sig "${plist[@]}")"
  if [[ "$rc" == "0" && "$astatus2" == "unchanged" && "$meta_before" == "$meta_after" ]]; then
    pass_ev "TC-UC024-A4[${fam}] 幂等：重复 apply 返回 unchanged、退出码 0、全部受管文件 file_meta（exists/size/sha256/mtime_ns）不变（零多余写盘）｜备份记账按 UC-003 A7 属预期（unchanged 仍记写前备份），不作幂等判据"
  else
    fail_ev "TC-UC024-A4[${fam}] 幂等（unchanged + 零多余写盘）" \
      "退出码 0 且 status=unchanged 且全部受管文件 file_meta（exists/size/sha256/mtime_ns）不变（零多余写盘）｜备份记账按 UC-003 A7 属预期，不作判据" \
      "退出码 ${rc}，status=${astatus2:-<空>}，meta 变化=$([[ "$meta_before" == "$meta_after" ]] && echo no || echo yes)" \
      "$EV/${CLI_LAST}.out"
  fi
done

# ── A1 集体断言：apply 后四家同一时刻**都**是 router 形态（「四族一键指向」）────

collective_ok=1
declare -a collective_bad=()
for fam in "${wanted[@]}"; do
  [[ -n "${PRIM[$fam]:-}" ]] || { collective_ok=0; collective_bad+=("${fam}:未就绪"); continue; }
  rc=0; cli_run "show4-${fam}" tools show "$fam" --json || rc=$?
  paths_raw="$(cli_jget '"|".join(p["path"] for p in d["data"]["tool"]["paths"])')"
  IFS='|' read -r -a plist <<<"$paths_raw"
  cls="$(routeform classify "$fam" "${plist[@]}")"
  if [[ "$cls" != "route" ]]; then
    collective_ok=0
    collective_bad+=("${fam}:${cls}")
  fi
done
if (( collective_ok == 1 )); then
  pass_ev "TC-UC024-A1[集体] 四族一键指向：claude、opencode、pi、codex 同一时刻均为请求映射形态（分类全 route）"
else
  fail_ev "TC-UC024-A1[集体] 四族一键指向" \
    "apply 后四家 live 配置同一时刻均为 router 形态（分类全 route）" \
    "失败项: $(printf '%s ' "${collective_bad[@]}")" \
    "$EV"
fi

# ── 主流程 4：restore 逐字节回起点（A3 第二形态：restore --backup <初始快照>）────────
#
# claude 的裸 restore 腿已在主流程 3 内完成（顺序纪律：裸 restore = 恢复全库最新那份备份，
# 必须落在「该次写入的写前备份 = 全库最新备份」的窗口内；UC-003 A7 的 unchanged 备份会
# 插入更新记录，详见头注释「用法」段）。本段只做其余三家的显式初始快照恢复——编号显式
# 指定，不受全库备份次序影响。

restore_one() { # restore_one <fam>：经 restore --backup <初始快照> 回起点（claude 已在主流程 3 走裸 restore）
  local fam="$1" rc restored_via end_sig end_class
  rc=0; cli_run "show5-${fam}" tools show "$fam" --json || rc=$?
  paths_raw="$(cli_jget '"|".join(p["path"] for p in d["data"]["tool"]["paths"])')"
  IFS='|' read -r -a plist <<<"$paths_raw"

  rc_list=0; cli_run "backups-${fam}" backups list --json || rc_list=$?
  bk="$(cli_jget 'next((b["backupId"] for b in d["data"]["items"] if b.get("toolId")=="'"${fam}"'" and b.get("initial")), "")')"
  if [[ "$rc_list" != "0" || -z "$bk" ]]; then
    fail_ev "TC-UC024-A3[${fam}] 初始快照在册" \
      "backups list 给出该族 initial 快照（restore --backup 的目标）" \
      "退出码 ${rc_list}，bk=${bk:-<空>}" \
      "$EV/${CLI_LAST}.out"
    return 0
  fi
  rc=0; cli_run "restore-${fam}" restore --backup "$bk" --json || rc=$?
  restored_via="restore --backup ${bk}（初始快照）"
  if [[ "$rc" != "0" ]]; then
    fail_ev "TC-UC024-A3[${fam}] restore 回起点" \
      "退出码 0（${restored_via:-restore}）" \
      "退出码 ${rc}，out=$(cli_out)" \
      "$EV/${CLI_LAST}.out"
    return 0
  fi
  end_sig="$(bytes_sig "${plist[@]}")"
  end_class="$(routeform classify "$fam" "${plist[@]}")"
  if [[ "$end_sig" == "${START_SIG[$fam]}" ]]; then
    pass_ev "TC-UC024-A3[${fam}] restore 后逐字节回到起点（经 ${restored_via}，签名 $(printf '%s' "$end_sig" | shasum -a 256 | cut -c1-12)）"
  else
    fail_ev "TC-UC024-A3[${fam}] restore 后逐字节回起点" \
      "终点签名 == 起点签名（${START_SIG[$fam]}）" \
      "终点签名 ${end_sig}" \
      "${plist[*]}"
  fi
  if [[ "$end_class" != "route" ]]; then
    pass_ev "TC-UC024-A3[${fam}] restore 后回到起点侧（分类=${end_class}，起点=${START_CLASS[$fam]}）"
  else
    fail_ev "TC-UC024-A3[${fam}] restore 后托管条目摘除" \
      "分类 != route（router 形态条目被撤销）" \
      "分类=route（router 形态仍在）" \
      "${plist[*]}"
  fi
}

for fam in "${wanted[@]}"; do
  [[ "$fam" == "claude" ]] && continue
  [[ -n "${PRIM[$fam]:-}" ]] && restore_one "$fam"
done

# ── A5（负·目标不存在）：未知工具 id ⇒ 退出码 2/1 + 全部受管路径零变化 ─────────

meta_all_before="$(meta_sig "${PLIST[@]}")"
rc=0; cli_run "a5-apply-nope" apply no-such-tool --profile "${PIDS[opencode]:-router}" --json || rc=$?
err_out="$(cli_out)"
meta_all_after="$(meta_sig "${PLIST[@]}")"
if [[ ( "$rc" == "2" || "$rc" == "1" ) && "$meta_all_before" == "$meta_all_after" ]]; then
  pass_ev "TC-UC024-A5 目标不存在：apply 未知工具 id ⇒ 退出码 ${rc}（用法/业务错误），全部受管路径盘上零变化"
else
  fail_ev "TC-UC024-A5 目标不存在（退出码 2/1 + 盘上零变化）" \
    "退出码 2 或 1，且全部受管路径 file_meta 零变化" \
    "退出码 ${rc}，meta 变化=$([[ "$meta_all_before" == "$meta_all_after" ]] && echo no || echo yes)，out=${err_out}" \
    "$EV/${CLI_LAST}.out"
fi

printf 'report  cli-rewrite-router: families=%s router-base=%s first-red-detail=%s/first-fail.txt\n' \
  "${FAMILIES}" "$ROUTER_BASE" "$EV"
verify_finish

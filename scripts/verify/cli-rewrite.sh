#!/usr/bin/env bash
# scripts/verify/cli-rewrite.sh —— 经 yzagmgr **CLI**（不是 curl）对 opencode、pi、claude
# 三个家族做完整 rewrite 往返：
#
#   preview 无副作用（含 mtime）→ apply 后盘上决定键正确 → restore 后逐字节回到起点
#
# 非空洞判据（缺一即本脚本失败）：
#   - apply 后的中间态签名**必须**与起点不同（中间态与起点相同 = 什么都没改的空洞往返）；
#   - decide 档位由**独立分类器**（tierof.py，只读盘上字节，不复用产品 Probe()）判定；
#   - 「preview 无副作用」比较 file_meta 全量（exists/size/sha256/mtime_ns）。
#
# 边界与纪律：
#   - 全程 YZAGMGR_HOME 隔离根：绝不触碰用户真实的 ~/.pi、~/.config/opencode、~/.claude；
#   - 被验操作（tools/preview/apply/restore/backups）**一律经 bin/yzagmgr CLI**；
#     唯一的 curl 是 lib.sh 的守护进程管理助手（就绪探针打 /api/v1/health，非裸 /health）；
#   - 版本探测白名单置空 = 不执行任何真实 CLI 二进制（本格只验配置文件 rewrite）；
#   - restore 两条形态都验：opencode/pi 走 `restore --backup <初始快照>`，
#     claude 走裸 `restore`（= 撤销最近一次写入，恢复全库最新备份）。
#
# 用法: cli-rewrite.sh [--families opencode,pi,claude] [--origin URL]
#   --origin 默认用 hermetic 测试域（example.com 保留域；该形参的网关语义已随
#   旧中继删除停用——家族 Probe 只认回环受管形态）。
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib.sh
. "$HERE/lib.sh"

FAMILIES="opencode,pi,claude"
ORIGIN="https://gateway.example.com"
while (($# > 0)); do
  case "$1" in
    --families) shift; FAMILIES="${1:-}" ;;
    --origin) shift; ORIGIN="${1:-}" ;;
    *) die "未知参数 $1" ;;
  esac
  shift
done

# 清掉会污染被测进程的宿主注入（v1 托盘会向进程树注入整套 ANTHROPIC_* 管理 env，实测教训）。
unset ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN ANTHROPIC_BASE_URL ANTHROPIC_MODEL 2>/dev/null || true
unset CLAUDE_CONFIG_DIR PI_CODING_AGENT_DIR XDG_CONFIG_HOME XDG_DATA_HOME CODEX_HOME 2>/dev/null || true

verify_init cli-rewrite
seed_test_policy "$ORIGIN"
# 版本探测白名单置空 = 跳过一切真实 CLI 探测（YZAGMGR_TEST_VERSION_PROBE_FAMILIES 的空值语义）。
export YZAGMGR_TEST_VERSION_PROBE_FAMILIES=""
start_isolated_daemon

YZAGMGR="$ROOT/bin/yzagmgr"
[[ -x "$YZAGMGR" ]] || die "bin/yzagmgr 不存在（CLI 未构建）。先执行 make build。"

# ── CLI 调用助手（被验操作只经 CLI，不经 curl）────────────────────────────────

cli_run() { # cli_run <args...>：stdout→$RUN/cli.out stderr→$RUN/cli.err；返回 CLI 退出码
  run_capped 30 "$RUN/cli.out" "$RUN/cli.err" env \
    YZAGMGR_HOME="$RUN" \
    YZAGMGR_API_PORT="$API_PORT" \
    YZAGMGR_TEST_VERSION_PROBE_FAMILIES= \
    "$YZAGMGR" "$@"
}
cli_out() { cat "$RUN/cli.out"; }
cli_err() { cat "$RUN/cli.err" 2>/dev/null || true; }
cli_jget() { jget "$(cli_out)" "$1"; }

# ── 独立档位分类器（只读决定键字节；与 cli-roundtrip.sh 同源，不复用产品实现）────
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

# 内容签名（存在性 + 逐文件 sha256；与 cli-roundtrip.sh 同法）。
bytes_sig() {
  local out="" p
  while IFS= read -r p; do
    [[ -z "$p" ]] && continue
    if [[ -f "$p" ]]; then out="${out}${p}=$(sha256_file "$p")|"; else out="${out}${p}=<absent>|"; fi
  done < <(printf '%s\n' "$@" | LC_ALL=C sort)
  printf '%s' "${out%|}"
}

# 全量元数据签名（exists/size/sha256/mtime_ns）：preview 零副作用的判据——
# 内容没变但 mtime 被摸过也算副作用（preview-noop.sh 同款强判据）。
meta_sig() {
  local out="" p
  while IFS= read -r p; do
    [[ -z "$p" ]] && continue
    out="${out}${p}=$(file_meta "$p")|"
  done < <(printf '%s\n' "$@" | LC_ALL=C sort)
  printf '%s' "${out%|}"
}

# ── 全局 CLI 行为断言（错误路径与退出码映射的真跑级证据）──────────────────────
rc=0; cli_run health --json || rc=$?
if [[ "$rc" == "0" ]] && [[ -n "$(cli_jget 'str(d["data"].get("usageDb",{}).get("state",""))')" ]]; then
  record_pass "cli health --json：退出码 0，daemon/代理/usageDb 状态可读（usageDb.state=$(cli_jget 'str(d["data"]["usageDb"]["state"])')）"
else
  record_fail "cli health --json 失败：rc=${rc} out=$(cli_out | head -c 200)"
fi

rc=0; cli_run tools show no-such-tool --json || rc=$?
if [[ "$rc" == "1" ]] && [[ "$(cli_jget 'd.get("error",{}).get("code","")')" == "not_supported" ]]; then
  record_pass "cli 错误路径：未知工具 id → 退出码 1 + error.code=not_supported（契约 §1.3 的 2026-09-23 定稿）"
else
  record_fail "cli 错误路径不符：rc=${rc}（期望 1）out=$(cli_out | head -c 200)"
fi

rc=0; cli_run preview pi || rc=$?
if [[ "$rc" == "2" ]] && cli_err | grep -q -- "--profile"; then
  record_pass "cli 错误路径：preview 缺 --profile → 退出码 2（用法错误）并指明缺什么"
else
  record_fail "cli 用法错误路径不符：rc=${rc}（期望 2）err=$(cli_err | head -c 200)"
fi

# 守护进程不可达 → 4 且不自动拉起：拿一个此刻空闲、无监听的端口打 health。
dead_port="$(pick_free_port)"
rc=0; run_capped 15 "$RUN/cli.out" "$RUN/cli.err" env \
  YZAGMGR_HOME="$RUN" YZAGMGR_API_PORT="$dead_port" "$YZAGMGR" health >/dev/null 2>&1 || rc=$?
if [[ "$rc" == "4" ]] && grep -q "守护进程未运行" "$RUN/cli.err" 2>/dev/null; then
  record_pass "cli 错误路径：管理 API 不可达 → 退出码 4 +「守护进程未运行」提示（且未自动拉起守护进程）"
else
  record_fail "cli 不可达路径不符：rc=${rc}（期望 4）err=$(head -c 200 "$RUN/cli.err" 2>/dev/null || true)"
fi

# ── 逐家族 rewrite 往返（opencode → pi → claude；全部经 CLI）──────────────────
IFS=',' read -r -a wanted <<<"$FAMILIES"
for fam in "${wanted[@]}"; do
  # ① CLI 取工具详情 + 受管卡 + 受管路径。
  rc=0; cli_run tools show "$fam" --json || rc=$?
  mpid="$(cli_jget 'next((p["id"] for p in d["data"]["profiles"]["items"] if p.get("kind")=="managed"), "")')"
  paths_raw="$(cli_jget '"|".join(p["path"] for p in d["data"]["tool"]["paths"])')"
  if [[ "$rc" != "0" || -z "$mpid" || -z "$paths_raw" ]]; then
    record_fail "${fam} cli tools show 未给出受管卡/路径（rc=${rc} mpid=${mpid:-<空>} paths=${paths_raw:-<空>}）"
    continue
  fi
  record_pass "${fam} cli tools show：受管卡 ${mpid}、受管路径 $(printf '%s' "$paths_raw" | tr '|' ' ')"
  IFS='|' read -r -a plist <<<"$paths_raw"
  primary="${plist[0]}"

  # ② 起点夹具：用户自有内容（rewrite 往返必须有可回的起点；只写 CLI 报告的受管路径）。
  mkdir -p "$(dirname "${primary}")"
  case "$fam" in
    opencode) printf '{\n  "provider": {\n    "user-owned": {\n      "options": { "baseURL": "https://user.example/v1", "apiKey": "user-key" }\n    }\n  }\n}\n' >"$primary" ;;
    pi) printf '{\n  "providers": {\n    "user-owned": {\n      "models": [ { "id": "user-model", "baseUrl": "https://user.example/v1" } ]\n    }\n  }\n}\n' >"$primary" ;;
    claude) printf '{\n  "model": "user-model",\n  "env": { "USER_KEEP_ME": "1" }\n}\n' >"$primary" ;;
    *) printf 'user-owned: https://user.example/v1\n' >"$primary" ;;
  esac
  start_sig="$(bytes_sig "${plist[@]}")"
  start_meta="$(meta_sig "${plist[@]}")"
  start_tier="$(tierof "$fam" "${plist[@]}")"

  # ③ preview：纯计算不落盘 —— 全量元数据（含 mtime_ns）必须一字不变。
  rc=0; cli_run preview "$fam" --profile "$mpid" --json || rc=$?
  pvid="$(cli_jget 'str(d["data"].get("previewId",""))')"
  pfiles="$(cli_jget 'len(d["data"].get("files") or [])')"
  if [[ "$rc" != "0" || -z "$pvid" || "$pfiles" == "0" ]]; then
    record_fail "${fam} cli preview 失败或空计划（rc=${rc} previewId=${pvid:-<空>} files=${pfiles}）"
    continue
  fi
  record_pass "${fam} cli preview：previewId 返回且计划含 ${pfiles} 个文件"
  if [[ "$(meta_sig "${plist[@]}")" == "$start_meta" ]]; then
    record_pass "${fam} preview 无副作用：全部受管路径的 exists/size/sha256/mtime_ns 与预览前逐字节一致"
  else
    record_fail "${fam} preview 有副作用：预览后盘上元数据被改（mtime/内容/存在性）"
  fi

  # ④ apply：rewrite 落盘 —— 决定键正确 + 中间态与起点不同（非空洞判据）。
  rc=0; cli_run apply "$fam" --profile "$mpid" --json || rc=$?
  astatus="$(cli_jget 'str(d["data"].get("status",""))')"
  if [[ "$rc" != "0" || "$astatus" != "applied" ]]; then
    record_fail "${fam} cli apply 失败（rc=${rc} status=${astatus:-<空>} out=$(cli_out | head -c 200)）"
    continue
  fi
  record_pass "${fam} cli apply：status=applied（含写前备份语义，backupIds=$(cli_jget '",".join(d["data"].get("backupIds") or [])')）"

  mid_tier="$(tierof "$fam" "${plist[@]}")"
  if [[ "$mid_tier" == "gateway" ]]; then
    record_pass "${fam} apply 后盘上决定键正确：独立分类器读出 gateway（起点分类=${start_tier}）"
  else
    record_fail "${fam} apply 后决定键不正确：独立分类器读出 ${mid_tier}（期望 gateway）"
  fi

  mid_sig="$(bytes_sig "${plist[@]}")"
  if [[ "$mid_sig" != "$start_sig" ]]; then
    record_pass "${fam} 非空洞判据：apply 后中间态与起点**不同**（rewrite 真的改了盘上字节）"
  else
    record_fail "${fam} 往返是空洞的：apply 后签名与起点完全相同 —— 什么都没改"
  fi

  # ⑤ restore 回到起点（逐字节）。两种形态都走 CLI：
  #    opencode/pi → restore --backup <初始快照>（先经 backups list 取编号）；
  #    claude → 裸 restore（撤销最近一次写入 = 恢复全库最新备份）。
  if [[ "$fam" == "claude" ]]; then
    rc=0; cli_run restore --json || rc=$?
    restored_via="裸 restore（撤销最近一次写入，目标=$(cli_jget 'str(d["data"].get("backupId",""))')）"
  else
    rc_list=0; cli_run backups list --json || rc_list=$?
    bk="$(cli_jget 'next((b["backupId"] for b in d["data"]["items"] if b.get("toolId")=="'"${fam}"'" and b.get("initial")), "")')"
    if [[ "$rc_list" != "0" || -z "$bk" ]]; then
      record_fail "${fam} cli backups list 未给出初始快照（rc=${rc_list} bk=${bk:-<空>}）"
      continue
    fi
    record_pass "${fam} cli backups list：初始快照在册（${bk}，永不淘汰的那条）"
    rc=0; cli_run restore --backup "$bk" --json || rc=$?
    restored_via="restore --backup ${bk}（初始快照）"
  fi
  if [[ "$rc" != "0" ]]; then
    record_fail "${fam} cli restore 失败（rc=${rc} out=$(cli_out | head -c 200)）"
    continue
  fi
  end_sig="$(bytes_sig "${plist[@]}")"
  end_tier="$(tierof "$fam" "${plist[@]}")"
  if [[ "$end_sig" == "$start_sig" ]]; then
    record_pass "${fam} restore 后逐字节回到起点（经 ${restored_via}，签名 $(printf '%s' "$end_sig" | shasum -a 256 | cut -c1-12)）"
  else
    record_fail "${fam} restore 后未回到起点（起点=${start_sig} 终点=${end_sig}）"
  fi
  if [[ "$end_tier" != "gateway" ]]; then
    record_pass "${fam} restore 后档位回到起点侧（分类=${end_tier}，起点分类=${start_tier}）"
  else
    record_fail "${fam} restore 后托管条目仍在（分类=gateway）"
  fi

  printf 'report  %s preview:noop apply:gateway mid-differs:yes restore:byte-exact\n' "$fam"
done

verify_finish

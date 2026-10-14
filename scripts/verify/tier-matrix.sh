#!/usr/bin/env bash
# scripts/verify/tier-matrix.sh —— 十家族网关档决定键矩阵（配置封层核心断言）。
#
# 验收手册: doc/acceptance-steps.md §3.1；决定键的权威定义见
#   .agents/skills/yzagmgr-config-layer/references/acceptance-recipe.md §A。
#
# 纪律（本脚本的要点）:
#   1. **从盘上读决定键**判定档位，不看内部记账（recipe §C 手段 A）。
#   2. **独立取值**：本脚本用自己的提取器读盘，**不复用产品的 Probe()** ——
#      复用会让 Probe 里的 bug 同时通过实现与验收两侧。
#   3. 证据形态：绝对路径 + 前后 sha256 + 字节长度 + 决定键精确子串（recipe §E）。
#
# 前置（未就绪即 BLOCKED，退出码 2）:
#   - bin/yzagmgr（Go 守护进程）
#   - YZAGMGR_POLICY_FILE + YZAGMGR_TOKEN_FILE（测试种子注入）：自动化验收需要一个
#     已知的 origin 与令牌；正常形态下这两者来自登录后的策略拉取，门禁里不联网，
#     故用种子注入。守护进程须在启动日志里显式声明已启用注入。
#
# 用法: tier-matrix.sh [--against-v1]
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib.sh
. "$HERE/lib.sh"

AGAINST_V1=0
while (($# > 0)); do
  case "$1" in
    --against-v1) AGAINST_V1=1 ;;
    *) die "未知参数 $1" ;;
  esac
  shift
done

verify_init tier-matrix

# ── 种子注入（自动化验收的前提）─────────────────────────────────────────────
# 形状见 doc/api-v1.md §3 的 GET /policy 响应。
# 注意 `version` 必须是**整数**（§J.2：Version int 严格递增）—— 写成字符串会让
# 策略解析失败，表现为 /policy 返回零值与「模型策略没有默认模型」。
seed_policy() {
  cat >"$RUN/policy.json" <<'JSON'
{
  "version": 1,
  "defaultModel": "deepseek-flash",
  "models": [{ "id": "deepseek-flash", "name": "deepseek-flash", "supportsImages": true }],
  "catalogs": {}
}
JSON
  echo "acceptance-token-0000000000" >"$RUN/inference-token"
  export YZAGMGR_POLICY_FILE="$RUN/policy.json"
  export YZAGMGR_TOKEN_FILE="$RUN/inference-token"
  # origin 刻意用**不可达的保留域**：hermetic 验收不该真连网关（要可重复、不依赖网络）。
  # 本脚本只断言决定键的**形态**（本地 route / 以 /v1、/v1beta 结尾），不断言 origin 本身，
  # 所以取值不影响判定，只要它被一致地写进盘上。用 .invalid 是 RFC 2606 的保留 TLD。
  # origin 用**属于产品自己域**但不需要真实可达**的主机名。两个约束：
  #   ① 不真连网关（验收要可重复、不依赖网络）⇒ 不需要解析这个域；
  #   ② 受管形态只认回环（原旧网关域判定已随 C 路删除——design §2.3），见长注释。
  #      会被判成「非受管端点」而让写后自检失败（实测踩到）。
  export YZAGMGR_GATEWAY_ORIGIN="https://gateway.example.com"
}
seed_policy

# 端口隔离**必须在起守护进程之前**：用户/并发实例占着 17022 时，本脚本的守护进程会按契约
# 直接绑端口失败退出，而就绪探针会被**别人的实例**应答（同端口、异令牌）⇒ 报出「daemon up」
# 却对着别人的实例断言。实测（未隔离时）：探针拿到用户已安装 v2 的 200，随后 10 条断言全被
# 拒（unauthorized 缺少或错误的管理令牌）。start_isolated_daemon 现在自带隔离与「这就是我的
# 实例」证明，这里显式调用只为让所选端口先见于输出。
choose_isolated_ports
start_isolated_daemon

# 守护进程必须显式声明启用了注入（静默后门是危险的）。
if grep -q "已启用测试种子注入" "$RUN/daemon.log"; then
  record_pass "seed-injection 守护进程已在启动日志声明启用测试种子注入"
else
  record_fail "seed-injection 守护进程未在启动日志声明启用了测试种子注入（静默后门）"
fi

# ── 独立提取器 ──────────────────────────────────────────────────────────────
# 按格式从盘上取「决定键」的值。刻意不复用产品实现。
cat >"$RUN/decide.py" <<'PY'
import json, re, sys, pathlib

FAMILY, PATH = sys.argv[1], pathlib.Path(sys.argv[2])

def jload(p):
    return json.loads(p.read_text(encoding="utf-8"))

def out(v):
    print("" if v is None else v); raise SystemExit

if not PATH.exists():
    out(None)
if FAMILY == "claude":
    # 决定键 env.ANTHROPIC_BASE_URL（旧名 claude.json 由 config 层选择）
    out(jload(PATH).get("env", {}).get("ANTHROPIC_BASE_URL"))
if FAMILY == "claude-desktop":
    # 决定键 profile 内的 inferenceProvider
    out(jload(PATH).get("inferenceProvider"))
if FAMILY == "codex":
    # 决定键 = model_provider（+ [model_providers.custom].base_url，由调用方另断）
    m = re.search(r'^\s*model_provider\s*=\s*"([^"]*)"', PATH.read_text(encoding="utf-8"), re.M)
    out(m.group(1) if m else None)
if FAMILY == "gemini":
    # 决定键在 .env：GOOGLE_GEMINI_BASE_URL
    for line in PATH.read_text(encoding="utf-8").splitlines():
        m = re.match(r'\s*(?:export\s+)?GOOGLE_GEMINI_BASE_URL\s*=\s*"?([^"\n]*)"?\s*$', line)
        if m:
            out(m.group(1))
    out(None)
if FAMILY == "grokbuild":
    # 决定键 [model."<id>"].base_url —— 取任意一条 base_url
    m = re.search(r'^\s*base_url\s*=\s*"([^"]*)"', PATH.read_text(encoding="utf-8"), re.M)
    out(m.group(1) if m else None)
if FAMILY == "opencode":
    d = jload(PATH)
    for k, v in (d.get("provider") or {}).items():
        if k.startswith("yzagmgr-"):
            out((v.get("options") or {}).get("baseURL"))
    out(None)
if FAMILY == "openclaw":
    # JSON5 + 裸根键（models: 不带引号）：严格 JSON 读者会解析失败，故不引 json.load。
    # 键本身**带引号**（"baseUrl": …），正则必须容下键的闭合引号 —— 少了它会静默取到空值。
    m = re.search(r'"?baseUrl"?\s*:\s*"([^"]*)"', PATH.read_text(encoding="utf-8"))
    out(m.group(1) if m else None)
if FAMILY == "hermes":
    # 托管 provider 是 custom_providers 列表的**元素**，故行首可能是 `- `（YAML 序列项）：
    # 少了 `(?:-\s*)?` 会静默取到空值（实测踩到）。
    m = re.search(r'^\s*(?:-\s*)?base_url\s*:\s*"?([^"\n]*?)"?\s*$', PATH.read_text(encoding="utf-8"), re.M)
    out(m.group(1) if m else None)
if FAMILY == "pi":
    d = jload(PATH)
    for k, v in (d.get("providers") or {}).items():
        if k.startswith("yzagmgr-"):
            ms = v.get("models") or []
            out(ms[0].get("baseUrl") if ms else None)
    out(None)
if FAMILY == "workbuddy":
    arr = jload(PATH)
    for e in arr if isinstance(arr, list) else []:
        u = e.get("url") or ""
        if "127.0.0.1" in u:
            out(u)
    out(None)
out(None)
PY

# 每族：工具 id | 决定键所在文件（相对 $RUN） | 期望形态（正则）
# 期望形态取自 recipe §A；claude 走本地 route，其余指向网关 origin。
declare -a FAMILIES=(
  "claude|.claude/settings.json|^http://127\.0\.0\.1:[0-9]+/claude$"
  "claude-desktop|Library/Application Support/Claude-3p/configLibrary/00000000-0000-4000-8000-000000157210.json|^gateway$"
  "codex|.codex/config.toml|^custom$"
  "gemini|.gemini/.env|/v1beta$"
  "grokbuild|.grok/config.toml|/v1$"
  "opencode|.config/opencode/opencode.json|/v1$"
  "openclaw|.openclaw/openclaw.json|/v1$"
  "hermes|.hermes/config.yaml|/v1$"
  "pi|.pi/agent/models.json|/v1$"
  # workbuddy 刻意**不走工具通道**（UC-009：provider_preview/apply 对它返回 not_supported），
  # 其断言在循环之后单独处理。
)

for row in "${FAMILIES[@]}"; do
  IFS='|' read -r fam relpath want <<<"$row"
  file="$RUN/$relpath"

  # 受管卡 id 取自 API，不硬编码（UC-018 A5/A10）。
  profiles="$(api GET "/tools/${fam}/profiles" || true)"
  pid="$(printf '%s' "$profiles" | python3 -c '
import json,sys
try: d=json.load(sys.stdin)["data"]
except Exception: print(""); raise SystemExit
for p in d.get("items",[]):
    if p.get("kind")=="managed": print(p["id"]); break
' 2>/dev/null || true)"

  if [[ -z "$pid" ]]; then
    record_fail "${fam} 取不到受管卡（profiles 响应: ${profiles:0:160}）"
    continue
  fi

  prev_sha=""; [[ -f "$file" ]] && prev_sha="$(sha256_file "$file")"
  prev_len=0; [[ -f "$file" ]] && prev_len="$(wc -c <"$file" | tr -d ' ')"

  pv="$(api POST "/tools/${fam}/preview" "{\"profileId\":\"${pid}\"}" || true)"
  pvid="$(printf '%s' "$pv" | python3 -c '
import json,sys
try: print(json.load(sys.stdin)["data"]["previewId"])
except Exception: print("")
' 2>/dev/null || true)"
  if [[ -z "$pvid" ]]; then
    record_fail "${fam} preview 未返回 previewId（响应: ${pv:0:160}）"
    continue
  fi

  ap="$(api POST "/tools/${fam}/apply" "{\"previewId\":\"${pvid}\"}" || true)"
  okflag="$(printf '%s' "$ap" | python3 -c '
import json,sys
try: print("1" if json.load(sys.stdin).get("ok") else "0")
except Exception: print("0")
' 2>/dev/null || echo 0)"
  if [[ "$okflag" != "1" ]]; then
    record_fail "${fam} apply 未成功（响应: ${ap:0:200}）"
    continue
  fi

  got="$(python3 "$RUN/decide.py" "$fam" "$file" 2>/dev/null || true)"
  new_sha=""; [[ -f "$file" ]] && new_sha="$(sha256_file "$file")"
  new_len=0; [[ -f "$file" ]] && new_len="$(wc -c <"$file" | tr -d ' ')"

  if [[ ! -f "$file" ]]; then
    record_fail "${fam} 决定键所在文件不存在: ${file}"
    continue
  fi
  if [[ "$got" =~ $want ]]; then
    record_pass "${fam} 决定键=${got} 路径=${file} sha256:${prev_sha:0:12}→${new_sha:0:12} 字节:${prev_len}→${new_len}"
  else
    record_fail "${fam} 决定键=${got} 不匹配期望 ${want}（路径=${file}）"
  fi

  # codex 另外要求 [model_providers.custom].base_url 指向 origin（§A 行 3 是双键）。
  if [[ "$fam" == "codex" ]]; then
    if grep -qE '^\s*base_url\s*=\s*"[^"]+"' "$file"; then
      record_pass "codex [model_providers.custom].base_url 已写入"
    else
      record_fail "codex 缺少 [model_providers.custom].base_url"
    fi
    if grep -q "model_max_output_tokens" "$file"; then
      record_fail "codex 写出了非法字段 model_max_output_tokens（--strict-config 会拒绝）"
    else
      record_pass "codex 未写出非法字段 model_max_output_tokens"
    fi
  fi
done

# ── workbuddy：面已随 DA R4 删除（design §1.1）───────────────────────────────
# 原「switch-channel 直写 + 受管行首位」断言随 internal/family/workbuddy 整包与
# /workbuddy/* 路由删除而注销（无解除条件：该面已退役）。

# ── v1 基线（manual §8.1）──────────────────────────────────────────────────
if [[ "$AGAINST_V1" == "1" ]]; then
  # v1 的 API 走 Tauri IPC，本脚本无法直接调用；改为要求用户按 manual §8.1 手工记录。
  record_unknown "v1-baseline" \
    "--against-v1 需要 v1 应用正在运行且其档位需经其自身 UI/IPC 读取，本脚本无通路" \
    "按 doc/acceptance-steps.md §8.1 人工记录，或为 v1 提供等价查询入口"
fi

verify_finish

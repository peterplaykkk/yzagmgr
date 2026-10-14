#!/usr/bin/env bash
# scripts/verify/tier-roundtrip.sh —— 官方档 ↔ 网关档往返（终点字节回到起点）。
#
# 验收手册: doc/acceptance-steps.md §3.3；判定依据 recipe §G 断言组⑤。
#
# 前置（未就绪即 BLOCKED，退出码 2）：
#   - bin/yzagmgr；种子策略 + 令牌（理由同 tier-matrix.sh：门禁不联网，origin 用
#     example.com 保留域——形参语义已随旧中继删除停用）。
#
# 判读（两类家族的往返语义不同，不得互套）：
#   - **有官方种子的五家**（claude / claude-desktop / codex / gemini / grokbuild）：
#     官方档与网关档都是本产品能写出的**确定形态**，故往返是严格的
#     「官方 → 网关 → 官方 …」N 轮，终点 sha256 == 起点 sha256，且每种的形态在
#     各轮之间逐字节稳定（无累计漂移）。
#   - **additive 四家**（opencode / openclaw / hermes / pi）：**没有官方卡**
#     （/tools/{id}/profiles 里就没有 kind=official 项，这是数据面事实，不靠断言猜），
#     故其「回到官方档」的等价物 = **恢复初始快照**（恢复会移除我们加的托管条目）。
#     起点取「用户自有内容」的夹具：往返断言 = 恢复后逐字节回到该夹具，
#     且网关档下用户自有条目必须存活（不是覆盖式接管）。
#
# 从盘上读档位（**不复用产品 Probe()**）：本脚本自带 tierof.py 独立分类器，
# 只读决定键，与 API 报告的 tier 交叉核对（两处不一致本身就是一条断言）。
#
# 用法: tier-roundtrip.sh [--rounds N]（默认 3）
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib.sh
. "$HERE/lib.sh"

ROUNDS=3
while (($# > 0)); do
  case "$1" in
    --rounds) shift; ROUNDS="${1:-3}" ;;
    *) die "未知参数 $1" ;;
  esac
  shift
done

verify_init tier-roundtrip
seed_test_policy
choose_isolated_ports
start_isolated_daemon
require_own_daemon

SEEDED="claude claude-desktop codex gemini grokbuild"
ADDITIVE="opencode openclaw hermes pi"

# ── 独立档位分类器（只读决定键；刻意不复用产品实现）────────────────────────
cat >"$RUN/tierof.py" <<'PY'
import json, re, sys, pathlib

FAMILY = sys.argv[1]
PATHS = [p for p in sys.argv[2:] if p]

def pick(*needles):
    """在受管路径里挑决定键所在的文件：最后一个命中片段的路径优先（最具体者胜）。"""
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

def jload(text):
    try:
        return json.loads(text)
    except Exception:
        return None

def classify():
    if FAMILY == "claude":
        p = pick(".claude/settings.json", ".claude/claude.json")
        t = read(p)
        if t is None:
            return "official"   # 键缺席 = 官方档（recipe §A 的官方档值形态）
        d = jload(t) or {}
        v = (d.get("env") or {}).get("ANTHROPIC_BASE_URL")
        if not v:
            return "official"
        return "gateway" if ("127.0.0.1" in v) else "unknown"
    if FAMILY == "claude-desktop":
        # 3P 档案是 configLibrary/<UUID>.json；同目录还有 _meta.json（不是档案，
        # 里面没有 inferenceProvider）——「取最后一个匹配」会命中 _meta.json 而误判成
        # official（实测踩到）。故显式排除它。
        cand = [x for x in PATHS if "configLibrary/" in x and not x.endswith("_meta.json")]
        p = cand[-1] if cand else pick("configLibrary/")
        t = read(p)
        if t is None:
            return "official"
        d = jload(t) or {}
        return "gateway" if d.get("inferenceProvider") == "gateway" else "official"
    if FAMILY == "codex":
        t = read(pick(".codex/config.toml"))
        if t is None:
            return "official"
        m = re.search(r'^\s*model_provider\s*=\s*"([^"]*)"', t, re.M)
        if m and m.group(1) == "custom":
            return "gateway"
        return "official"
    if FAMILY == "gemini":
        t = read(pick(".gemini/.env"))
        if t is None:
            return "official"
        for line in t.splitlines():
            if re.match(r'\s*(?:export\s+)?GOOGLE_GEMINI_BASE_URL\s*=\s*"?\S', line):
                return "gateway"
        return "official"
    if FAMILY == "grokbuild":
        t = read(pick(".grok/config.toml"))
        if t is None:
            return "official"
        return "gateway" if re.search(r'^\s*base_url\s*=\s*"', t, re.M) else "official"
    if FAMILY in ("opencode", "openclaw", "hermes", "pi"):
        # additive：判据 = 托管条目在不在（托管 provider id 前缀 yzagmgr-）。
        needle = {"opencode": "opencode.json", "openclaw": "openclaw.json",
                  "hermes": "config.yaml", "pi": "models.json"}[FAMILY]
        p = pick(needle)
        t = read(p)
        if t is None:
            return "absent"
        return "gateway" if re.search(r'yzagmgr-[A-Za-z0-9_]', t) else "official"
    return "unknown"

print(classify())
PY

# tierof <fam> <path...> —— 从盘上独立分类。
tierof() {
  local fam="$1"; shift
  python3 "$RUN/tierof.py" "$fam" "$@"
}

# card_ids <fam> —— 打印 "managed_id official_id"（官方卡缺席时第二项为空）。
card_ids() {
  api GET "/tools/$1/profiles" | jget - '" ".join([next((p["id"] for p in d["data"]["items"] if p.get("kind")=="managed"), ""),
   next((p["id"] for p in d["data"]["items"] if p.get("kind")=="official"), "")])'
}

# fam_paths <fam> —— 该族受管文件绝对路径（取自 API，不硬编码）。
# 以 `|` 连接而不是空格：macOS 的 Claude Desktop 路径里带空格，空格分隔会切坏路径。
fam_paths() { api GET "/tools/$1" | jget - '"|".join(p["path"] for p in d["data"]["paths"])'; }

# bytes_sig <path...> —— 逐文件 sha256 拼接（顺序按路径名排序，保证可比较）。
# 注意：必须 `while IFS= read -r` 逐行读，不能 `for p in $(...)` —— 后者按空白切词，
# 而 Claude Desktop 的路径里带空格（实测踩到：签名里出现 "Application" 这种碎片）。
bytes_sig() {
  local out="" p
  while IFS= read -r p; do
    [[ -z "$p" ]] && continue
    if [[ -f "$p" ]]; then out="${out}${p}=$(sha256_file "$p")|"; else out="${out}${p}=<absent>|"; fi
  done < <(printf '%s\n' "$@" | LC_ALL=C sort)
  printf '%s' "${out%|}"
}

# apply_card <fam> <profileId> —— 走真实的 preview → apply 写链。
apply_card() {
  local fam="$1" pid="$2" pv pvid ap
  pv="$(api POST "/tools/${fam}/preview" "{\"profileId\":\"${pid}\"}" || true)"
  pvid="$(jget "$pv" 'd["data"]["previewId"]')"
  if [[ -z "$pvid" ]]; then
    record_fail "${fam} 预览档位卡 ${pid} 失败：${pv:0:200}"
    return 1
  fi
  ap="$(api POST "/tools/${fam}/apply" "{\"previewId\":\"${pvid}\"}" || true)"
  if ! printf '%s' "$ap" | grep -q '"ok":true'; then
    record_fail "${fam} 应用档位卡 ${pid} 失败：${ap:0:240}"
    return 1
  fi
  return 0
}

# restore_card <fam> <backupId> —— 走真实的 restore/preview → restore/apply 写链。
restore_backup() {
  local fam="$1" bk="$2" pv pvid ap
  pv="$(api POST "/tools/${fam}/restore/preview" "{\"backupId\":\"${bk}\"}" || true)"
  pvid="$(jget "$pv" 'd["data"]["previewId"]')"
  if [[ -z "$pvid" ]]; then
    record_fail "${fam} 恢复预览失败：${pv:0:200}"
    return 1
  fi
  ap="$(api POST "/tools/${fam}/restore/apply" "{\"previewId\":\"${pvid}\"}" || true)"
  if ! printf '%s' "$ap" | grep -q '"ok":true'; then
    record_fail "${fam} 恢复应用失败：${ap:0:240}"
    return 1
  fi
  return 0
}

# ── 有官方种子的五家 ────────────────────────────────────────────────────────
for fam in $SEEDED; do
  read -r mpid opid <<<"$(card_ids "$fam")"
  if [[ -z "$mpid" || -z "$opid" ]]; then
    record_fail "${fam} 档位卡缺失（managed=${mpid:-<无>} official=${opid:-<无>}）：有官方种子的家族必须同时有受管卡与官方卡"
    continue
  fi
  paths="$(fam_paths "$fam")"
  IFS='|' read -r -a plist <<<"$paths"

  # 归一化：先各走一遍两种档位，把「官方档」落到它的**稳定形态**，再取起点。
  # 为什么必须归一化：grokbuild 的官方档就是「空 config」（recipe §A 行 5 的官方种子
  # settings_config 为空）——从「文件不存在」出发应用官方档时，目标字节与写前字节同为
  # 空 ⇒ tx 记 unchanged 不落盘，于是「不存在」与「0 字节文件」成了官方档的两种形态。
  # 不归一化就会把形态差异误读成往返破坏（实测踩到；经口径 owner 裁定空 config 即官方档）。
  if ! apply_card "$fam" "$mpid" || ! apply_card "$fam" "$opid"; then continue; fi
  start_sig="$(bytes_sig "${plist[@]}")"
  start_tier="$(tierof "$fam" "${plist[@]}")"
  if [[ "$start_tier" == "official" ]]; then
    record_pass "${fam} 起点落官方档 决定键分类=official 起点 sha256=$(printf '%s' "$start_sig" | shasum -a 256 | cut -c1-12)"
  else
    record_fail "${fam} 起点未落官方档（盘上分类=${start_tier}）签名=${start_sig}"
  fi

  gateway_sigs=(); official_sigs=()
  for ((r = 1; r <= ROUNDS; r++)); do
    if ! apply_card "$fam" "$mpid"; then break; fi
    gt="$(tierof "$fam" "${plist[@]}")"
    gateway_sigs+=("$(bytes_sig "${plist[@]}")")
    api_tier="$(api GET "/tools/$fam" | jget - 'd["data"]["tier"]')"
    if [[ "$gt" == "gateway" && "$api_tier" == "gateway" ]]; then
      record_pass "${fam} 第${r}轮 网关档 盘上分类=gateway API tier=gateway sha256=$(sha256_file "${plist[0]}" 2>/dev/null | cut -c1-12)"
    else
      record_fail "${fam} 第${r}轮 未落网关档（盘上=${gt} API tier=${api_tier}）签名=$(bytes_sig "${plist[@]}")"
    fi

    if ! apply_card "$fam" "$opid"; then break; fi
    ot="$(tierof "$fam" "${plist[@]}")"
    official_sigs+=("$(bytes_sig "${plist[@]}")")
    if [[ "$ot" != "official" ]]; then
      record_fail "${fam} 第${r}轮 未回官方档（盘上分类=${ot}）签名=$(bytes_sig "${plist[@]}")"
    fi
  done

  # 各轮之间逐字节稳定（网关档之间、官方档之间都不许累计漂移）。
  gw_uniq="$(printf '%s\n' "${gateway_sigs[@]}" | sort -u | wc -l | tr -d ' ')"
  off_uniq="$(printf '%s\n' "${official_sigs[@]}" | sort -u | wc -l | tr -d ' ')"
  if [[ "$gw_uniq" == "1" ]]; then
    record_pass "${fam} ${ROUNDS} 轮网关档签名逐轮一致（无累计漂移）"
  else
    record_fail "${fam} 网关档签名在各轮不同（${gw_uniq} 种）：$(printf '%s\n' "${gateway_sigs[@]}")"
  fi

  if [[ "$off_uniq" == "1" ]]; then
    record_pass "${fam} ${ROUNDS} 轮官方档签名逐轮一致（无累计漂移）"
  else
    record_fail "${fam} 官方档签名在各轮不同（${off_uniq} 种）：$(printf '%s\n' "${official_sigs[@]}")"
  fi

  # 终点 == 起点：最后一轮已回官方档，直接与起点签名比对。
  if ((${#official_sigs[@]} == 0)); then
    continue  # 前面已逐轮记 fail，这里不再叠加无依据的断言
  fi
  end_sig="${official_sigs[$((${#official_sigs[@]} - 1))]}"
  if [[ "$end_sig" == "$start_sig" ]]; then
    record_pass "${fam} 往返终点 == 起点（字节精确，${ROUNDS} 轮；官方档签名稳定：${off_uniq} 种）路径=${plist[*]}"
  else
    record_fail "${fam} 往返终点 != 起点（注意 \`<absent>\` 与 e3b0c442…（空字节 sha256）= 两种不同落盘形态）：起点=${start_sig} 终点=${end_sig}"
  fi
done

# ── additive 四家（无官方卡：等价物 = 恢复初始快照）─────────────────────────
for fam in $ADDITIVE; do
  read -r mpid opid <<<"$(card_ids "$fam")"
  if [[ -z "$mpid" ]]; then
    record_fail "${fam} 无受管卡（additive 家族也必须有受管卡）"
    continue
  fi
  if [[ -n "$opid" ]]; then
    record_fail "${fam} 出现了官方卡（additive 家族按契约没有官方档形态）"
  else
    record_pass "${fam} 无官方卡（additive：/tools/${fam}/profiles 里没有 kind=official，回到官方档走恢复初始快照）"
  fi
  paths="$(fam_paths "$fam")"
  IFS='|' read -r -a plist <<<"$paths"
  primary="${plist[0]}"

  # 夹具：模拟真机「文件里已有用户自己的 provider」——往返必须从这份字节出发。
  mkdir -p "$(dirname "$primary")"
  case "$fam" in
    opencode)
      printf '{\n  "provider": {\n    "user-owned": {\n      "options": { "baseURL": "https://user.example/v1", "apiKey": "user-key-marker" }\n    }\n  }\n}\n' >"$primary" ;;
    openclaw)
      printf '{\n  models: {\n    providers: {\n      "user-owned": { baseUrl: "https://user.example/v1", apiKey: "user-key-marker" }\n    }\n  }\n}\n' >"$primary" ;;
    hermes)
      printf 'custom_providers:\n  - name: user-owned\n    base_url: https://user.example/v1\n    api_key: user-key-marker\n' >"$primary" ;;
    pi)
      printf '{\n  "providers": {\n    "user-owned": {\n      "models": [ { "id": "user-model", "baseUrl": "https://user.example/v1" } ]\n    }\n  }\n}\n' >"$primary" ;;
  esac
  fx_sig="$(bytes_sig "${plist[@]}")"

  ok_rounds=0
  for ((r = 1; r <= ROUNDS; r++)); do
    if ! apply_card "$fam" "$mpid"; then break; fi
    gt="$(tierof "$fam" "${plist[@]}")"
    if [[ "$gt" != "gateway" ]]; then
      record_fail "${fam} 第${r}轮 网关档托管条目未落盘（盘上分类=${gt}）"
      break
    fi
    # 用户自有条目必须存活（不是覆盖式接管）。
    # 存活判据用两个四家夹具都具备的字符串：`user-owned`（用户自己的 provider id）
    # 与用户自己的 base url。刻意不用「apiKey 标记」——四家夹具里只有三家带它，
    # 拿它当判据会把「夹具本身没有」误判成「被覆盖」（实测踩到）。
    if grep -qF "user-owned" "$primary" && grep -qF "https://user.example/v1" "$primary"; then
      keep=1
    else
      keep=0
    fi

    bk="$(api GET "/tools/${fam}/backups?limit=50" | jget - 'next((b["backupId"] for b in d["data"]["items"] if b.get("initial")), "")')"
    if [[ -z "$bk" ]]; then
      record_fail "${fam} 取不到初始快照（/tools/${fam}/backups 无 initial:true 项）"
      break
    fi
    if ! restore_backup "$fam" "$bk"; then break; fi
    back_sig="$(bytes_sig "${plist[@]}")"

    if [[ $keep == 1 && "$back_sig" == "$fx_sig" ]]; then
      ok_rounds=$((ok_rounds + 1))
      if (( r == 1 )); then
        record_pass "${fam} 第1轮 网关档保留用户自有条目且加托管条目；恢复初始快照后逐字节回到用户原样（sha256=$(sha256_file "$primary" | cut -c1-12) 路径=${primary}）"
      fi
    else
      if (( keep == 0 )); then
        record_fail "${fam} 第${r}轮 网关档覆盖了用户自有条目（夹具 marker 丢失）：$(head -c 240 "$primary" | tr '\n' ' ')"
      else
        record_fail "${fam} 第${r}轮 恢复未回到用户原样：期望=${fx_sig} 实际=${back_sig}"
      fi
      break
    fi
  done
  if (( ok_rounds == ROUNDS )); then
    record_pass "${fam} additive 往返 ${ROUNDS} 轮稳定（每轮：托管条目被加入 → 恢复后移除并逐字节回到用户原样）"
  elif (( ok_rounds > 0 )); then
    record_fail "${fam} additive 往返只稳定 ${ok_rounds}/${ROUNDS} 轮"
  fi
done

verify_finish

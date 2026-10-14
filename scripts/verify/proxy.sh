#!/usr/bin/env bash
# scripts/verify/proxy.sh —— 本地代理数据面（UC-007 / UC-017）。
#
# 契约: doc/api-v1.md §7（数据面契约）。
#
# 前置（未就绪即 BLOCKED，退出码 2）：
#   - bin/yzagmgr；种子策略 + 令牌；本机存在**非回环** IPv4（代理禁止把上游写成回环，UC-007 A13，
#     故 hermetic 上游桩填本机非回环地址 —— host_ip）。
#
# 上游桩（lib.sh 的 start_stub_upstream）：数据面要断言的四件事都要求上游**可控**：
#   逐字节透传 / 非 2xx 透传 / SSE 首帧时间与总长无关 / 每请求一条用量记录。
#   真实网关给不出可控响应，也不可重复 ⇒ 桩是唯一能做到「真跑 + 可重复」的摆法。
#
# 用法: proxy.sh
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib.sh
. "$HERE/lib.sh"

verify_init proxy

# 目录载荷刻意用**不规则空白 + 非字典序键 + 1e6 这类数字字面量**：
# 「逐字节等同」这条断言要能区分「原样搬运」与「解析后重编码」（后者会抹平空白、
# 重排键序、改写数字字面量）。同时 seed 里另存一份紧凑重编码，断言两者**不等**，
# 以证明本用例不是「无论怎么实现都过」的空断言。
CATALOG_RAW='{ "models" : [ {"zId":"first","n":1e6},   {"aId":"second","n":1.50} ]  }'
CATALOG_COMPACT='{"models":[{"zId":"first","n":1000000},{"aId":"second","n":1.5}]}'
printf '%s' "$CATALOG_RAW" >"$RUN/catalog.codex.expected"
printf '%s' "$CATALOG_COMPACT" >"$RUN/catalog.codex.compact"

seed_test_policy "https://gateway.example.com" "{\"codex\":${CATALOG_RAW}}"
choose_isolated_ports
start_isolated_daemon
require_own_daemon

UP_IP="$(host_ip || true)"
[[ -n "$UP_IP" ]] || die "本机没有非回环 IPv4：代理的上游守卫禁止回环地址（UC-007 A13），无法起 hermetic 上游桩"
# 桩口由 lib.sh 请内核分配（**不要**写成 proxy+100：代理口抽到 65487 时那个算术会越界，
# python 的 bind 抛 OverflowError，suite 会以一句没有信息的 BLOCKED 收场）。
start_stub_upstream
STUB_URL="http://${UP_IP}:${STUB_PORT}"

# 契约 §7 的九个前缀（集合语义，逐字取自 doc/api-v1.md §7）。
CONTRACT_PREFIXES="claude claude-desktop codex anthropic openai gemini grokbuild opencode pi"

# ── 管理面：/proxy/status ──────────────────────────────────────────────────
st="$(api GET /proxy/status)"
if [[ "$(jget "$st" 'd["data"]["running"]')" == "true" && "$(jget "$st" 'd["data"]["listenAddr"]')" == "127.0.0.1" ]]; then
  record_pass "GET /proxy/status：running=true listenAddr=127.0.0.1 port=$(jget "$st" 'd["data"]["port"]')（只绑回环，UC-007 A12）"
else
  record_fail "GET /proxy/status 不符：${st:0:240}"
fi
if [[ "$(jget "$st" 'd["data"]["port"]')" == "$PROXY_PORT" && "$(jget "$st" 'd["data"]["restartRequired"]')" == "false" ]]; then
  record_pass "实际监听端口 == 配置端口（${PROXY_PORT}）且 restartRequired=false"
else
  record_fail "端口/重启标记不符：port=$(jget "$st" 'd["data"]["port"]') configuredPort=$(jget "$st" 'd["data"]["configuredPort"]') restartRequired=$(jget "$st" 'd["data"]["restartRequired"]')"
fi

got_prefixes="$(jget "$st" '" ".join(sorted(p["prefix"] for p in d["data"]["prefixes"]))')"
# shellcheck disable=SC2086  # 这里**有意**按空白分词：CONTRACT_PREFIXES 是前缀清单
want_prefixes="$(printf '%s\n' $CONTRACT_PREFIXES | LC_ALL=C sort | tr '\n' ' ' | sed 's/ $//')"
if [[ "$got_prefixes" == "$want_prefixes" ]]; then
  record_pass "九个前缀与契约 §7 完全一致：${got_prefixes}"
else
  record_fail "前缀集合不符：实际=[${got_prefixes}] 契约=[${want_prefixes}]"
fi
modes="$(jget "$st" '" ".join(sorted({p["mode"] for p in d["data"]["prefixes"]}))')"
if [[ "$modes" == "passthrough" ]]; then
  record_pass "全部前缀默认 mode=passthrough（UC-017 用户裁定：直传为默认，转换矩阵默认关）"
else
  record_fail "默认 mode 集合=${modes}，期望 {passthrough}"
fi


# ── 把九个前缀指到桩上（一次 API 调用改一条，同时验回读一致）──────────────
route_ok=1
for p in $CONTRACT_PREFIXES; do
  r="$(api PUT "/proxy/routes/${p}" "{\"upstream\":\"${STUB_URL}\"}")"
  back="$(jget "$r" "next((x['upstream'] for x in d['data']['items'] if x['prefix']=='$p'), '')")"
  [[ "$back" == "$STUB_URL" ]] || { route_ok=0; record_fail "PUT /proxy/routes/${p} 回读不一致：${back:-<无>}"; }
done
(( route_ok == 1 )) && record_pass "PUT /proxy/routes/{prefix} 九条全部回读一致（上游=${STUB_URL}）"

# ── ① 九个前缀均可达 ───────────────────────────────────────────────────────
stub_reset
for p in $CONTRACT_PREFIXES; do
  code="$(curl -sS -o "$RUN/dp.$p.json" -w '%{http_code}' -m 15 -X POST \
      -H 'Content-Type: application/json' \
      --data-binary '{"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}]}' \
      "http://127.0.0.1:${PROXY_PORT}/${p}/v1/chat/completions")"
  if [[ "$code" == "200" ]]; then
    record_pass "数据面 /${p}/... 可达（HTTP ${code}，上游桩应答: $(head -c 80 "$RUN/dp.$p.json" | tr -d '\n'))"
  else
    record_fail "数据面 /${p}/... 不可达：HTTP ${code} 正文=$(head -c 160 "$RUN/dp.$p.json")"
  fi
done
hits="$(stub_hits 'POST /v1/chat/completions')"
if [[ "$hits" == "9" ]]; then
  record_pass "上游桩收到 9 条转发（九前缀各一条，逐条真的出了本地代理）"
else
  record_fail "上游桩收到的转发数=${hits}，期望 9（桩日志 ${STUB_LOG}）"
fi

# ── ② 未列前缀 404 ────────────────────────────────────────────────────────
notlisted="$(curl -sS -o "$RUN/dp.bad.json" -w '%{http_code}' -m 10 "http://127.0.0.1:${PROXY_PORT}/nope/v1/models")"
badbody="$(cat "$RUN/dp.bad.json")"
badcode="$(api_error_code "$badbody")"
if [[ "$notlisted" == "404" && "$badcode" == "not_found" ]]; then
  record_pass "未列前缀 → HTTP 404 + error.code=not_found（UC-007 A7，不回落成某个默认前缀）"
else
  record_fail "未列前缀的表现：HTTP ${notlisted} code=${badcode:-<无>}（正文 ${badbody:0:160}）"
fi

# ── ③ 目录命中逐字节等同（且命中不触上游）─────────────────────────────────
before_hits="$(stub_hits 'GET /v1/models')"
mcode="$(curl -sS -o "$RUN/models.codex.bin" -w '%{http_code}' -D "$RUN/models.codex.hdr" -m 10 \
    "http://127.0.0.1:${PROXY_PORT}/codex/v1/models")"
exp_sha="$(shasum -a 256 "$RUN/catalog.codex.expected" | awk '{print $1}')"
got_sha="$(shasum -a 256 "$RUN/models.codex.bin" | awk '{print $1}')"
ctype="$(grep -i '^content-type:' "$RUN/models.codex.hdr" | tr -d '\r' | awk '{print $2}')"
# (a) 与 /policy 的 catalogs.codex **原文**（按字节区间取，不重编码）比对。
api GET /policy >"$RUN/policy.resp.json"
cat >"$RUN/policy_slice.py" <<'SLICE'
import json, sys
raw = open(sys.argv[1], encoding="utf-8").read()
dec = json.JSONDecoder()
i = raw.index('"catalogs"')
j = raw.index(":", i) + 1
while raw[j] in " \t\r\n":
    j += 1
_, _ = dec.raw_decode(raw, j)          # 跳过 catalogs 对象本身
k = raw.index('"' + sys.argv[2] + '"', j)
c = raw.index(":", k) + 1
while raw[c] in " \t\r\n":
    c += 1
_, end = dec.raw_decode(raw, c)
sys.stdout.write(raw[c:end])
SLICE
python3 "$RUN/policy_slice.py" "$RUN/policy.resp.json" codex >"$RUN/catalog.codex.served_policy"
pol_sha="$(shasum -a 256 "$RUN/catalog.codex.served_policy" | awk '{print $1}')"
# 与 /policy 的字节区间比对只作**观察**：/policy 的响应是服务端重新生成的 JSON 信封，
# 它的等价渲染（空白/数字字面量可能被规范化）不受 §7 约束；受约束的是「数据面命中即搬运
# 策略目录载荷」——那一条由下面的 (b)（与注入的策略文件原文比对）判定。
if [[ "$got_sha" == "$pol_sha" ]]; then
  printf 'note    /policy 的 catalogs 原文与数据面下发字节一致（sha256=%s）\n' "${got_sha:0:12}"
else
  printf 'note    /policy 信封里 catalogs 的渲染与数据面字节不同（/policy=%s 数据面=%s）：这是信封重渲染，不作为契约断言\n' \
    "${pol_sha:0:12}" "${got_sha:0:12}"
fi
if [[ "$mcode" == "200" && -z "$ctype" ]]; then
  record_fail "目录命中的 Content-Type 缺失（应显式声明 application/json，避免客户端按文本流处理）"
fi

# (b) §7 的主判据：**注入什么字节，数据面就下发什么字节**（原始字节搬运、不重编码、保键序）。
#     载荷刻意写成不规则空白 + 非字典序键 + 1e6/1.50 这类字面量：任何解析后重编码都会破功。
if [[ "$got_sha" == "$exp_sha" ]]; then
  record_pass "GET /codex/v1/models 与注入的策略目录原文**逐字节等同**（sha256=${got_sha:0:12} 字节=$(wc -c <"$RUN/models.codex.bin" | tr -d ' ') content-type=${ctype}）——原始字节搬运、未重编码、键序与数字字面量原样"
else
  record_fail "目录命中未逐字节等同注入原文：HTTP ${mcode} 期望 sha256=${exp_sha:0:12} 实际=${got_sha:0:12}（注入字节=$(cat "$RUN/catalog.codex.expected") / 下发字节=$(cat "$RUN/models.codex.bin")）"
fi
if [[ "$(shasum -a 256 "$RUN/catalog.codex.compact" | awk '{print $1}')" != "$exp_sha" ]]; then
  record_pass "该用例非空断言：紧凑重编码与策略原文不同（sha 不等），故「逐字节等同」能区分搬运与重编码"
else
  record_fail "用例退化：紧凑重编码与原文相同，无法区分搬运与重编码"
fi
after_hits="$(stub_hits 'GET /v1/models')"
if [[ "$before_hits" == "$after_hits" ]]; then
  record_pass "目录命中不触上游（命中前后上游桩的 /v1/models 计数均为 ${after_hits}）"
else
  record_fail "目录命中却触了上游（上游计数 ${before_hits}→${after_hits}）"
fi

# 未命中目录（pi 没有 catalogs）→ 回落既有行为，即**转发上游**。
stub_reset
pcode="$(curl -sS -o "$RUN/models.pi.bin" -w '%{http_code}' -m 15 "http://127.0.0.1:${PROXY_PORT}/pi/v1/models")"
p_hits="$(stub_hits 'GET /v1/models')"
if [[ "$pcode" == "200" && "$p_hits" == "1" ]]; then
  record_pass "未命中目录的 /pi/v1/models 回落为转发上游（HTTP ${pcode}，上游桩收到 1 条 GET /v1/models）"
else
  record_fail "未命中回落不符：HTTP ${pcode} 上游收到=${p_hits}（期望 200 与 1）"
fi

# ── ④ 模型名映射 / 自映射拒绝（UC-007 A4、UC-017 A3）──────────────────────
mpid="$(api GET /tools/claude/profiles | jget - 'next((p["id"] for p in d["data"]["items"] if p.get("kind")=="managed"), "")')"
[[ -n "$mpid" ]] || die "取不到 claude 的受管卡"
pv="$(api POST /tools/claude/preview "{\"profileId\":\"${mpid}\"}")"
pvid="$(jget "$pv" 'd["data"]["previewId"]')"
[[ -n "$pvid" ]] || die "claude preview 未返回 previewId：${pv:0:200}"
api POST /tools/claude/apply "{\"previewId\":\"${pvid}\"}" >/dev/null
tier_model="$(jget "$(cat "$RUN/.claude/settings.json")" '((d.get("env") or {}).get("ANTHROPIC_MODEL") or "")')"
if [[ -n "$tier_model" && "$tier_model" != "deepseek-flash" ]]; then
  record_pass "受管档写出的客户端可见档名（ANTHROPIC_MODEL=${tier_model}）与上游名（deepseek-flash）不同 ⇒ 映射必须能在本地代理内发生"
else
  record_fail "受管档的档名=${tier_model:-<空>}，构造不出「档名 ≠ 上游名」的映射场景"
fi
stub_reset
curl -sS -o /dev/null -m 15 -X POST -H 'Content-Type: application/json' \
  --data-binary "{\"model\":\"${tier_model}\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}" \
  "http://127.0.0.1:${PROXY_PORT}/claude/v1/messages" || true
seen_model="$(grep -o 'model=[^ ]*' "$STUB_LOG" | head -1 | sed 's/^model=//')"
if [[ "$seen_model" == "deepseek-flash" ]]; then
  record_pass "模型名映射生效：客户端发 ${tier_model} ⇒ 上游收到 ${seen_model}（UC-017 A3）"
else
  record_fail "模型名映射未生效：客户端发 ${tier_model} ⇒ 上游收到 ${seen_model:-<无转发>}（断言依据：UC-017 A3 要求档名映射到该档的上游名，这里是 deepseek-flash）"
fi
# ── ⑤ 上游非 2xx 状态透传 ────────────────────────────────────────────────
ncode="$(curl -sS -o "$RUN/dp.418.json" -w '%{http_code}' -m 15 -X POST -H 'Content-Type: application/json' \
    --data-binary '{"model":"stub-status-418","messages":[{"role":"user","content":"hi"}]}' \
    "http://127.0.0.1:${PROXY_PORT}/openai/v1/chat/completions")"
if [[ "$ncode" == "418" ]] && grep -q 'stub-injected' "$RUN/dp.418.json"; then
  record_pass "上游非 2xx 状态被逐字透传（HTTP 418 + 上游正文原样，未被吞成 200）"
else
  record_fail "非 2xx 未透传：HTTP ${ncode} 正文=$(head -c 160 "$RUN/dp.418.json")"
fi

# ── ⑥ SSE 逐帧透传（首帧时间与响应总长无关）──────────────────────────────
sse_short="$(curl -sS -o "$RUN/sse.short.txt" -w '%{time_starttransfer} %{time_total}' -m 20 -X POST \
    -H 'Content-Type: application/json' --data-binary '{"model":"stub-sse-1x0","stream":true,"messages":[]}' \
    "http://127.0.0.1:${PROXY_PORT}/openai/v1/chat/completions")"
sse_long="$(curl -sS -o "$RUN/sse.long.txt" -w '%{time_starttransfer} %{time_total}' -m 30 -X POST \
    -H 'Content-Type: application/json' --data-binary '{"model":"stub-sse-6x400","stream":true,"messages":[]}' \
    "http://127.0.0.1:${PROXY_PORT}/openai/v1/chat/completions")"
read -r ss_first ss_total <<<"$sse_short"
read -r ls_first ls_total <<<"$sse_long"
frames_short="$(grep -c '^data:' "$RUN/sse.short.txt" || true)"
frames_long="$(grep -c '^data:' "$RUN/sse.long.txt" || true)"
awk_first="$(awk -v a="$ss_first" -v b="$ls_first" 'BEGIN{print (a<0.8 && b<0.8) ? "1" : "0"}')"
awk_len="$(awk -v a="$ss_total" -v b="$ls_total" 'BEGIN{print (b-a>1.0 && b>1.5) ? "1" : "0"}')"
if [[ "$frames_short" == "1" && "$frames_long" == "6" && "$awk_first" == "1" && "$awk_len" == "1" ]]; then
  record_pass "SSE 逐帧透传：帧数 1/6 相符，首帧 ${ss_first}s / ${ls_first}s（均 <0.8s，与总长无关），总时长 ${ss_total}s / ${ls_total}s（差 >1s ⇒ 不是全量缓冲后一次性返回）"
else
  record_fail "SSE 透传不符：帧数 ${frames_short}/${frames_long}（期望 1/6）首帧 ${ss_first}/${ls_first} 总时长 ${ss_total}/${ls_total}"
fi

# ── ⑦ 每请求一条用量记录（含映射前后模型名）───────────────────────────────
n0="$(api GET "/usage/requests?limit=200" | jget - 'd["data"]["total"]')"
mark="$(date +%s)"
curl -sS -o /dev/null -m 15 -X POST -H 'Content-Type: application/json' \
  --data-binary '{"model":"deepseek-flash","messages":[]}' \
  "http://127.0.0.1:${PROXY_PORT}/opencode/v1/chat/completions" || true
curl -sS -o /dev/null -m 15 -X POST -H 'Content-Type: application/json' \
  --data-binary '{"model":"stub-status-503","messages":[]}' \
  "http://127.0.0.1:${PROXY_PORT}/opencode/v1/chat/completions" || true
sleep 0.3
reqs="$(api GET "/usage/requests?limit=200")"
n1="$(jget "$reqs" 'd["data"]["total"]')"
delta=$((n1 - n0))
if [[ "$delta" == "2" ]]; then
  record_pass "每请求恰好一条用量记录（两次请求 ⇒ 新增 ${delta} 条）"
else
  record_fail "用量记录条数不符：两次请求新增 ${delta} 条（期望 2）；n0=${n0} n1=${n1}"
fi
new_recs="$(jget "$reqs" '"|".join(b["app"]+"/"+b["prefix"]+"/"+str(b["status"])+"/"+str(b["modelRequested"])+"->"+str(b["modelUpstream"]) for b in d["data"]["items"][:2])')"
if [[ "$new_recs" == *"opencode/opencode/200/deepseek-flash->deepseek-flash"* && "$new_recs" == *"opencode/opencode/503/"* ]]; then
  record_pass "记录含 app/prefix/status 与映射前后模型名：${new_recs}"
else
  record_fail "记录字段不符（期望含 opencode 的两条、状态 200 与 503）：${new_recs}"
fi
# 时间窗过滤：本实现的列表端点要求 from/to **同时**给出（只给一侧返回 invalid_request，
# 实测如此；契约 §8 只写窗口是闭开区间 [from,to)，未写单侧无界）。
from_ts="$(date -u -r "$((mark - 60))" +%Y-%m-%dT%H:%M:%SZ)"
to_ts="$(date -u -r "$((mark + 60))" +%Y-%m-%dT%H:%M:%SZ)"
win="$(api GET "/usage/requests?from=${from_ts}&to=${to_ts}&limit=200" | jget - 'd["data"]["total"]')"
if [[ -n "$win" && "$win" -ge 2 ]]; then
  record_pass "时间窗过滤可用（闭开区间 [${from_ts}, ${to_ts}) 内 ${win} 条 ≥ 刚造的 2 条）"
else
  record_fail "时间窗过滤异常：window=${win:-<空>}（from=${from_ts} to=${to_ts}）"
fi

# 非 2xx 的那条记录必须如实带状态码（不是 200）。
bad_rec_status="$(jget "$reqs" 'next((str(b["status"]) for b in d["data"]["items"] if b["status"]>=500), "")')"
if [[ "$bad_rec_status" == "503" ]]; then
  record_pass "失败请求的记录保留上游状态码 503（不冒充 200）"
else
  record_fail "失败请求的记录状态码=${bad_rec_status:-<无>}，期望 503"
fi

# ── ⑧ 自映射被拒（UC-007 A4）───────────────────────────────────────────────
# 自映射 = 「档名被配成了映射目标」。装配层从**受管计划字节**推导映射
# （internal/api/modelmap.go：ANTHROPIC_MODEL 等客户侧档名 → 上游名），
# 于是把策略的 defaultModel 种成客户侧档名，就能构造出 A→A 的自映射：
#   档名（ANTHROPIC_MODEL）= 上游名（策略 default）= claude-fable-5-1。
# 期望：装载被拒且**响亮**记日志（绝不静默装上一张失效的表），数据面因此不做改写。
tier_model_raw="$(jget "$(cat "$RUN/.claude/settings.json")" '((d.get("env") or {}).get("ANTHROPIC_MODEL") or "")')"
tier_model="${tier_model_raw%\[1m\]}"
if [[ -z "$tier_model" || "$tier_model" == "kein" ]]; then
  record_fail "取不到客户侧档名（${RUN}/.claude/settings.json 的 env.ANTHROPIC_MODEL=${tier_model_raw:-<空>}），构造不出自映射场景"
else
  # 第二个隔离实例：策略 default = 客户侧档名（自映射的直接触发条件）。
  RUN1="$RUN"
  RUN2="$RUN/selfmap-run"
  mkdir -p "$RUN2"
  kill "$DAEMON_PID" 2>/dev/null || true
  wait "$DAEMON_PID" 2>/dev/null || true
  DAEMON_PID=""
  RUN="$RUN2"
  printf '{"version":2,"defaultModel":"%s","models":[{"id":"%s","name":"%s","supportsImages":true}],"catalogs":{}}\n' \
    "$tier_model" "$tier_model" "$tier_model" >"$RUN2/policy.json"
  export YZAGMGR_POLICY_FILE="$RUN2/policy.json"
  start_isolated_daemon
  require_own_daemon

  mpid2="$(api GET /tools/claude/profiles | jget - 'next((p["id"] for p in d["data"]["items"] if p.get("kind")=="managed"), "")')"
  if [[ -z "$mpid2" ]]; then
    record_fail "自映射场景：取不到 claude 受管卡"
  else
    pv2="$(api POST /tools/claude/preview "{\"profileId\":\"${mpid2}\"}")"
    pvid2="$(jget "$pv2" 'd["data"]["previewId"]')"
    if [[ -z "$pvid2" ]]; then
      record_fail "自映射场景：preview 未返回 previewId（${pv2:0:200}）"
    else
      api POST /tools/claude/apply "{\"previewId\":\"${pvid2}\"}" >/dev/null
      api PUT "/proxy/routes/claude" "{\"upstream\":\"${STUB_URL}\"}" >/dev/null
      warn_line="$(grep -m1 "映射" "$RUN2/daemon.log" 2>/dev/null || true)"
      warn_msg="${warn_line##*msg=}"
      if [[ -n "$warn_line" ]]; then
        # 用 bash 子串而不是 `cut -c`：中文证据串在 C locale 下会被按字节切开，
        # 产出非法 UTF-8 字节，之后 grep 会把整份输出当二进制文件（实测踩到）。
        record_pass "自映射被拒且**响亮**记日志：${warn_msg:0:140}"
      else
        log_tail="$(tail -3 "$RUN2/daemon.log" 2>/dev/null | tr '\n' ' ')"
        record_fail "自映射没有被响亮报出（日志里没有映射相关告警）：${log_tail:0:200}"
      fi
      # 装载被拒 ⇒ 数据面不做改写：档名必须原样出网（而不是被换成一个不存在的上游名）。
      stub_reset
      curl -sS -o /dev/null -m 20 -X POST -H 'Content-Type: application/json' \
        --data-binary "{\"model\":\"${tier_model_raw}\",\"messages\":[]}" \
        "http://127.0.0.1:${PROXY_PORT}/claude/v1/messages" || true
      seen_selfmap="$(grep -o 'model=[^ ]*' "$STUB_LOG" | head -1 | sed 's/^model=//')"
      if [[ "$seen_selfmap" == "$tier_model" ]]; then
        record_pass "自映射被拒后数据面如实透传（上游收到 ${seen_selfmap}，即未装载任何映射）——与「装上但静默失效」可区分"
      else
        record_fail "自映射场景下的数据面模型名=${seen_selfmap:-<无转发>}，期望原样透传 ${tier_model}"
      fi
    fi
  fi
  RUN="$RUN1"
fi

verify_finish

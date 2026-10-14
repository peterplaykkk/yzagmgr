#!/usr/bin/env bash
# scripts/verify/usage.sh —— 用量观测面（UC-008）。
#
# 验收手册: doc/acceptance-steps.md §6；契约 §1.5（分页）+ §8（用量）。
#
# 前置（未就绪即 BLOCKED，退出码 2）：
#   - bin/yzagmgr；种子策略 + 令牌；本机存在**非回环** IPv4（上游桩要填非回环地址）。
#
# 造数方式（写进脚本、可重复）：经**本地代理**真发请求到 hermetic 上游桩
#   （lib.sh 的 start_stub_upstream，桩按路径返回带 usage 的应答），
#   故库里每一条记录都是真实转发留下的 —— 不用直接往 usage.db 塞夹具，
#   那样只能证明「读得出来」，证明不了「记得对」。
#   三类造数：成功（200，带 token 数）/ 上游失败（503）/ 流式（SSE，token 数在帧里）。
#
# 用法: usage.sh
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib.sh
. "$HERE/lib.sh"

verify_init usage
seed_test_policy "https://gateway.example.com"
choose_isolated_ports
start_isolated_daemon
require_own_daemon

UP_IP="$(host_ip || true)"
[[ -n "$UP_IP" ]] || die "本机没有非回环 IPv4：上游守卫禁止回环（UC-007 A13），无法起 hermetic 上游桩"
# 桩口由 lib.sh 请内核分配（**不要**写成 proxy+100：代理口抽到 65487 时那个算术会越界，
# python 的 bind 抛 OverflowError，suite 会以一句没有信息的 BLOCKED 收场）。
start_stub_upstream
STUB_URL="http://${UP_IP}:${STUB_PORT}"

for p in claude codex opencode; do
  api PUT "/proxy/routes/${p}" "{\"upstream\":\"${STUB_URL}\"}" >/dev/null
done

# 造数：一条独特的哨兵串放进请求体，稍后断言它**没有**出现在任何记录里（UC-008 A9）。
SENTINEL="SENTINEL-DO-NOT-LOG-$(date +%s)-$$"
fire() { # fire <prefix> <path> <model> [stream]
  local prefix="$1" path="$2" model="$3" stream="${4:-false}"
  local body
  if [[ "$prefix" == "claude" ]]; then
    body="{\"model\":\"${model}\",\"stream\":${stream},\"messages\":[{\"role\":\"user\",\"content\":\"${SENTINEL}\"}]}"
  else
    body="{\"model\":\"${model}\",\"stream\":${stream},\"messages\":[{\"role\":\"user\",\"content\":\"${SENTINEL}\"}]}"
  fi
  curl -sS -o /dev/null -m 25 -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
    --data-binary "$body" "http://127.0.0.1:${PROXY_PORT}/${prefix}${path}"
}

# 三段造数之间留 1s 间隔：后面要用「某条记录的 at 秒」当窗口边界，同秒多条会让边界断言
# 依赖「同一秒内再切分」这种实现细节（契约只保证秒精度渲染）。
c1="$(fire claude /v1/messages deepseek-flash)"; sleep 1.1
c2="$(fire codex /v1/responses deepseek-flash)"; sleep 1.1
c3="$(fire opencode /v1/chat/completions stub-status-503)"; sleep 1.1
c4="$(fire opencode /v1/chat/completions deepseek-flash true)"
if [[ "$c1" == "200" && "$c2" == "200" && "$c3" == "503" && "$c4" == "200" ]]; then
  record_pass "造数：4 条真实转发（200/200/503/200 流式），逐条经本地代理到上游桩"
else
  record_fail "造数失败：HTTP 序列 ${c1}/${c2}/${c3}/${c4}，期望 200/200/503/200"
fi
sleep 1.1

# ── 取全量记录（分页走到底，limit=200 一次拿全）──────────────────────────
all="$(api GET "/usage/requests?limit=200")"
total="$(jget "$all" 'd["data"]["total"]')"
n_rec="$(jget "$all" 'len(d["data"]["items"])')"
ids="$(jget "$all" '"|".join(b["id"] for b in d["data"]["items"])')"
if [[ "$total" == "4" && "$n_rec" == "4" ]]; then
  record_pass "库里有 4 条记录（total=${total}，与造数一致；每请求一条，不多不少）"
else
  record_fail "记录数不符：total=${total} items=${n_rec}，期望 4/4（响应: ${all:0:200}）"
fi

# 记录不得含请求正文（哨兵串）也不得含令牌明文（UC-008 A9）。
if printf '%s' "$all" | grep -qF "$SENTINEL"; then
  record_fail "记录里出现了请求正文哨兵串（UC-008 A9：记录不含请求/响应正文）"
else
  record_pass "记录不含请求正文（哨兵串 ${SENTINEL:0:18}… 未出现在列表响应里）"
fi
if printf '%s' "$all" | grep -qF "acceptance-token-0000"; then
  record_fail "记录里出现了推理令牌明文（UC-008 A9）"
else
  record_pass "记录不含令牌明文（种子令牌未出现在列表响应里）"
fi

# 逐条字段形状 + 「映射前后模型名 + 上游状态码」（UC-008）。
shape_bad="$(printf '%s' "$all" | python3 -c '
import json, sys
items = json.load(sys.stdin)["data"]["items"]
need = ["id","at","app","prefix","provider","modelRequested","modelUpstream","status",
        "durationMs","inputTokens","outputTokens","streamed","errorCode"]
bad = []
for b in items:
    miss = [k for k in need if k not in b]
    if miss:
        bad.append(b.get("id","?") + " 缺字段 " + ",".join(miss))
    if b.get("status") != 200 and not b.get("errorCode"):
        bad.append(b.get("id","?") + " 非 2xx 却没有 errorCode")
print("|".join(bad))
')"
if [[ -z "$shape_bad" ]]; then
  record_pass "每条记录的字段形状完整（含 modelRequested/modelUpstream/status/errorCode）"
else
  record_fail "记录字段形状有缺：${shape_bad}"
fi

tok_ok="$(printf '%s' "$all" | python3 -c '
import json, sys
items = json.load(sys.stdin)["data"]["items"]
bad = []
for b in items:
    if b["prefix"] == "claude" or b["prefix"] == "codex":
        # 非流式应答里桩声明 input=3 / output=2（见 lib.sh 的桩），必须被如实记下来。
        if (b["inputTokens"], b["outputTokens"]) != (3, 2):
            bad.append(b["id"] + " " + str((b["inputTokens"], b["outputTokens"])))
    if b["status"] == 503 and b["errorCode"] == "":
        bad.append(b["id"] + " 503 无 errorCode")
print("|".join(bad))
')"
if [[ -z "$tok_ok" ]]; then
  record_pass "非流式记录的 token 数取自上游应答（input=3/output=2，与桩声明一致）"
else
  record_fail "token 数或错误码不符：${tok_ok}"
fi

streamed_rec="$(printf '%s' "$all" | jget - 'str(any(b["streamed"] for b in d["data"]["items"]))')"
if [[ "$streamed_rec" == "True" ]]; then
  record_pass "流式请求的 streamed=true 被如实记录"
else
  record_fail "没有记录被标为 streamed（造数里有一条 SSE 请求）"
fi

detail_id="${ids%%|*}"
detail="$(api GET "/usage/requests/${detail_id}")"
if [[ "$(jget "$detail" 'd["data"]["id"]')" == "$detail_id" && -n "$(jget "$detail" 'd["data"]["modelUpstream"]')" ]]; then
  record_pass "GET /usage/requests/{id} 单条可读且含映射前后模型名与状态码（id=${detail_id} status=$(jget "$detail" 'd["data"]["status"]') modelUpstream=$(jget "$detail" 'd["data"]["modelUpstream"]')）"
else
  record_fail "单条详情不符：${detail:0:200}"
fi
missing="$(api GET "/usage/requests/rq_no_such_id" || true)"
if [[ "$(api_error_code "$missing")" == "not_found" ]]; then
  record_pass "未知记录 id → not_found"
else
  record_fail "未知记录 id 的响应不符：${missing:0:200}"
fi

# ── summary ↔ 原始记录逐项对账 ────────────────────────────────────────────
# 窗口取「最早一条的 at」（列表默认 desc，末条即最早）起、「最新一条的 at」+60s 止。
last_at="$(jget "$all" 'd["data"]["items"][0]["at"]')"
from_ts="$(rfc3339_utc "$(jget "$all" 'd["data"]["items"][-1]["at"]')")"
to_ts="$(rfc3339_utc "$(rfc3339_shift "$last_at" 60)")"

sum="$(api GET "/usage/summary?from=${from_ts}&to=${to_ts}")"
s_req="$(jget "$sum" 'd["data"]["requests"]')"
s_in="$(jget "$sum" 'd["data"]["inputTokens"]')"
s_out="$(jget "$sum" 'd["data"]["outputTokens"]')"
r_in="$(printf '%s' "$all" | jget - 'sum(b["inputTokens"] for b in d["data"]["items"])')"
r_out="$(printf '%s' "$all" | jget - 'sum(b["outputTokens"] for b in d["data"]["items"])')"
if [[ "$s_req" == "$total" && "$s_in" == "$r_in" && "$s_out" == "$r_out" ]]; then
  record_pass "summary 与原始记录逐项对账相等（requests=${s_req} input=${s_in} output=${s_out}）"
else
  record_fail "summary 与记录不一致：summary=${s_req}/${s_in}/${s_out} 记录=${total}/${r_in}/${r_out}"
fi

# ── 时间窗：闭开区间 [from, to) ──────────────────────────────────────────
zero_ts="$(rfc3339_utc "$last_at")"
zero="$(api GET "/usage/summary?from=${zero_ts}&to=${zero_ts}")"
if [[ "$(jget "$zero" 'd["data"]["requests"]')" == "0" ]]; then
  record_pass "零长窗口 [${zero_ts}, ${zero_ts}) 计数为 0（上界**不含**：闭开区间）"
else
  record_fail "零长窗口计数=$(jget "$zero" 'd["data"]["requests"]')，期望 0（若为 1 说明按闭区间算了）"
fi

# 边界记录只计一次：在某个记录的 at 处切开，两段之和 == 总量。
mid="$(rfc3339_utc "$(jget "$all" 'd["data"]["items"][1]["at"]')")"
seg1="$(api GET "/usage/summary?from=${from_ts}&to=${mid}")"
seg2="$(api GET "/usage/summary?from=${mid}&to=${to_ts}")"
seg_sum="$(($(jget "$seg1" 'd["data"]["requests"]') + $(jget "$seg2" 'd["data"]["requests"]')))"
if [[ "$seg_sum" == "$s_req" ]]; then
  record_pass "边界记录只计一次：[${from_ts},${mid}) + [${mid},${to_ts}) = ${seg_sum} == 总量 ${s_req}"
else
  record_fail "分段计数与总量不符：${seg_sum} vs ${s_req}（重或漏）"
fi

# 空窗口：没有记录时 cost=0 且 costUnknown=false（与「有记录但无价」是两个结论）。
empty="$(api GET "/usage/summary?from=2000-01-01T00:00:00Z&to=2000-01-02T00:00:00Z")"
if [[ "$(jget "$empty" 'd["data"]["requests"]')" == "0" && "$(jget "$empty" 'd["data"]["cost"]')" == "0" && "$(jget "$empty" 'd["data"]["costUnknown"]')" == "false" ]]; then
  record_pass "空窗口：requests=0 且 cost=0、costUnknown=false（已知的零，不同于「有记录但无价」）"
else
  record_fail "空窗口的结论不符：${empty:0:240}"
fi

# 非法时间格式必须被拒，不得静默当全窗口。
bad_win="$(api GET "/usage/summary?from=not-a-time&to=also-bad" || true)"
if [[ "$(api_error_code "$bad_win")" == "invalid_request" ]]; then
  record_pass "非法时间格式 → invalid_request（不静默当全窗口）"
else
  record_fail "非法时间格式未被拒：${bad_win:0:200}"
fi

# ── 趋势：空桶补零 ────────────────────────────────────────────────────────
t_from="$(rfc3339_utc "$(rfc3339_shift "$last_at" -259200)")"
trend="$(api GET "/usage/trends?from=${t_from}&to=${to_ts}&bucket=day&by=all")"
pts_n="$(jget "$trend" 'len(d["data"]["series"][0]["points"])')"
empty_buckets="$(jget "$trend" 'sum(1 for p in d["data"]["series"][0]["points"] if p["requests"]==0 and p["tokens"]==0)')"
nonempty_no_tokens="$(jget "$trend" 'sum(1 for p in d["data"]["series"][0]["points"] if p["requests"]>0 and p["tokens"]==0)')"
sum_pts="$(jget "$trend" 'sum(p["requests"] for p in d["data"]["series"][0]["points"])')"
# 桶数 = ceil(窗口跨度 / 桶长)：窗口是「最新一条 at 前 3 天 → 该 at + 60s」⇒ 4 个日桶。
# 空桶的**个数**取决于记录落点（分钟级偏移），故只断「至少有一个零值桶被显式补出零」，
# 不钉具体个数（钉死会随夹具时序漂移，实测踩到）。
if [[ "$pts_n" == "4" && "$empty_buckets" -ge 1 && "$nonempty_no_tokens" == "0" && "$sum_pts" == "$s_req" ]]; then
  record_pass "趋势空桶补零：4 个日桶中 ${empty_buckets} 个零值桶（显式 requests=0/tokens=0），各桶之和 ${sum_pts} == 窗口总量 ${s_req}"
else
  record_fail "趋势补零不符：桶数=${pts_n}（期望 4）零值桶=${empty_buckets}（期望 ≥1）有请求但零 token 的桶=${nonempty_no_tokens} 求和=${sum_pts} vs ${s_req}"
fi
trend_keys="$(jget "$trend" '"|".join(p["at"] for p in d["data"]["series"][0]["points"])')"
if [[ -n "$trend_keys" && "$(jget "$trend" 'd["data"]["bucket"]')" == "day" ]]; then
  record_pass "趋势点自带时间戳且 bucket 回显正确（首点 $(printf '%s' "$trend_keys" | cut -d'|' -f1)…）"
else
  record_fail "趋势点形状不符：bucket=$(jget "$trend" 'd["data"]["bucket"]') keys=${trend_keys:0:120}"
fi

# ── 三维拆分：各自求和 == 总量 ───────────────────────────────────────────
for by in app provider model; do
  bd="$(api GET "/usage/breakdown?from=${from_ts}&to=${to_ts}&by=${by}")"
  b_req="$(jget "$bd" 'sum(i["requests"] for i in d["data"]["items"])')"
  b_in="$(jget "$bd" 'sum(i["inputTokens"] for i in d["data"]["items"])')"
  b_out="$(jget "$bd" 'sum(i["outputTokens"] for i in d["data"]["items"])')"
  if [[ "$b_req" == "$s_req" && "$b_in" == "$s_in" && "$b_out" == "$s_out" ]]; then
    record_pass "breakdown by=${by} 求和 == 总量（requests=${b_req} input=${b_in} output=${b_out}；键=$(jget "$bd" '"|".join(i["key"] for i in d["data"]["items"])')）"
  else
    record_fail "breakdown by=${by} 求和与总量不符：${b_req}/${b_in}/${b_out} vs ${s_req}/${s_in}/${s_out}"
  fi
done

# ── 翻页不重不漏 ─────────────────────────────────────────────────────────
page1="$(api GET "/usage/requests?limit=2&order=desc")"
cursor1="$(jget "$page1" 'd["data"]["nextCursor"]')"
page2="$(api GET "/usage/requests?limit=2&cursor=${cursor1}&order=desc")"
cursor2="$(jget "$page2" 'd["data"]["nextCursor"]')"
all_ids="$(printf '%s' "$all" | jget - '"|".join(b["id"] for b in d["data"]["items"])')"
walk_ids="$(jget "$page1" '"|".join(b["id"] for b in d["data"]["items"])')|$(jget "$page2" '"|".join(b["id"] for b in d["data"]["items"])')"
dup_n="$(printf '%s' "$walk_ids" | tr '|' '\n' | sort | uniq -d | wc -l | tr -d ' ')"
if [[ "$walk_ids" == "$all_ids" && "$cursor2" == "" && "$dup_n" == "0" ]]; then
  record_pass "翻页不重不漏：limit=2 两页（游标串起）与 limit=200 一次取全完全一致（${walk_ids}）"
else
  record_fail "翻页不重不漏不成立：walk=${walk_ids} 全量=${all_ids} 重复=${dup_n} 末页 nextCursor=${cursor2}"
fi
page_totals="$(jget "$page1" 'd["data"]["total"]')/$(jget "$page2" 'd["data"]["total"]')"
if [[ "$page_totals" == "${s_req}/${s_req}" ]]; then
  record_pass "翻页期间 total 稳定（两页均为 ${s_req}）"
else
  record_fail "翻页 total 不稳定：${page_totals}"
fi

# ── 单价缺失时金额未知而不是 0 ───────────────────────────────────────────
if [[ "$(jget "$sum" 'd["data"]["costUnknown"]')" == "true" && "$(jget "$sum" 'd["data"]["cost"]')" == "" ]]; then
  record_pass "单价缺失：costUnknown=true 且金额为 null（**不是 0**，UC-008 A7）"
else
  record_fail "单价缺失时的金额表述不符：cost=$(jget "$sum" 'd["data"]["cost"]') costUnknown=$(jget "$sum" 'd["data"]["costUnknown"]')（期望 null/true）"
fi
# 补上单价后：costUnknown 转 false，金额 > 0 且与 token 数一致（3×N 输入、2×N 输出）。
# 单价上限是 1000000/百万 token（实现守卫；写 2000000 会被 invalid_request 拒掉 ——
# 早先这里静默丢掉了 PUT 的响应，于是后面把「键没写进去」误读成「金额算不出来」（实测踩到）。
# 故这里既断言 PUT 成功，也回读一致。
pput="$(api PUT /usage/pricing '{"entries":[{"model":"deepseek-flash","inputPerMillion":1000000,"outputPerMillion":1000000,"currency":"CNY"}]}')"
if [[ "$(jget "$pput" 'd["data"]["entries"][0]["model"]')" == "deepseek-flash" ]]; then
  preg="$(api GET /usage/pricing)"
  record_pass "PUT /usage/pricing 回读一致（entries[0]=$(jget "$preg" 'd["data"]["entries"][0]["model"]') inputPerMillion=$(jget "$preg" 'd["data"]["entries"][0]["inputPerMillion"]')）"
else
  record_fail "PUT /usage/pricing 未成功：${pput:0:240}"
fi
# 全窗口：库里还有一条未定价记录（stub-status-503 的模型名）⇒ 金额整体仍记未知。
priced="$(api GET "/usage/summary?from=${from_ts}&to=${to_ts}")"
p_unknown="$(jget "$priced" 'd["data"]["costUnknown"]')"
if [[ "$p_unknown" == "true" && "$(jget "$priced" 'd["data"]["cost"]')" == "" ]]; then
  record_pass "补价后窗口内仍有未定价记录 ⇒ 金额整体记未知（cost=null/costUnknown=true，UC-008 A7 的「整体未知」语义）"
else
  record_fail "补价后的整体未知语义不符：cost=$(jget "$priced" 'd["data"]["cost"]') costUnknown=${p_unknown}"
fi
# 只含已定价记录的子窗口（app=codex 那一条）：金额可算且为正。
priced1="$(api GET "/usage/summary?from=${from_ts}&to=${to_ts}&app=codex")"
p_cost="$(jget "$priced1" 'd["data"]["cost"]')"
p_unknown1="$(jget "$priced1" 'd["data"]["costUnknown"]')"
# 期望金额 = (3+2) token × 1.0/百万 = 5e-06（与桩声明的 input=3/output=2 一致）。
if [[ "$p_unknown1" == "false" ]] && awk -v c="$p_cost" 'BEGIN{exit !(c>0)}'; then
  record_pass "有单价且窗口内全部已定价时金额可算：cost=${p_cost}（期望 5e-06 = (3+2)×1.0/1e6）costUnknown=false"
else
  record_fail "已定价窗口的金额=${p_cost:-<null>} costUnknown=${p_unknown1}，期望 >0/false"
fi
api PUT /usage/pricing '{"entries":[]}' >/dev/null
after_clear="$(api GET "/usage/summary?from=${from_ts}&to=${to_ts}")"
if [[ "$(jget "$after_clear" 'd["data"]["costUnknown"]')" == "true" ]]; then
  record_pass "清空定价表后 costUnknown 回到 true（PUT 可逆、回读一致）"
else
  record_fail "清空定价后 costUnknown=$(jget "$after_clear" 'd["data"]["costUnknown"]')，期望 true"
fi

verify_finish

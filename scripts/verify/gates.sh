#!/usr/bin/env bash
# scripts/verify/gates.sh —— 低层门禁驱动（`make gates` 的实现）。
#
# 依次跑（顺序固定，便于机器判读）：
#   go-fmt-check → go-vet → go-build → go-test
#   → web-typecheck → web-unit（web/ 未交付时登记 skip 并写明原因）
#
# 判读纪律：
#   - 四态 pass / fail / pending / skip；**pending 与 skip 不算 pass**，
#     在摘要里单独列出原因，且写进 summary.json 的 skipped 列表；
#   - 退出码：任一 fail ⇒ 1，否则 0；
#   - 「零测试」按 pending 处理（不是 pass）—— 测试由并行任务交付后自动转真判。
#   - web 检查**只跑既有依赖**：按 lockfile 判包管理器，node_modules 不存在即 skip，
#     绝不代跑 install（2026-09-22 实测：裸 `pnpm run` 会隐式安装并改写别人用 npm
#     装的 node_modules —— 门禁不得有这种副作用）。
#
# 产物：reports/gates-<时间戳>/logs/<check>.log + summary.json（reports/ 已 gitignore）。
set -uo pipefail

if ((BASH_VERSINFO[0] < 4)); then
  echo "BLOCKED: 需要 bash ≥ 4（当前 ${BASH_VERSION}）。用 'bash scripts/verify/gates.sh' 而非 /bin/bash 3.2。" >&2
  exit 2
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

GO="${GO:-go}"
TS="$(date +%Y%m%dT%H%M%S)"
REPORT="$ROOT/reports/gates-$TS"
mkdir -p "$REPORT/logs"

declare -a RESULTS=()   # name|status|detail|seconds
STEP_NAME=""
STEP_LOG=""
STEP_START=0

step_begin() {
  STEP_NAME="$1"
  STEP_LOG="$REPORT/logs/$1.log"
  STEP_START=$SECONDS
}

step_end() { # step_end <pass|fail|pending|skip> <detail>
  RESULTS+=("$STEP_NAME|$1|$2|$((SECONDS - STEP_START))")
}

go_files() {
  find . -type f -name '*.go' -not -path './bin/*' -not -path './web/*' -not -path './reports/*' 2>/dev/null
}

# ── 1. gofmt 检查（只读；make fmt 才写盘）────────────────────────────────────
step_begin go-fmt-check
mapfile -t _gofiles < <(go_files)
if ((${#_gofiles[@]} == 0)); then
  step_end pending "无 Go 文件"
elif unformatted="$(gofmt -l "${_gofiles[@]}" 2>&1)"; then
  if [[ -z "$unformatted" ]]; then
    step_end pass "gofmt -l 干净（${#_gofiles[@]} 个文件）"
  else
    printf '%s\n' "$unformatted" >"$STEP_LOG"
    step_end fail "未格式化: $(printf '%s' "$unformatted" | tr '\n' ' ')"
  fi
else
  printf '%s\n' "$unformatted" >"$STEP_LOG"
  step_end fail "gofmt 执行失败（见 $(basename "$STEP_LOG")）"
fi

# ── 2. go vet ───────────────────────────────────────────────────────────────
step_begin go-vet
if "$GO" vet ./... >"$STEP_LOG" 2>&1; then
  step_end pass "go vet ./... 干净"
else
  step_end fail "go vet 报错（见 $(basename "$STEP_LOG")）"
fi

# ── 3. go build（纯 Go，CGO_ENABLED=0）───────────────────────────────────────
step_begin go-build
if make -s build-go >"$STEP_LOG" 2>&1; then
  if [[ -d "$ROOT/cmd/yzagmgr" ]]; then
    if [[ -x "$ROOT/bin/yzagmgr" ]]; then
      step_end pass "bin/yzagmgr 已构建（CGO_ENABLED=0）"
    else
      step_end fail "cmd/yzagmgr 存在但 bin/yzagmgr 未产出"
    fi
  else
    step_end pending "cmd/yzagmgr 未交付（守护进程由并行任务实现）；已编译全部现有包"
  fi
else
  step_end fail "go build 失败（见 $(basename "$STEP_LOG")）"
fi

# ── 4. go test（-json 计数）────────────────────────────────────────────────
step_begin go-test
"$GO" test -json -count=1 ./... >"$STEP_LOG" 2>&1
test_rc=$?
read -r tests_pass tests_fail tests_skip < <(python3 - "$STEP_LOG" <<'PY'
import json, sys
p = f = s = 0
for line in open(sys.argv[1], encoding="utf-8", errors="replace"):
    line = line.strip()
    if not line.startswith("{"):
        continue
    try:
        ev = json.loads(line)
    except Exception:
        continue
    if not ev.get("Test"):
        continue
    a = ev.get("Action")
    if a == "pass":
        p += 1
    elif a == "fail":
        f += 1
    elif a == "skip":
        s += 1
print(p, f, s)
PY
)
if ((test_rc != 0)); then
  step_end fail "go test 退出码 ${test_rc}（failed=${tests_fail}，见 $(basename "$STEP_LOG")）"
elif ((tests_pass + tests_fail + tests_skip == 0)); then
  step_end pending "0 tests（测试由并行任务交付）；go test 退出码 0"
else
  step_end pass "$tests_pass passed, $tests_skip skipped, $tests_fail failed"
fi

# ── 5/6. web 检查（web/ 未交付 ⇒ skip，不算 pass）───────────────────────────
# 硬纪律：**绝不代跑 install / 绝不改动 web/node_modules**。
# 教训（2026-09-22 实测）：直接 `pnpm run` 会触发 pnpm 的 verify-deps-before-run 自动安装，
# 把别人用 npm 装的 node_modules 改写成 pnpm 布局（顶层包被搬进 .ignored、.bin 链接被删）。
# 因此这里：① 按 lockfile 判定包管理器；② node_modules 不存在即 skip（不 install）；
# ③ pnpm 路径显式关闭 verify-deps-before-run。
web_pm() {
  if [[ -f "$ROOT/web/pnpm-lock.yaml" ]]; then echo pnpm
  elif [[ -f "$ROOT/web/package-lock.json" ]]; then echo npm
  elif [[ -f "$ROOT/web/yarn.lock" ]]; then echo yarn
  else echo ""
  fi
}

web_script_exists() { jq -e --arg s "$1" '.scripts[$s]' "$ROOT/web/package.json" >/dev/null 2>&1; }

web_step() { # web_step <check-name> <候选脚本名...>
  local name="$1"; shift
  step_begin "$name"
  if [[ ! -f "$ROOT/web/package.json" ]]; then
    step_end skip "web/ 未交付（前端由并行任务实现）；就绪后本检查自动生效"
    return
  fi
  if [[ ! -d "$ROOT/web/node_modules" ]]; then
    step_end skip "web/node_modules 不存在：本门禁不代跑 install（会改动前端工作状态），由前端任务自行安装后本检查自动生效"
    return
  fi
  local pm; pm="$(web_pm)"
  if [[ -z "$pm" ]]; then
    step_end skip "web/ 无 lockfile（pnpm-lock.yaml / package-lock.json / yarn.lock 全缺），无法判定包管理器"
    return
  fi
  if ! command -v "$pm" >/dev/null 2>&1; then
    step_end fail "web/ 由 $pm 管理（lockfile 判定）但本机无 $pm"
    return
  fi
  local script=""
  for cand in "$@"; do
    if web_script_exists "$cand"; then script="$cand"; break; fi
  done
  if [[ -z "$script" ]]; then
    step_end fail "web/package.json 无 $* 任一脚本（须在 web/package.json 提供该脚本）"
    return
  fi
  # npm/yarn 的 run 不隐式安装；pnpm 必须显式关掉 verify-deps-before-run 才不动 node_modules。
  local -a pm_env=()
  [[ "$pm" == "pnpm" ]] && pm_env+=(npm_config_verify_deps_before_run=false)
  if (cd "$ROOT/web" && env ${pm_env[@]+"${pm_env[@]}"} "$pm" run "$script") >"$STEP_LOG" 2>&1; then
    step_end pass "$pm run $script"
  else
    step_end fail "$pm run $script 失败（见 $(basename "$STEP_LOG")）"
  fi
}

web_step web-typecheck typecheck type-check
web_step web-unit test:unit test

# ── 摘要 + summary.json ────────────────────────────────────────────────────
counts_pass=0 counts_fail=0 counts_pending=0 counts_skip=0
for r in "${RESULTS[@]}"; do
  IFS='|' read -r _ st _ _ <<<"$r"
  case "$st" in
    pass) counts_pass=$((counts_pass + 1)) ;;
    fail) counts_fail=$((counts_fail + 1)) ;;
    pending) counts_pending=$((counts_pending + 1)) ;;
    skip) counts_skip=$((counts_skip + 1)) ;;
  esac
done

printf '\n== gates summary（机器可读）==\n'
for r in "${RESULTS[@]}"; do
  IFS='|' read -r name st detail secs <<<"$r"
  printf '%-14s %-8s %s\n' "$name" "$st" "$detail"
done
printf '%-14s %-8s pass=%d fail=%d pending=%d skip=%d\n' gate "$([[ $counts_fail -eq 0 ]] && echo PASS || echo FAIL)" \
  "$counts_pass" "$counts_fail" "$counts_pending" "$counts_skip"
if ((counts_pending > 0 || counts_skip > 0)); then
  printf '注意: pending/skip **不算 pass**（不算通过项），逐条原因见上表与 summary.json。\n'
fi

python3 - "$REPORT" "$ROOT" "$TS" "${RESULTS[@]}" <<'PY'
import json, sys, datetime
report, root, ts = sys.argv[1], sys.argv[2], sys.argv[3]
checks = []
for raw in sys.argv[4:]:
    name, status, detail, secs = raw.split("|", 3)
    checks.append({"name": name, "status": status, "detail": detail, "seconds": int(secs)})
counts = {k: sum(1 for c in checks if c["status"] == k) for k in ("pass", "fail", "pending", "skip")}
doc = {
    "suite": "gates",
    "startedAt": ts,
    "finishedAt": datetime.datetime.now().strftime("%Y%m%dT%H%M%S"),
    "root": root,
    "gate": "fail" if counts["fail"] else "pass",
    "counts": counts,
    "checks": checks,
    "note": "pending/skip 不算通过项；逐条原因见 checks[].detail",
}
with open(report + "/summary.json", "w", encoding="utf-8") as fh:
    json.dump(doc, fh, ensure_ascii=False, indent=2)
    fh.write("\n")
print("summary.json:", report + "/summary.json")
PY

if ((counts_fail > 0)); then
  printf 'gates: FAIL（%d 项失败）\n' "$counts_fail"
  exit 1
fi
printf 'gates: 无失败项（pass=%d pending=%d skip=%d）\n' "$counts_pass" "$counts_pending" "$counts_skip"
exit 0

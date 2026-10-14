#!/usr/bin/env bash
# =============================================================================
# test-mem-guard.sh —— 测试进程的"宿主内存守卫"（并发度 + 进程内存上限）
# =============================================================================
#
# 为什么存在（2026-09-23 一次整机假死事故）：
#   宿主直跑的 vitest 舰队没有任何内存约束——实测三个 worker 的 RSS 分别 16.5G / 7.8G /
#   5.2G，1 分钟内吃满 32G RAM，随后 32G swap 也被吃光（MemAvailable=47MB、loadavg 247、
#   PSI full 35%），整机用户态停滞：内核还在答 TCP 握手，但 sshd 发不出 SSH banner，
#   SSH 完全进不去，只能云 API 重启（三次同型事故）。
#   结论：**测试入口必须自己约束自己**——并发度按宿主可用内存推导，每个进程设堆上限，
#   能上 cgroup 就再套一层硬帽（超限杀测试树，而不是拖死宿主机）。
#
# 用法：
#   scripts/test-mem-guard.sh --print                     # 只打印决策（不执行）
#   scripts/test-mem-guard.sh -- vitest run               # 计算后执行命令（推荐）
#   scripts/test-mem-guard.sh --node-test src/*.test.js   # node --test 模式（自动加 --test-concurrency）
#
# 旋钮（环境变量，均有保守缺省）：
#   TEST_MEM_GUARD_CPU_FRACTION    并发 = nproc × 该比例（默认 0.6；8 核→4，16 核→9）
#   TEST_MEM_GUARD_MAX_WORKERS     并发的额外硬帽（默认 = nproc；一般不需要）
#   TEST_MEM_GUARD_PER_WORKER_MB   每个 worker 的 JS 堆上限（默认 2048）
#   TEST_MEM_GUARD_RESERVE_MB      给宿主机留的余量（默认 max(2048, 总内存 25%)）
#   TEST_MEM_GUARD_CGROUP          on|off（默认 off；on 时用 systemd-run 给单次运行套硬帽）
#   TEST_MEM_GUARD_CGROUP_SWAP_MB  该硬帽的 swap 上限（默认预算的 25%）
#
# 并发公式（CPU 优先，2026-09-23 用户裁决）：
#   workers = min( floor(nproc × CPU_FRACTION),           # 主要：留余量给系统，且随机器扩容自适应
#                  floor(内存预算 / 每 worker 预算),        # 次要：小内存机器保护
#                  MAX_WORKERS, nproc )
#
# 导出的变量（供 vitest.config.ts / package.json 脚本读取）：
#   TEST_MEM_GUARD_WORKERS        推导出的并发度
#   TEST_MEM_GUARD_WORKER_MB      每 worker 堆上限（MB）
#   NODE_OPTIONS                  追加 --max-old-space-size=<每 worker 堆上限>
#
# 退出码：与被执行命令一致；守卫自身参数/环境错误=2。
# =============================================================================
set -euo pipefail
export LC_ALL=C

say() { printf '[test-mem-guard] %s\n' "$*"; }
die() { printf '[test-mem-guard] 错误: %s\n' "$*" >&2; exit 2; }

PER_WORKER_MB=${TEST_MEM_GUARD_PER_WORKER_MB:-2048}
CPU_FRACTION=${TEST_MEM_GUARD_CPU_FRACTION:-0.6}
MAX_WORKERS_CFG=${TEST_MEM_GUARD_MAX_WORKERS:-}
RESERVE_MB_CFG=${TEST_MEM_GUARD_RESERVE_MB:-}
CGROUP_MODE=${TEST_MEM_GUARD_CGROUP:-off}
SWAP_MB_CFG=${TEST_MEM_GUARD_CGROUP_SWAP_MB:-}

MODE_PRINT=0
NODE_TEST=0
while (( $# )); do
    case $1 in
        --print)      MODE_PRINT=1 ;;
        --node-test)  NODE_TEST=1; shift; break ;;
        --)           shift; break ;;
        -h|--help)    sed -n '2,40p' "$0"; exit 0 ;;
        *)            break ;;
    esac
    shift
done

# ---------------------------------------------------------- 宿主内存探测
# Linux：MemAvailable 是内核给出的"新分配可用量"口径，最贴近我们的需求。
# macOS：vm_stat 的 free+inactive+purgeable+speculative 页 × 页大小（近似口径，
#        比 free 更接近 MemAvailable；本机只作开发机，取保守值即可）。
host_mem_mb() {
    if [[ -r /proc/meminfo ]]; then
        awk '/^MemTotal:/{t=$2} /^MemAvailable:/{a=$2} END{printf "%d %d\n", t/1024, a/1024}' /proc/meminfo
        return
    fi
    local total_bytes pagesize pagecnt free inactive purgeable speculative
    total_bytes=$(sysctl -n hw.memsize 2>/dev/null || echo 0)
    pagesize=$(sysctl -n hw.pagesize 2>/dev/null || echo 4096)
    local vmstat_out
    vmstat_out=$(vm_stat 2>/dev/null || true)
    # vm_stat 行形如 "Pages free:  1244662."（键含空格，故按 $NF 取数字、去尾点）。
    pagecnt() { awk -v k="$1" 'index($0, k) == 1 { n=$NF; gsub(/[^0-9]/, "", n); print n+0 }' <<<"$vmstat_out"; }
    free=$(pagecnt "Pages free")
    inactive=$(pagecnt "Pages inactive")
    purgeable=$(pagecnt "Pages purgeable")
    speculative=$(pagecnt "Pages speculative")
    printf '%d %d\n' \
        "$(( total_bytes / 1024 / 1024 ))" \
        "$(( (free + inactive + purgeable + speculative) * pagesize / 1024 / 1024 ))"
}

read -r TOTAL_MB AVAIL_MB <<<"$(host_mem_mb)"
(( TOTAL_MB > 0 )) || die '无法读取宿主内存'

if [[ -n $RESERVE_MB_CFG ]]; then
    RESERVE_MB=$RESERVE_MB_CFG
else
    # 固定 4G：关键侧预留的职责已上移到主机（system.slice MemoryMin/MemoryLow，
    # 见宿主 systemd 配置），守卫这里只需覆盖"内核 + 无界组"的少量料，
    # 不必再按 25% 过度保守（那会把 8 核机器的并发压到远低于用户裁决的 nproc×0.6）。
    RESERVE_MB=4096
fi

BUDGET_MB=$(( AVAIL_MB - RESERVE_MB ))
if (( BUDGET_MB < 512 )); then
    # 可用内存已经贴近余量线：只允许 1 个 worker + 小堆，并且明确告警——
    # 此时继续开并发就是在赌宿主机，宁可测试慢/失败也不要再来一次假死。
    say "警告: 可用内存仅 ${AVAIL_MB}MB（余量线 ${RESERVE_MB}MB），降到 1 worker 且堆限 512MB"
    BUDGET_MB=512
    PER_WORKER_MB=512
fi

NPROC=$(getconf _NPROCESSORS_ONLN 2>/dev/null || nproc 2>/dev/null || echo 2)
MAX_WORKERS=${MAX_WORKERS_CFG:-$NPROC}
# CPU 优先（用户 2026-09-23 裁决 nproc×0.6）：留余量给系统、且随机器扩容自适应；
# 内存预算作次要约束（小内存机器保护）。二者取小，再套硬帽与 nproc。
CPU_WORKERS=$(awk -v n="$NPROC" -v f="$CPU_FRACTION" 'BEGIN{w=int(n*f); if (w<1) w=1; print w}')
MEM_WORKERS=$(( BUDGET_MB / PER_WORKER_MB ))
(( MEM_WORKERS < 1 )) && MEM_WORKERS=1
WORKERS=$CPU_WORKERS
(( MEM_WORKERS < WORKERS )) && WORKERS=$MEM_WORKERS
(( MAX_WORKERS < WORKERS )) && WORKERS=$MAX_WORKERS
(( WORKERS < 1 )) && WORKERS=1

# --------------------------------------------------- 进程内堆上限（NODE_OPTIONS）
# 注意：堆上限不等于 RSS 上限（typed array / Buffer / jsdom native 侧可超），
# 所以它只是第一道闸；真正的硬帽是下面的 cgroup。
if [[ ${NODE_OPTIONS:-} == *--max-old-space-size* ]]; then
    say "NODE_OPTIONS 已有 --max-old-space-size，尊重调用方设置"
else
    export NODE_OPTIONS="${NODE_OPTIONS:-} --max-old-space-size=${PER_WORKER_MB}"
fi
export TEST_MEM_GUARD_WORKERS=$WORKERS
export TEST_MEM_GUARD_WORKER_MB=$PER_WORKER_MB

# ------------------------------------------------------------- cgroup 硬帽
use_cgroup=0
case $CGROUP_MODE in
    on)  use_cgroup=1 ;;
    off) use_cgroup=0 ;;
    auto) [[ $(id -u) -eq 0 ]] && command -v systemd-run >/dev/null 2>&1 && use_cgroup=1 || use_cgroup=0 ;;
    *)   die "TEST_MEM_GUARD_CGROUP 只接受 auto|on|off" ;;
esac
if (( use_cgroup )); then
    # 注意：不能用 `systemctl is-system-running` 的退出码——系统处于 degraded（有任意
    # 失败单元，如 fwupd-refresh）时它返回非零，但 scope 完全可用。
    # 2026-09-23 实测踩过：degraded 被误判成"无 systemd"而静默关掉硬帽。
    sys_state=$(systemctl is-system-running 2>/dev/null || true)
    case $sys_state in
        running|degraded) ;;
        *) use_cgroup=0 ;;
    esac
fi

if [[ -n $SWAP_MB_CFG ]]; then SWAP_MB=$SWAP_MB_CFG; else SWAP_MB=$(( BUDGET_MB / 4 )); fi

say "宿主 total=${TOTAL_MB}MB avail=${AVAIL_MB}MB reserve=${RESERVE_MB}MB → budget=${BUDGET_MB}MB"
if (( use_cgroup )); then
    say "并发=${WORKERS}（≤nproc ${NPROC}，≤cap ${MAX_WORKERS}）；每 worker 堆=${PER_WORKER_MB}MB；cgroup=on（MemoryMax=${BUDGET_MB}M MemorySwapMax=${SWAP_MB}M）"
else
    say "并发=${WORKERS}（≤nproc ${NPROC}，≤cap ${MAX_WORKERS}）；每 worker 堆=${PER_WORKER_MB}MB；cgroup=off（非 root/无 systemd，仅堆上限保护）"
fi

(( MODE_PRINT == 1 )) && exit 0
(( $# > 0 )) || die '没有要执行的命令（用法：test-mem-guard.sh -- <cmd...>）'

if (( NODE_TEST )); then
    # node --test：并发用 --test-concurrency，堆上限经 NODE_OPTIONS 传给各测试子进程。
    set -- node --test --test-concurrency="$WORKERS" "$@"
elif [[ ${1:-} == vitest || ${1:-} == */vitest || ${1:-} == vitest.mjs ]]; then
    # vitest：并发用 --maxWorkers（CLI 优先于 vitest.config 里的 maxWorkers）。
    # 已经显式给过并发开关的调用方不被覆盖（尊重显式意图，但仍受堆上限与 cgroup 约束）。
    already=0
    for a in "$@"; do
        case $a in
            --maxWorkers*|--max-workers*|--no-file-parallelism|--fileParallelism=false) already=1 ;;
        esac
    done
    if (( already == 0 )); then
        set -- "$@" "--maxWorkers=${WORKERS}"
    fi
fi

if (( use_cgroup )); then
    # 必须显式 --slice=：systemd-run 的缺省落点是 **system.slice**（man 明载）——那会把
    # 测试放进入被保护的系统片里（既与 sshd/journald 争片内资源，也不受批量池的舱壁约束）。
    # 2026-09-23 独立校验发现此缺陷并修复。
    exec systemd-run --scope --quiet --collect \
        --slice=yzagmgr-test-guard.slice \
        -p "MemoryMax=${BUDGET_MB}M" -p "MemorySwapMax=${SWAP_MB}M" \
        -p "MemoryHigh=$(( BUDGET_MB * 3 / 4 ))M" \
        -- "$@"
fi
exec "$@"

// 本文件是「查询耗时埋点」（UC-049 的第三块：查询延时相关参数）。
//
// 口径（冻结）：per 操作名 summary/trends/breakdown/requests/detail/pricing/maintenance
// 的 {count, avgMs, p95Ms, lastMs}，基于**最近 N=256 次调用**的环形样本；
// 从未调用过的操作是 null（不是 0——0 会被读成「很快」）。样本是**进程内**的：
// 重启归零，不是持久化历史（界面对此如实标注）。
//
// 埋点范围就是 Store 上对应的那七个方法（在方法入口 defer 一次 observe）；
// 失败调用同样计数（耗时统计问的是「这个操作花多久」，与成败无关）。
package usage

import (
	"math"
	"sort"
	"sync"
	"time"
)

// opStatRingSize 是每个操作的环形样本容量（冻结值 N=256）。
const opStatRingSize = 256

// opName 是埋点的操作名（冻结集合，与 GET /system/info 的 queryStats 键一一对应）。
type opName string

const (
	opSummary     opName = "summary"
	opTrends      opName = "trends"
	opBreakdown   opName = "breakdown"
	opRequests    opName = "requests"
	opDetail      opName = "detail"
	opPricing     opName = "pricing"
	opMaintenance opName = "maintenance"
)

// OpStat 是单个操作的耗时统计。avgMs/p95Ms/lastMs 是毫秒（三位小数，微秒精度）。
type OpStat struct {
	// Count 是进程内的累计调用次数（单调递增，不受环形容量影响）。
	Count int64 `json:"count"`
	// AvgMs 是最近样本的算术平均；P95Ms 是最近样本的 nearest-rank P95
	// （升序第 ⌈0.95n⌉ 个，n = 已填样本数）；LastMs 是最近一次调用的耗时。
	AvgMs  float64 `json:"avgMs"`
	P95Ms  float64 `json:"p95Ms"`
	LastMs float64 `json:"lastMs"`
}

// QueryStats 是全部操作的耗时统计快照。
// 七个键恒在场；没有样本的操作是 null（JSON null）。
//
// 注意与 `Store.Stats`（窗口聚合统计，stats.go）无关：那是「窗口内发生了什么」，
// 这里是「查这个窗口花了多久」。
type QueryStats struct {
	Summary     *OpStat `json:"summary"`
	Trends      *OpStat `json:"trends"`
	Breakdown   *OpStat `json:"breakdown"`
	Requests    *OpStat `json:"requests"`
	Detail      *OpStat `json:"detail"`
	Pricing     *OpStat `json:"pricing"`
	Maintenance *OpStat `json:"maintenance"`
}

// opStatRing 是单个操作的环形样本累加器。
type opStatRing struct {
	count  int64
	last   time.Duration
	ring   [opStatRingSize]time.Duration
	next   int // 下一个写入位置
	filled int // 已填入的样本数（≤ opStatRingSize）
}

func (r *opStatRing) observe(d time.Duration) {
	r.count++
	r.last = d
	r.ring[r.next] = d
	r.next = (r.next + 1) % opStatRingSize
	if r.filled < opStatRingSize {
		r.filled++
	}
}

// stat 快照当前统计；没有样本（count == 0）时返回 nil。
// 调用方必须持有 opMeter 的锁：本函数无锁读环形样本（count/last/ring[:filled]），
// 与并发 observe 直接构成数据竞争（见 snapshot 的锁纪律说明）。
func (r *opStatRing) stat() *OpStat {
	if r == nil || r.count == 0 {
		return nil
	}
	samples := r.ring[:r.filled]
	var sum time.Duration
	for _, d := range samples {
		sum += d
	}
	avg := sum / time.Duration(len(samples))

	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	// nearest-rank，与 §14/UC-047 的 P95 口径同源：⌈95n/100⌉（整数算术，无浮点）。
	rank := (95*int64(len(sorted)) + 99) / 100
	p95 := sorted[rank-1]

	return &OpStat{
		Count:  r.count,
		AvgMs:  durationMs(avg),
		P95Ms:  durationMs(p95),
		LastMs: durationMs(r.last),
	}
}

// durationMs 把时长渲染成毫秒（三位小数）。
func durationMs(d time.Duration) float64 {
	return math.Round(float64(d.Nanoseconds())/1e3) / 1e3
}

// opMeter 是全部操作的埋点集合。零值不可用，用 newOpMeter 构造。
type opMeter struct {
	mu    sync.Mutex
	rings map[opName]*opStatRing
}

func newOpMeter() *opMeter {
	return &opMeter{rings: map[opName]*opStatRing{}}
}

// observeSince 记录一次从 start 起算的调用（nil 接收者安全：Store 一定持有它，
// 但零值 Store 的路径不该炸）。
func (m *opMeter) observeSince(op opName, start time.Time) {
	if m == nil {
		return
	}
	m.observe(op, time.Since(start))
}

// observe 记录一次已知耗时的调用。
func (m *opMeter) observe(op opName, d time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ring := m.rings[op]
	if ring == nil {
		ring = &opStatRing{}
		m.rings[op] = ring
	}
	ring.observe(d)
}

// snapshot 取出全部操作的统计快照（固定七键，无样本为 nil）。
//
// 锁纪律（P5 修复）：读全程持 meter 锁——环形样本的读（本函数）与写（observe）
// 必须在同一把锁下。旧实现在锁内取 ring 指针、锁外再 ring.stat() 读样本，
// 与并发 observe 构成数据竞争（-race 可复现），还可能读到 count 与样本撕裂的组合。
// 七个键取自同一临界区，快照内部彼此一致。
func (m *opMeter) snapshot() QueryStats {
	if m == nil {
		return QueryStats{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return QueryStats{
		Summary:     m.rings[opSummary].stat(),
		Trends:      m.rings[opTrends].stat(),
		Breakdown:   m.rings[opBreakdown].stat(),
		Requests:    m.rings[opRequests].stat(),
		Detail:      m.rings[opDetail].stat(),
		Pricing:     m.rings[opPricing].stat(),
		Maintenance: m.rings[opMaintenance].stat(),
	}
}

// QueryStats 返回进程内的查询耗时统计快照（纯内存读，不访问库）。
func (s *Store) QueryStats() QueryStats { return s.queryMeter.snapshot() }

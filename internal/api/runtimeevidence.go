// §4.8 运行时证据与受控验证（UC-092 A9/A10/A11 additive，2026-09-27）。
//
// 本文件回答一个问题：**「已验证生效」这句话，能被执行证据支持到什么范围？**
//
// 证据链（A9 的五要素 + 获得时间/适用实例）在这里被拆成三层，缺一层就不升级：
//  1. 配置版本：应用事务身份（appliedTx）+ 盘上受管文件逐文件 sha256 + 脱敏决定键摘要；
//  2. 受控工作区：把**盘上字节**（经数据根指针改写）放进一次性临时根，注入唯一标记
//     （档名后缀 `-agv-<nonce>`；C 路模型映射已删，出站名 = 实发名），
//     用该工具的 CLI 在其中发起**一次**最小请求；
//  3. 可观测路径：本地代理把这次请求记进用量库；只有「app = 该工具、前缀 ∈ 该工具车道、
//     请求模型含唯一标记、状态 2xx」的那条记录才升级为已验证。
//
// 三条纪律（与硬边界一一对应）：
//   - 写盘成功、进程存在、版本命令成功、单独 curl 网关都**不**构成本文件的证据——它们
//     都到不了第 3 层；旁路网关（直连）形态没有本地观测点，如实报「不可观测」，折成
//     成功是伪造。网关健康检查同样不参与判定。
//   - 失效在**读取时**判定（外改 = 盘上指纹变化；再次应用 = 事务身份变化；端点
//     origin 变化），不靠后台计时器、不靠客户端记忆（多窗口/刷新
//     同源）。「已重启」不由本文件声称——没有证据就保持未确认。
//   - 受控验证受既有网关额度与用户批准上限约束：额度不足/不明、报价不明、上界越上限、
//     配置版本漂移——一律**拒绝发起**（0 次模型调用）；失败/超时只记尝试与机器可读原因，
//     **不自动重发**、不自动结束或重启用户的工具。本文件不新增任何钱包扣款路径：
//     不调用任何计费接口，只用既有推理额度面做**读取判定**。
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/family/claude"
	"github.com/peterplaykkk/yzagmgr/internal/proxy"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

// ── 常量与词表 ──────────────────────────────────────────────────────────────

const (
	// runtimeEvidenceSchema 是证据记账的盘上 schema。
	runtimeEvidenceSchema = 1
	// runtimeVerifyDirPrefix 是受控工作区临时根的目录前缀（崩溃残留清理只认它）。
	runtimeVerifyDirPrefix = "yzagmgr-runtime-verify-"
	// runtimeVerifyMarkerPrefix 是唯一标记的前缀（`agv-` + 12 hex）。
	runtimeVerifyMarkerPrefix = "agv-"
	// runtimeVerifyMarkerKey 是承载标记的档名键（与 §A 行 1 的客户端档名键同源；
	// 只标客户端档名，**不标** DEFAULT_* 键——后者是映射目标，标了会让上游名变形）。
	runtimeVerifyMarkerKey = claudeModelKey
	// runtimeVerifyInputBoundTokens 是费用上界的输入 token 声明上界（含 CLI 系统提示）。
	runtimeVerifyInputBoundTokens = 65536
	// runtimeVerifyOutputBoundTokens 是策略条目缺席时的输出 token 上界（保守声明值）。
	runtimeVerifyOutputBoundTokens = 65536
	// runtimeVerifyEstimateInputTokens / OutputTokens 是「预计费用」的典型值（最小请求）。
	runtimeVerifyEstimateInputTokens  = 2048
	runtimeVerifyEstimateOutputTokens = 512
	// runtimeVerifyDefaultCapUSD 是不带 maxCostUsd 时的批准上限（用户可在请求里调低，
	// 调高不得超过 runtimeVerifyMaxCapUSD）。
	runtimeVerifyDefaultCapUSD = 0.50
	// runtimeVerifyMaxCapUSD 是单次受控验证允许的批准上限天花板。
	runtimeVerifyMaxCapUSD = 1.00
	// runtimeVerifyAttemptKeep 是每工具保留的尝试条数（追加式；「首次失败留存」靠它
	// 只裁最旧的尾巴，最新 N 条永不静默丢失）。
	runtimeVerifyAttemptKeep = 20
	// runtimeVerifyObservationWait 是执行成功后读观测记录前的**一次**有界等待
	// （代理在响应流结束后才提交记录，这里补写可见性窗口；等待本身不构成任何证据）。
	runtimeVerifyObservationWait = 600 * time.Millisecond
)

// 机器可读原因词表（封闭集合）。GET 的状态与受控验证的尝试结果共用同一套取值，
// 消费方（web）据此渲染，不再从文案里猜语义。
const (
	reasonVerified           = "verified"            // 已获证据且读取时复核通过
	reasonNeverVerified      = "never_verified"      // 从未取得过证据
	reasonUnsupportedTool    = "unsupported_tool"    // 本工具不支持受控验证（无配方/无标记载体/无车道）
	reasonNotObservable      = "not_observable"      // 当前配置形态不经本地可观测路径（直连网关/官方/router 形态）
	reasonConfigNotApplied   = "config_not_applied"  // 该工具当前没有已应用的档（无从验证）
	reasonConfigChanged      = "config_changed"      // 盘上指纹/配置版本与证据不符（外改、恢复/重置、预览后漂移）
	reasonReapplied          = "reapplied"           // 再次应用（事务身份变化）
	reasonMappingChanged     = "mapping_changed"     // 模型/端点映射变化（声明映射或上游 origin 重算不一致）
	reasonQuotaExhausted     = "quota_exhausted"     // 额度已用尽：拒绝发起（0 次调用）
	reasonQuotaUnknown       = "quota_unknown"       // 额度无法确定：拒绝发起（0 次调用）
	reasonQuoteUnknown       = "quote_unknown"       // 报价未知：拒绝发起（0 次调用，不伪装免费）
	reasonBudgetExceeded     = "budget_exceeded"     // 费用上界越过批准上限：拒绝发起（0 次调用）
	reasonMarkerUnresolvable = "marker_unresolvable" // 标记无法安全注入（无档名键/映射解析不一致）
	reasonInProgress         = "in_progress"         // 同一工具上已有受控运行在跑
	reasonWorkspaceFailed    = "workspace_failed"    // 受控工作区建立失败（未发起任何调用）
	reasonVerifyFailed       = "verify_failed"       // 受控运行失败（已发出 1 次调用；不自动重发）
	reasonVerifyTimeout      = "verify_timeout"      // 受控运行超时（已发出 1 次调用；不自动重发）
	reasonObservationMissing = "observation_missing" // 响应丢失/观测缺失：没有可关联记录（未确认，不重发）
	reasonUpstreamRejected   = "upstream_rejected"   // 已关联但上游非 2xx：链路被拒，不升级
	reasonNotSelf            = "not_self"            // 候选记录属于另一工具（负向矩阵）
	reasonPathUnobservable   = "path_unobservable"   // 候选记录不经该工具车道（旁路/错路径）
	reasonMarkerMissing      = "marker_missing"      // 候选记录不带唯一标记（旧进程/旧配置版本的相同模型请求）
	reasonMappingMismatch    = "mapping_mismatch"    // 候选记录映射后上游与声明不符
)

// ── 证据记账（盘上） ────────────────────────────────────────────────────────

// runtimeEvidenceStore 是运行时证据与尝试历史的持久化记账（managed/runtime-evidence.json）。
type runtimeEvidenceStore struct {
	mu   sync.Mutex
	path string
	doc  runtimeEvidenceDoc
}

type runtimeEvidenceDoc struct {
	Schema int                               `json:"schema"`
	Tools  map[string]toolRuntimeEvidenceDoc `json:"tools"`
}

type toolRuntimeEvidenceDoc struct {
	// Evidence 是最近一次**校验通过**的证据（读取时还要过一遍失效判定才会呈现）。
	Evidence *runtimeEvidenceRecord `json:"evidence,omitempty"`
	// Attempts 是追加式尝试历史（最新在尾；只裁最旧的尾巴）。
	Attempts []runtimeVerifyAttempt `json:"attempts,omitempty"`
}

// runtimeVerifyAttempt 是一条尝试记录。modelCalls 是**已发出的模型调用数**：
// 闸门拒绝 = 0（未发起），执行过 = 1（不自动重发 ⇒ 恒 ≤1）。
type runtimeVerifyAttempt struct {
	At         string `json:"at"`
	Outcome    string `json:"outcome"` // verified | unconfirmed
	Reason     string `json:"reason"`
	ModelCalls int    `json:"modelCalls"`
}

// runtimeEvidenceRecord 是「已验证生效」的全部携带物（A9 五要素 + 获得时间/适用实例）。
type runtimeEvidenceRecord struct {
	ToolID string `json:"toolId"`
	// Source 恒为 controlled：本文件的证据只来自受控验证（被动观测没有唯一标记，
	// 不足以把「旧进程/旧配置版本的相同模型请求」排除掉，因此不升级为已验证）。
	Source string `json:"source"`
	// ObtainedAt 是证据获得时间。
	ObtainedAt string                   `json:"obtainedAt"`
	Instance   runtimeEvidenceInstance  `json:"instance"`
	Config     runtimeEvidenceConfig    `json:"configVersion"`
	Workspace  runtimeEvidenceWorkspace `json:"workspace"`
	Request    runtimeEvidenceRequest   `json:"request"`
	Outbound   runtimeEvidenceOutbound  `json:"outbound"`
	// Marker 是本次验证注入的唯一标记（`agv-<nonce>`）。
	Marker string `json:"marker"`
	// AppliesTo 是适用实例/范围：证据只证明这个工具、这个车道前缀、这一份生效档。
	AppliesTo runtimeEvidenceScopeRef `json:"appliesTo"`
	// RuleVersion 是**证据关联请求**开始时刻的生效请求映射规则集内容指纹
	// （UC-096 A6 additive；与 §7.1/RequestLog.ruleVersion 同一概念）。它从被关联的
	// 用量记录**照抄**（受控验证只复核不重算），因此与那条请求的记账逐字一致；
	// 空/缺席 = 该请求不在 /router 车道或来自旧版本（界面按「未知」如实呈现，
	// 不折成任何具体值、不回填）。
	RuleVersion string `json:"ruleVersion,omitempty"`
}

// runtimeEvidenceInstance 是第 1 要素：目标工具实例/进程身份（受控运行的那一个）。
type runtimeEvidenceInstance struct {
	ToolID string `json:"toolId"`
	Kind   string `json:"kind"` // 恒为 controlled-run
	CLI    string `json:"cli"`
	// PID 为 0 表示进程身份不可得（平台限制/桩实现）——如实报 0，不编。
	PID        int    `json:"pid"`
	StartedAt  string `json:"startedAt"`
	FinishedAt string `json:"finishedAt"`
}

// runtimeEvidenceFile 是「路径 + 内容 sha256」对（不携带任何字节内容）。
type runtimeEvidenceFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// runtimeEvidenceConfig 是第 2 要素：配置版本（应用指纹 + 脱敏决定键摘要）。
type runtimeEvidenceConfig struct {
	// ApplyTx / AppliedAt 是应用事务身份（UC-092 A10 的「再次应用」判据）。
	ApplyTx   string `json:"applyTx"`
	AppliedAt string `json:"appliedAt"`
	ProfileID string `json:"profileId"`
	// Files 是应用指纹：盘上受管文件在验证时的逐文件 sha256。
	Files []runtimeEvidenceFile `json:"files"`
	// DecideKey 是脱敏决定键摘要：键名 + 掩码后的值 + 对**掩码值**的 sha256
	// （摘要不含任何明文材料）。
	DecideKey runtimeEvidenceDecideKey `json:"decideKey"`
}

type runtimeEvidenceDecideKey struct {
	Key         string `json:"key"`
	MaskedValue string `json:"maskedValue"`
	Digest      string `json:"digest"`
	Path        string `json:"path"`
}

// runtimeEvidenceWorkspace 是第 3 要素：受控工作区/配置位置（运行后即删；这里记当时的位置与内容指纹）。
type runtimeEvidenceWorkspace struct {
	// Root 是受控工作区根（临时目录；本次受控运行结束即删除）。
	Root string `json:"root"`
	// ConfigPath 是 CLI 实际读取配置的位置（工作区内的对应路径）。
	ConfigPath string `json:"configPath"`
	// Files 是工作区内写入的文件（相对根 + 写入字节的 sha256）。
	Files []runtimeEvidenceFile `json:"files"`
}

// runtimeEvidenceRequest 是第 4 要素：应用后的请求身份与时间（可观测路径上的那条记录）。
type runtimeEvidenceRequest struct {
	RecordID     string `json:"recordId"`
	At           string `json:"at"`
	Status       int    `json:"status"`
	App          string `json:"app"`
	Prefix       string `json:"prefix"`
	DurationMs   int64  `json:"durationMs"`
	InputTokens  int64  `json:"inputTokens"`
	OutputTokens int64  `json:"outputTokens"`
}

// runtimeEvidenceOutbound 是第 5 要素：实际出站模型与端点。
// （声明映射字段已随 C 路模型映射删除——design §2.2，旧中继退役。）
type runtimeEvidenceOutbound struct {
	// ModelRequested 是客户端实发的请求模型名（含唯一标记）；ModelUpstream 是
	// 实际出站的上游模型名。两者不得混为一谈。
	ModelRequested string `json:"modelRequested"`
	ModelUpstream  string `json:"modelUpstream"`
	Endpoint       struct {
		// ClientBaseURL 是配置里写死/指向的基址（脱敏）。
		ClientBaseURL  string `json:"clientBaseUrl"`
		ObservedPrefix string `json:"observedPrefix"`
		UpstreamOrigin string `json:"upstreamOrigin"`
	} `json:"endpoint"`
}

// runtimeEvidenceScopeRef 是证据的适用范围。
type runtimeEvidenceScopeRef struct {
	ToolID    string   `json:"toolId"`
	ProfileID string   `json:"profileId"`
	Prefixes  []string `json:"prefixes"`
}

// newRuntimeEvidenceStore 打开（或初始化）证据记账。
func newRuntimeEvidenceStore(env configEnv) (*runtimeEvidenceStore, error) {
	path := filepath.Join(env.ManagedDir(), "runtime-evidence.json")
	s := &runtimeEvidenceStore{path: path, doc: runtimeEvidenceDoc{Schema: runtimeEvidenceSchema, Tools: map[string]toolRuntimeEvidenceDoc{}}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *runtimeEvidenceStore) load() error {
	blob, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return apierr.Wrap(apierr.CodeStorage, err, "读取运行时证据记账失败")
	}
	var doc runtimeEvidenceDoc
	if err := json.Unmarshal(blob, &doc); err != nil || doc.Schema != runtimeEvidenceSchema {
		// 记账损坏不阻塞启动：按空记账处理（判定不依赖历史证据，只影响呈现）。
		return nil
	}
	if doc.Tools == nil {
		doc.Tools = map[string]toolRuntimeEvidenceDoc{}
	}
	s.doc = doc
	return nil
}

// saveLocked 原子落盘指定文档（与 profiles.json 同一纪律：临时文件 + 0600 + rename）。
//
// 只写盘、**不碰内存态**：调用方在写盘成功后自行提交 s.doc（落盘纪律见 putEvidence）。
// 失败路径不留半写文件：临时文件无论成败都由 defer 清掉，目标文件只经 rename 整体替换。
func (s *runtimeEvidenceStore) saveLocked(doc runtimeEvidenceDoc) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "建立运行时证据目录失败")
	}
	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "编码运行时证据失败")
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".runtime-evidence-*.json")
	if err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "创建临时文件失败")
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(append(blob, '\n')); err != nil {
		_ = tmp.Close()
		return apierr.Wrap(apierr.CodeStorage, err, "写入运行时证据失败")
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return apierr.Wrap(apierr.CodeStorage, err, "设置运行时证据权限失败")
	}
	if err := tmp.Close(); err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "关闭临时文件失败")
	}
	if err := os.Rename(name, s.path); err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "替换运行时证据失败")
	}
	return nil
}

// clone 深拷贝记账文档：每个工具的尝试切片都不与旧内存态共享底层数组
// （证据记录只被整体替换、从不原地改写，按指针共享即可）。
func (d runtimeEvidenceDoc) clone() runtimeEvidenceDoc {
	next := runtimeEvidenceDoc{Schema: d.Schema, Tools: make(map[string]toolRuntimeEvidenceDoc, len(d.Tools))}
	for id, rec := range d.Tools {
		rec.Attempts = append([]runtimeVerifyAttempt(nil), rec.Attempts...)
		next.Tools[id] = rec
	}
	return next
}

// tool 读某工具的证据副本（调用方不必持锁）。
func (s *runtimeEvidenceStore) tool(id string) toolRuntimeEvidenceDoc {
	if s == nil {
		return toolRuntimeEvidenceDoc{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.doc.Tools[id]
	rec.Attempts = append([]runtimeVerifyAttempt(nil), rec.Attempts...)
	return rec
}

// putEvidence 记下一条证据（覆盖上一条；尝试历史不动）。
//
// 落盘纪律（F1）：**先落盘、成功后才提交内存**。写盘失败（磁盘满/只读/权限/rename
// 失败）时内存保持旧值 ⇒ 同进程 GET 与盘上真值恒同源；否则同进程会宣称一份盘上并不
// 存在的「已验证」，重启即消失——那等于在内存里把一次写失败升级成成功，违反本文件
// 「证据写盘失败 ⇒ 不宣称已验证」的双向纪律与 §4.8「GET 回读与执行响应同源」。
func (s *runtimeEvidenceStore) putEvidence(id string, ev runtimeEvidenceRecord) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.doc.clone()
	rec := next.Tools[id]
	rec.Evidence = &ev
	next.Tools[id] = rec
	if err := s.saveLocked(next); err != nil {
		return err // 内存不提交：读取面继续以盘上真值为源。
	}
	s.doc = next
	return nil
}

// putAttempt 追加一条尝试（只裁最旧的尾巴，保留上限见 runtimeVerifyAttemptKeep）。
// 落盘纪律与 putEvidence 同口径：写盘失败时内存不提交（不留一枚盘上不存在的幽灵尝试）。
func (s *runtimeEvidenceStore) putAttempt(id string, at runtimeVerifyAttempt) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.doc.clone()
	rec := next.Tools[id]
	rec.Attempts = append(rec.Attempts, at)
	if len(rec.Attempts) > runtimeVerifyAttemptKeep {
		rec.Attempts = append([]runtimeVerifyAttempt(nil), rec.Attempts[len(rec.Attempts)-runtimeVerifyAttemptKeep:]...)
	}
	next.Tools[id] = rec
	if err := s.saveLocked(next); err != nil {
		return err
	}
	s.doc = next
	return nil
}

// ── 能力与支持面 ────────────────────────────────────────────────────────────

// runtimeVerifySupported 报告某工具是否支持受控验证，以及不支持时的原因。
//
// 支持集刻意只有一个工具（claude，2026-09-27）：受控验证要求「CLI 配方 + 唯一标记的
// 安全载体（档名键 + 非恒等映射）+ 本地可观测车道」三件同时在场。claude 是唯一满足的
// 家族；其余家族如实报 unsupported_tool（不是「暂时不能」，而是没有可执行的检测入口）。
func runtimeVerifySupported(id family.ID) (bool, string) {
	if _, ok := profileTestRecipes[id]; !ok {
		return false, reasonUnsupportedTool
	}
	if id != family.Claude {
		// 其余家族没有已实证的「标记 → 映射后上游不变」的载体（映射恒等或有不可控变形），
		// 注入标记会让请求以标记名出网（网关 400），不猜配方。
		return false, reasonUnsupportedTool
	}
	// 九前缀车道登记表已随 C 路删除（design §2.4）：观测前缀直接取家族写链的本地
	// route 后缀（见 runtimeVerifyLanePrefixes），有 recipe 且为 claude 即受支持。
	return true, ""
}

// runtimeVerifyLanePrefixes 给出该工具的本地车道前缀（字符串形）。
//
// 车道表（proxy.LaneFor）已随 C 路删除；盘上决定键形态由家族写链钉住——claude 的
// 网关档恒写 `{LocalRoute}/claude`（claude.LocalRouteSuffix），前缀与之同源取值，
// 观测关联（correlateRuntimeObservation）与决定键判定（runtimeRequestRoute）共用。
func runtimeVerifyLanePrefixes(id family.ID) []string {
	if id == family.Claude {
		return []string{strings.TrimPrefix(claude.LocalRouteSuffix, "/")}
	}
	return nil
}

// ── 事实读取（受控验证的计划面；零副作用、零调用） ──────────────────────────

// runtimeVerifyFacts 是一次受控验证的全部可读事实（计划与闸门共用）。
type runtimeVerifyFacts struct {
	ToolID    family.ID
	ProfileID string
	Prefixes  []string

	Plans      []family.FilePlan
	ConfigPath string
	// TierModel 是声明档名（未标记）；MarkedModel 是注入唯一标记后的档名；
	// ExpectedUpstream 是计价目标（C 路映射已删：出站名 = 档名，报价按未标记档名查）。
	TierModel        string
	MarkedModel      string
	ExpectedUpstream string

	// RoutePrefix 是请求将进入的本地前缀；UpstreamOrigin 是该前缀的出网 origin。
	RoutePrefix    string
	UpstreamOrigin string

	DecideKey      string
	DecideMasked   string
	DecideDigest   string
	ClientBaseURL  string
	ConfigFiles    []runtimeEvidenceFile
	ApplyTx        string
	AppliedAt      string
	LocalRouteBase string

	OutputBoundTokens int
	InPerMillion      float64
	OutPerMillion     float64
	Currency          string
	QuoteKnown        bool

	Blockers []runtimeVerifyBlocker
}

// runtimeVerifyBlocker 是一条机器可读的阻塞（reason 取自封闭词表）。
type runtimeVerifyBlocker struct {
	Reason  string         `json:"reason"`
	Message string         `json:"message"`
	Detail  map[string]any `json:"detail,omitempty"`
}

// identity 是「配置版本 + 受控运行输入」的指纹：预览 → 执行之间发生任何漂移都要拒发。
func (f runtimeVerifyFacts) identity() string {
	var b strings.Builder
	b.WriteString(f.ApplyTx)
	b.WriteString("\x00")
	b.WriteString(f.ProfileID)
	b.WriteString("\x00")
	for _, file := range f.ConfigFiles {
		b.WriteString(file.Path)
		b.WriteString("=")
		b.WriteString(file.SHA256)
		b.WriteString("\x00")
	}
	b.WriteString(f.MarkedModel)
	b.WriteString("\x00")
	b.WriteString(f.ExpectedUpstream)
	return tx.HashBytes([]byte(b.String()))
}

// runtimeVerifyFactsFor 读一遍全部事实。参数 marker 是本次验证的唯一标记。
//
// 读取面刻意全是只读操作（探针/盘上哈希/策略/代理路由表）：本函数不做任何写盘，
// 也不发起任何模型调用与网络请求（额度面读的是**本地缓存**，见 runtimeVerifyQuotaNow）。
func (s *Server) runtimeVerifyFactsFor(ctx context.Context, id family.ID, marker string, capUSD float64) (runtimeVerifyFacts, error) {
	facts := runtimeVerifyFacts{ToolID: id, Prefixes: runtimeVerifyLanePrefixes(id)}

	if ok, reason := runtimeVerifySupported(id); !ok {
		facts.Blockers = append(facts.Blockers, runtimeVerifyBlocker{
			Reason: reason, Message: string(id) + " 不支持受控验证（无配方/无标记载体/无本地车道）",
		})
		return facts, nil
	}

	cur := s.profiles.snapshot(string(id))
	facts.ProfileID = cur.Applied
	facts.ApplyTx = cur.AppliedTx
	facts.AppliedAt = cur.AppliedAt
	if cur.Applied == "" {
		facts.Blockers = append(facts.Blockers, runtimeVerifyBlocker{
			Reason:  reasonConfigNotApplied,
			Message: string(id) + " 当前没有已应用的档位卡：先应用一次配置再验证运行时生效",
		})
		facts.Blockers = append(facts.Blockers, s.runtimeVerifyEarlyGateBlockers(ctx, capUSD)...)
		return facts, nil
	}

	// 决定键（脱敏 + 摘要）。
	probe, perr := s.probeFor(id)
	if perr != nil {
		facts.Blockers = append(facts.Blockers, runtimeVerifyBlocker{
			Reason:  reasonNotObservable,
			Message: "档位探测失败，无法确定请求路径是否可观测",
		})
		facts.Blockers = append(facts.Blockers, s.runtimeVerifyEarlyGateBlockers(ctx, capUSD)...)
		return facts, nil
	}
	facts.DecideKey = probe.DecideKey
	facts.DecideMasked = maskCredentials(strings.TrimSpace(probe.DecideValue))
	facts.DecideDigest = tx.HashBytes([]byte(facts.DecideMasked))
	facts.ClientBaseURL = facts.DecideMasked
	facts.LocalRouteBase = s.localRouteBase()

	// 可观测形态：决定键必须指向**本机代理**的这个工具车道（旁路形态没有观测点）。
	routePrefix, ok := s.runtimeRequestRoute(facts.LocalRouteBase, facts.Prefixes, probe.DecideValue)
	if !ok {
		facts.Blockers = append(facts.Blockers, runtimeVerifyBlocker{
			Reason: reasonNotObservable,
			Message: "当前配置形态的请求不经本地可观测路径（直连网关/官方/router/独立端点之一）：" +
				"受控验证只支持本地 route 形态（决定键指向 " + facts.LocalRouteBase + "/<车道>）",
			Detail: map[string]any{"decideKey": probe.DecideKey},
		})
		facts.Blockers = append(facts.Blockers, s.runtimeVerifyEarlyGateBlockers(ctx, capUSD)...)
		return facts, nil
	}
	facts.RoutePrefix = routePrefix

	// 计划与盘上现状。
	plans, err := s.plansForCard(ctx, id, cur.Applied)
	if err != nil {
		return facts, err
	}
	facts.Plans = plans
	facts.ConfigPath = familyModelFile(id, s.Env)

	changed := make([]string, 0)
	for _, plan := range plans {
		raw, rerr := tx.ReadForFingerprint(plan.Path)
		if rerr != nil {
			return facts, rerr
		}
		digest := tx.HashBytes(raw)
		facts.ConfigFiles = append(facts.ConfigFiles, runtimeEvidenceFile{Path: plan.Path, SHA256: digest})
		if !plan.Delete && digest != tx.HashBytes(plan.After) {
			changed = append(changed, plan.Path)
		}
	}
	if len(changed) > 0 {
		facts.Blockers = append(facts.Blockers, runtimeVerifyBlocker{
			Reason:  reasonConfigChanged,
			Message: "盘上配置与当前档位卡的计划不符（应用后被外改或已漂移）：先重新应用再验证",
			Detail:  map[string]any{"changedPaths": changed},
		})
		// 早退不等于「这些事实不存在」：声明映射与费用口径照常读出（F2），否则预检会把
		// 未初始化零值当事实（空映射读成「未登记映射」、输出上界读成 0 tokens）。
		s.runtimeVerifyAttachDriftDataFace(ctx, &facts, capUSD)
		return facts, nil
	}

	// 标记注入的可行性。（声明映射已随 C 路模型映射删除——design §2.2：
	// 旧中继退役后本地 route 车道不再改写模型名，出站名 = 实发名。）
	if blob, ok := planBytesFor(plans, facts.ConfigPath); ok {
		tier, terr := runtimeVerifyTierModel(blob)
		if terr != nil {
			facts.Blockers = append(facts.Blockers, runtimeVerifyBlocker{
				Reason:  reasonMarkerUnresolvable,
				Message: "计划字节里没有可安全注入标记的档名键（" + runtimeVerifyMarkerKey + "）：" + terr.Error(),
			})
			return facts, nil
		}
		facts.TierModel = tier
	}
	marked := markModelName(facts.TierModel, marker)
	if marked == "" || marked == facts.TierModel {
		facts.Blockers = append(facts.Blockers, runtimeVerifyBlocker{
			Reason:  reasonMarkerUnresolvable,
			Message: "无法在档名上注入唯一标记（档名为空或标记为空）",
		})
		return facts, nil
	}
	facts.MarkedModel = marked
	facts.ExpectedUpstream = s.managedUpstreamModel()
	facts.UpstreamOrigin = s.runtimeUpstreamOrigin(routePrefix)

	// 报价与费用上界（未知报价 ⇒ 明示未知，不伪装免费）。
	s.runtimeVerifyLoadQuoteAndBound(ctx, &facts)

	// 闸门（额度 → 报价 → 预算）。顺序即拒绝原因的优先级：额度不足最靠前。
	s.runtimeVerifyAppendGateBlockers(ctx, &facts, capUSD)
	return facts, nil
}

// runtimeVerifyAttachDriftDataFace 在漂移早退路径上补齐预检数据面（F2）：声明映射、
// 估算口径（输出上界 + 单价）与闸门阻塞。
//
// 为什么早退也要补：漂移否定的是「这次能不能发起」，不否定「声明映射是什么」「目标
// 模型单价多少」这两件当前事实——重新应用后它们就是发起时会用到的那一份。缺了这一步，
// 预检就把未初始化零值当事实呈现（空映射被读成「未登记映射（直连网关）」、输出上界被
// 读成 `0 tokens`），与新鲜态不同形，违反 A11「触发前全字段可读」。读不出的事实如实
// 记阻塞或留空（估算金额按 unknown 给 null），绝不回零值冒充。
func (s *Server) runtimeVerifyAttachDriftDataFace(ctx context.Context, facts *runtimeVerifyFacts, capUSD float64) {
	// 档名从档位卡的计划字节读出（漂移不影响它：重新应用后盘上就是这份字节）；
	// C 路映射已删，估算目标 = 策略 default（读不出时留空——报价/上界按未知呈现，不猜）。
	if blob, ok := planBytesFor(facts.Plans, facts.ConfigPath); ok {
		if tier, terr := runtimeVerifyTierModel(blob); terr == nil {
			facts.TierModel = tier
			facts.ExpectedUpstream = s.managedUpstreamModel()
		}
	}
	s.runtimeVerifyLoadQuoteAndBound(ctx, facts)
	s.runtimeVerifyAppendGateBlockers(ctx, facts, capUSD)
}

// runtimeVerifyLoadQuoteAndBound 读目标模型的输出上界与单价（完整路径与早退数据面共用）。
//
// 目标模型 = 声明映射的映射后上游（facts.ExpectedUpstream）：读不出时两个事实都留空，
// 估算按「未知」呈现（quote=unknown + 金额 null），不拿 0 冒充。
func (s *Server) runtimeVerifyLoadQuoteAndBound(ctx context.Context, facts *runtimeVerifyFacts) {
	if strings.TrimSpace(facts.ExpectedUpstream) == "" {
		return
	}
	facts.OutputBoundTokens = runtimeVerifyOutputBoundTokens
	if p, perr := s.Policy.Load(); perr == nil && p != nil {
		if spec, ok := p.Spec(facts.ExpectedUpstream); ok && spec.MaxOutputTokens > 0 {
			facts.OutputBoundTokens = spec.MaxOutputTokens
		}
	}
	if quote, ok := s.runtimeVerifyQuote(ctx, facts.ExpectedUpstream); ok {
		facts.QuoteKnown = true
		facts.InPerMillion = quote.InPerMillion
		facts.OutPerMillion = quote.OutPerMillion
		facts.Currency = quote.Currency
	}
}

// runtimeVerifyAppendGateBlockers 把闸门（额度 → 报价 → 预算）的当前读数折成阻塞追加到
// facts。与完整路径同一顺序、同一词表；目标模型取档名（C 路映射已删），读不出时报价按
// 未知呈现（消息不点模型名，不编）。
func (s *Server) runtimeVerifyAppendGateBlockers(ctx context.Context, facts *runtimeVerifyFacts, capUSD float64) {
	switch q := s.runtimeVerifyQuotaNow(ctx); q.State {
	case runtimeQuotaExhausted:
		facts.Blockers = append(facts.Blockers, runtimeVerifyBlocker{
			Reason: reasonQuotaExhausted, Message: "网关额度已用尽，拒绝发起验证（未发出任何模型调用）", Detail: map[string]any{"quota": q},
		})
	case runtimeQuotaUnknown:
		facts.Blockers = append(facts.Blockers, runtimeVerifyBlocker{
			Reason: reasonQuotaUnknown, Message: "网关额度无法确定（账户面不可用/未登录/缓存过期），按「无法确定硬限不主动发起」处理", Detail: map[string]any{"quota": q},
		})
	}
	if !facts.QuoteKnown {
		target := "目标模型"
		if facts.ExpectedUpstream != "" {
			target = "目标模型（" + facts.ExpectedUpstream + "）"
		}
		facts.Blockers = append(facts.Blockers, runtimeVerifyBlocker{
			Reason: reasonQuoteUnknown,
			Message: target + "没有可用单价：费用未知不伪装免费，" +
				"按「无法确定硬限不主动发起」处理（可在用量定价表登记单价后重试）",
		})
	} else if upper := runtimeVerifyUpperBoundUSD(*facts); upper > capUSD {
		facts.Blockers = append(facts.Blockers, runtimeVerifyBlocker{
			Reason:  reasonBudgetExceeded,
			Message: "费用上界越过批准上限：拒绝发起（未发出任何模型调用）",
			Detail:  map[string]any{"upperBoundUsd": upper, "hardCapUsd": capUSD},
		})
	}
}

// runtimeVerifyEarlyGateBlockers 在「配置还没成形」的早退路径上补上额度/报价/预算阻塞。
//
// 为什么早退也要读这三项：预检的契约是「发起前给出全字段 + 阻塞如实呈现」——额度面与
// 定价面的事实与「配置有没有应用」无关（未登录、单价未登记都是**当前条件**），一次把会
// 拦下发起的原因都列出来，比只报一条、让用户去猜「就算配置补好了还能不能跑」更诚实。
// 目标模型取策略默认模型（完整路径用档名；此处还没有已生效配置可读）。
func (s *Server) runtimeVerifyEarlyGateBlockers(ctx context.Context, capUSD float64) []runtimeVerifyBlocker {
	blockers := make([]runtimeVerifyBlocker, 0, 2)
	switch q := s.runtimeVerifyQuotaNow(ctx); q.State {
	case runtimeQuotaExhausted:
		blockers = append(blockers, runtimeVerifyBlocker{
			Reason: reasonQuotaExhausted, Message: "网关额度已用尽，拒绝发起验证（未发出任何模型调用）", Detail: map[string]any{"quota": q},
		})
	case runtimeQuotaUnknown:
		blockers = append(blockers, runtimeVerifyBlocker{
			Reason: reasonQuotaUnknown, Message: "网关额度无法确定（账户面不可用/未登录/缓存过期），按「无法确定硬限不主动发起」处理", Detail: map[string]any{"quota": q},
		})
	}
	model := s.policyDefaultModelID()
	quote, ok := s.runtimeVerifyQuote(ctx, model)
	if model == "" || !ok {
		blockers = append(blockers, runtimeVerifyBlocker{
			Reason: reasonQuoteUnknown,
			Message: "目标模型没有可用单价：费用未知不伪装免费，按「无法确定硬限不主动发起」处理" +
				"（可在用量定价表登记单价后重试）",
		})
		return blockers
	}
	bound := runtimeVerifyOutputBoundTokens
	if p, err := s.Policy.Load(); err == nil && p != nil {
		if spec, ok := p.Spec(model); ok && spec.MaxOutputTokens > 0 {
			bound = spec.MaxOutputTokens
		}
	}
	probeFacts := runtimeVerifyFacts{
		InPerMillion: quote.InPerMillion, OutPerMillion: quote.OutPerMillion, OutputBoundTokens: bound,
	}
	if upper := runtimeVerifyUpperBoundUSD(probeFacts); upper > capUSD {
		blockers = append(blockers, runtimeVerifyBlocker{
			Reason:  reasonBudgetExceeded,
			Message: "费用上界越过批准上限：拒绝发起（未发出任何模型调用）",
			Detail:  map[string]any{"upperBoundUsd": upper, "hardCapUsd": capUSD},
		})
	}
	return blockers
}

// policyDefaultModelID 取策略默认模型 id（读不到返回空串；不编）。
func (s *Server) policyDefaultModelID() string {
	p, err := s.Policy.Load()
	if err != nil || p == nil {
		return ""
	}
	if model, ok := p.Default(); ok {
		return strings.TrimSpace(model.ID)
	}
	return ""
}

// probeFor 走家族探针（与 toolState 同一来源，只取决定键事实）。
func (s *Server) probeFor(id family.ID) (family.ProbeResult, error) {
	f, ok := s.Families.Lookup(id)
	if !ok {
		return family.ProbeResult{}, errNotSupported(id)
	}
	probe, err := f.Probe(s.Env)
	if err != nil {
		return family.ProbeResult{}, apierr.Wrap(apierr.CodeInternal, err, "档位探测失败")
	}
	return probe, nil
}

// runtimeRequestRoute 判定决定键值是否落在「本机代理 + 该工具车道」上。
//
// 判据是**字面前缀**（localRouteBase + "/" + 车道名之一），不是「回环即可」：
// 指向别的回环端口或别的路径时请求根本不会到达本产品的代理，那里没有观测点。
func (s *Server) runtimeRequestRoute(localRouteBase string, prefixes []string, decideValue string) (string, bool) {
	value := strings.TrimSpace(decideValue)
	if value == "" || localRouteBase == "" {
		return "", false
	}
	if !strings.HasPrefix(value, localRouteBase+"/") {
		return "", false
	}
	rest := strings.TrimPrefix(value, localRouteBase+"/")
	seg := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		seg = rest[:i]
	}
	for _, p := range prefixes {
		if seg == p {
			return p, true
		}
	}
	return "", false
}

// runtimeVerifyTierModel 从计划字节里读客户端档名键（当前只认 claude 的 env.ANTHROPIC_MODEL）。
func runtimeVerifyTierModel(blob []byte) (string, error) {
	doc, err := docmodel.ParseJSON5Doc(blob)
	if err != nil {
		return "", apierr.Wrap(apierr.CodeConfigInvalid, err, "计划字节不可解析")
	}
	value, ok := doc.Root().Get("env", runtimeVerifyMarkerKey).AsString()
	value = strings.TrimSpace(value)
	if !ok || value == "" {
		return "", apierr.New(apierr.CodeInvalidRequest, "缺少 %s", runtimeVerifyMarkerKey)
	}
	return value, nil
}

// markModelName 在档名上注入唯一标记：`<base>-<marker>[1m]`（`[1m]` 后缀保持在末尾）。
func markModelName(model, marker string) string {
	model = strings.TrimSpace(model)
	marker = strings.TrimSpace(marker)
	if model == "" || marker == "" {
		return ""
	}
	base, suffix := model, ""
	if strings.HasSuffix(base, proxy.OneMMarker) {
		base = strings.TrimSuffix(base, proxy.OneMMarker)
		suffix = proxy.OneMMarker
	}
	return base + "-" + marker + suffix
}

// ── 自 modelmap.go 迁出的本地文件读取辅助（与 C 路无关，design §2.2） ────────

// claudeModelKey 是 claude 家族的档名 env 键（acceptance-recipe §A 行 1 的客户侧键名，
// 自 modelmap.go 迁出）：它出现在用户 `~/.claude/settings.json` 的 `env` 段里，
// 是配置封层的对外契约键位；也是受控验证承载唯一标记的档名键。
const claudeModelKey = "ANTHROPIC_MODEL"

// familyModelFile 给出承载档名键的受管文件。
func familyModelFile(id family.ID, env *config.Env) string {
	switch id {
	case family.Claude:
		return env.ClaudeSettings()
	case family.ClaudeDesktop:
		return claudeDesktopProfileFile(env)
	}
	return ""
}

// planBytesFor 取目标文件的计划字节；删除计划或不在计划里的路径返回 false。
func planBytesFor(plans []family.FilePlan, path string) ([]byte, bool) {
	if path == "" {
		return nil, false
	}
	for _, plan := range plans {
		if plan.Path != path || plan.Delete {
			continue
		}
		if len(plan.After) == 0 {
			return nil, false
		}
		return plan.After, true
	}
	return nil, false
}

// managedUpstreamModel 给出计价/关联用的出站目标模型。
//
// C 路模型映射删除后（design §2.2）没有「档名 → 上游名」改写层；受管语义是
// 「只会把策略 default 写进工具配置」（policy.ManagedModels），因此出站目标 =
// 策略 default。读不出（策略未就绪）⇒ 空串，报价按未知呈现，不猜。
func (s *Server) managedUpstreamModel() string {
	if p, err := s.Policy.Load(); err == nil && p != nil {
		if def, ok := p.Default(); ok {
			return def.ID
		}
	}
	return ""
}

// runtimeUpstreamOrigin 取某前缀的出网 origin。
// 九前缀出厂路由表已随 C 路删除（design §2.4）：读数恒为空串——证据字段如实留空，
// 不编造 origin（失效判定两侧同为空串，不会产生假 invalidation）。
func (s *Server) runtimeUpstreamOrigin(prefix string) string {
	_ = prefix
	return ""
}

// newRuntimeVerifyMarker 生成唯一标记（`agv-` + 12 hex）。
func newRuntimeVerifyMarker() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", apierr.Wrap(apierr.CodeInternal, err, "生成验证标记失败")
	}
	return runtimeVerifyMarkerPrefix + hex.EncodeToString(b), nil
}

// ── 报价与预算 ──────────────────────────────────────────────────────────────

// runtimeVerifyQuoteInfo 是目标模型的一对单价（每百万 token）。
type runtimeVerifyQuoteInfo struct {
	InPerMillion  float64
	OutPerMillion float64
	Currency      string
}

// runtimeVerifyQuote 查目标模型的单价。查不到（无定价表/无该条目/单价非正）⇒ ok=false。
//
// 单价非正按「未知」处理而不是「免费」：0 元会被读成「这次验证不花钱」，而事实是
// 没有可执行的计费维度上界（UC-092 A11「费用未知须明示，不伪装免费」）。
func (s *Server) runtimeVerifyQuote(ctx context.Context, model string) (runtimeVerifyQuoteInfo, bool) {
	if s.Usage == nil || strings.TrimSpace(model) == "" {
		return runtimeVerifyQuoteInfo{}, false
	}
	table, err := s.Usage.Pricing(ctx)
	if err != nil {
		return runtimeVerifyQuoteInfo{}, false
	}
	for _, entry := range table.Entries {
		if strings.TrimSpace(entry.Model) != strings.TrimSpace(model) {
			continue
		}
		if entry.InputPerMillion <= 0 || entry.OutputPerMillion <= 0 {
			return runtimeVerifyQuoteInfo{}, false
		}
		currency := strings.TrimSpace(entry.Currency)
		if currency == "" {
			currency = "USD"
		}
		return runtimeVerifyQuoteInfo{InPerMillion: entry.InputPerMillion, OutPerMillion: entry.OutputPerMillion, Currency: currency}, true
	}
	return runtimeVerifyQuoteInfo{}, false
}

// runtimeVerifyUpperBoundUSD 给出费用上界（输入/输出都按声明上界算，非典型值）。
func runtimeVerifyUpperBoundUSD(f runtimeVerifyFacts) float64 {
	in := float64(runtimeVerifyInputBoundTokens) * f.InPerMillion / 1e6
	out := float64(f.OutputBoundTokens) * f.OutPerMillion / 1e6
	return roundMoney(in + out)
}

// runtimeVerifyEstimateUSD 给出预计费用（最小请求的典型值）。
func runtimeVerifyEstimateUSD(f runtimeVerifyFacts) float64 {
	in := float64(runtimeVerifyEstimateInputTokens) * f.InPerMillion / 1e6
	out := float64(runtimeVerifyEstimateOutputTokens) * f.OutPerMillion / 1e6
	return roundMoney(in + out)
}

// runtimeVerifyEstimateFor 组装费用估算（口径 = 典型最小请求 × 单价；上界 = 声明上界 × 单价）。
//
// 报价未知 ⇒ 两个金额为 null 且 quote=unknown（与 §8 Summary.Cost 同一条纪律，不伪装免费）；
// 目标模型不可定（声明映射读不出）⇒ 输出上界一并明示未知——不把未初始化的 0 当事实（F2）。
func runtimeVerifyEstimateFor(facts runtimeVerifyFacts) runtimeVerifyEstimate {
	est := runtimeVerifyEstimate{
		Quote:             "unknown",
		Currency:          "USD",
		InputTokensBound:  runtimeVerifyInputBoundTokens,
		OutputTokensBound: facts.OutputBoundTokens,
		Basis: "预计费用 = 典型最小请求（输入 " + itoa(runtimeVerifyEstimateInputTokens) +
			" tokens、输出 " + itoa(runtimeVerifyEstimateOutputTokens) + " tokens）× 单价；上界 = 输入 ≤ " +
			itoa(runtimeVerifyInputBoundTokens) + " tokens",
	}
	if facts.OutputBoundTokens > 0 {
		est.Basis += "、输出 ≤ " + itoa(facts.OutputBoundTokens) + " tokens × 单价"
	} else {
		est.Basis += " × 单价 + 输出上界未知（目标模型不可定）"
	}
	if facts.QuoteKnown {
		est.Quote = "known"
		est.Currency = facts.Currency
		estimate := runtimeVerifyEstimateUSD(facts)
		upper := runtimeVerifyUpperBoundUSD(facts)
		est.EstimatedCostUSD = &estimate
		est.UpperBoundUSD = &upper
	}
	return est
}

// roundMoney 六位小数四舍五入（与用量面金额刻度同量级，避免浮点尾巴进入响应）。
func roundMoney(v float64) float64 {
	return float64(int64(v*1e6+0.5)) / 1e6
}

// ── 额度判定面（读本地缓存，绝不发起网络请求） ──────────────────────────────

// 额度判定状态（封闭取值）。
const (
	runtimeQuotaSufficient = "sufficient"
	runtimeQuotaExhausted  = "exhausted"
	runtimeQuotaUnknown    = "unknown"
)

// runtimeVerifyQuota 是额度读数（响应形状）。
type runtimeVerifyQuota struct {
	State  string `json:"state"`
	Source string `json:"source"`
	Detail string `json:"detail"`
}

// runtimeVerifyQuotaNow 读当前额度判定（测试可注入 runtimeVerifyQuotaRead；
// 生产实现读本地限额，与 quotagate 同源）。
func (s *Server) runtimeVerifyQuotaNow(ctx context.Context) runtimeVerifyQuota {
	if s.runtimeVerifyQuotaRead != nil {
		return s.runtimeVerifyQuotaRead(ctx)
	}
	return s.gatewayVerifyQuota(ctx)
}

// gatewayVerifyQuota 是生产实现：读**本地用量/限额**（与 internal/quotagate 同一
// 数据源；不发网络请求、不动任何计费接口）。登录链删除后账户面缓存不复存在
// （design §2.2/§4.1：额度闸门改读 quotagate 同源）。
//
// 判定语义与本地限额纪律同源（internal/usage/quota.go）：
//   - 用量库缺席或读数失败 ⇒ unknown——「无法确定硬限」按不主动发起处理；
//   - 无记录/未登记限额 ⇒ 不限（sufficient）；有余量 ⇒ sufficient；
//   - 生效用量 ≥ 限额 ⇒ exhausted。
//
// slug 取本机身份（anonymousUID「local」）：受控验证是管理员本机动作，
// 不属于任何 agserv 用户的会话流量。
func (s *Server) gatewayVerifyQuota(ctx context.Context) runtimeVerifyQuota {
	if s.Usage == nil {
		return runtimeVerifyQuota{State: runtimeQuotaUnknown, Source: "quotagate",
			Detail: "用量库不可用（进程启动时打开失败）"}
	}
	return quotaVerdictFromUsage(s.Usage.QuotaFor(ctx, anonymousUID))
}

// quotaVerdictFromUsage 把本地限额读数折算成额度判定（纯函数，便于逐值测试）。
func quotaVerdictFromUsage(q usage.Quota, err error) runtimeVerifyQuota {
	if err != nil {
		return runtimeVerifyQuota{State: runtimeQuotaUnknown, Source: "quotagate",
			Detail: "本地限额读数失败：" + maskCredentials(err.Error())}
	}
	if q.Exhausted() {
		return runtimeVerifyQuota{State: runtimeQuotaExhausted, Source: "quotagate",
			Detail: "本机验证 slug 的本地限额已用尽"}
	}
	if !q.Enforced() {
		return runtimeVerifyQuota{State: runtimeQuotaSufficient, Source: "quotagate",
			Detail: "本地限额未对该 slug 登记（无记录不限）"}
	}
	return runtimeVerifyQuota{State: runtimeQuotaSufficient, Source: "quotagate",
		Detail: "本地限额有余量"}
}

// ── 受控工作区 ──────────────────────────────────────────────────────────────

// buildRuntimeVerifyWorkspace 把**盘上字节**写进一次性临时根，并注入唯一标记。
//
// 与 §4.6 的临时根共用两件机制（刻意不另写一套）：[`plannedContent`] 取要写出的文件、
// [`rewriteDataRootRefs`] 改写数据根指针、[`tempTreeNames`] 保结构落位。
// 与 §4.6 的差别只有一处：档名键被注入 `-agv-<nonce>` 标记（这正是受控验证的
// 「唯一标记」载体，声明映射在注入前已在 facts 层核过「映射结果不变」）。
//
// 权限纪律与 §4.6 相同：目录 0700 / 文件 0600；调用方**必须**在运行后用 defer 删除
// 整棵根（它装着带真凭据的配置副本）。
func (s *Server) buildRuntimeVerifyWorkspace(id family.ID, plans []family.FilePlan, marker string) (string, runtimeEvidenceWorkspace, error) {
	root, err := os.MkdirTemp("", runtimeVerifyDirPrefix)
	if err != nil {
		return "", runtimeEvidenceWorkspace{}, apierr.Wrap(apierr.CodeStorage, err, "建立受控工作区失败")
	}
	fail := func(e error) (string, runtimeEvidenceWorkspace, error) {
		_ = os.RemoveAll(root)
		return "", runtimeEvidenceWorkspace{}, e
	}
	if merr := os.Chmod(root, 0o700); merr != nil {
		return fail(apierr.Wrap(apierr.CodeStorage, merr, "设置受控工作区权限失败"))
	}
	content := plannedContent(plans)
	if len(content) == 0 {
		return fail(apierr.New(apierr.CodeConfigInvalid, "该配置没有任何要写出的文件，无法在受控工作区复现"))
	}
	names, nerr := tempTreeNames(s.Env.Home, plans)
	if nerr != nil {
		return fail(nerr)
	}
	refs := make([]string, 0, len(content)+8)
	for planPath := range content {
		refs = append(refs, planPath)
	}
	refs = append(refs, dataRootFiles(s.Env.DataRoot())...)

	configFile := familyModelFile(id, s.Env)
	copies := map[string][]byte{}
	written := map[string][]byte{}
	for planPath, blob := range content {
		rewritten := s.rewriteDataRootRefs(root, refs, blob, content, names, copies)
		if planPath == configFile {
			injected, ierr := injectRuntimeVerifyMarker(rewritten, marker)
			if ierr != nil {
				return fail(ierr)
			}
			rewritten = injected
		}
		written[names[planPath]] = rewritten
	}
	for target, blob := range copies {
		rel, rerr := filepath.Rel(root, target)
		if rerr != nil {
			return fail(apierr.Wrap(apierr.CodeStorage, rerr, "受控工作区引用副本定位失败"))
		}
		written[filepath.ToSlash(rel)] = blob
	}

	ws := runtimeEvidenceWorkspace{Root: root}
	relNames := make([]string, 0, len(written))
	for name := range written {
		relNames = append(relNames, name)
	}
	sort.Strings(relNames)
	for _, name := range relNames {
		blob := written[name]
		target := filepath.Join(root, filepath.FromSlash(name))
		if werr := writeTempFile(target, blob); werr != nil {
			return fail(werr)
		}
		ws.Files = append(ws.Files, runtimeEvidenceFile{Path: name, SHA256: tx.HashBytes(blob)})
		if name == names[configFile] {
			ws.ConfigPath = target
		}
	}
	return root, ws, nil
}

// injectRuntimeVerifyMarker 把唯一标记写进档名键（claude 的 env.ANTHROPIC_MODEL）。
//
// 只标客户端档名键：`ANTHROPIC_DEFAULT_*_MODEL` 是映射**目标**（上游名），标了会让
// 出网模型名变形。一个可标的键都没有时**拒绝**（没有唯一标记的请求无法与旧进程/
// 旧配置版本的相同模型请求区分，宁可不发起）。
func injectRuntimeVerifyMarker(blob []byte, marker string) ([]byte, error) {
	doc, err := docmodel.ParseJSON5Doc(blob)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeConfigInvalid, err, "受控工作区的配置字节不可解析，无法注入唯一标记")
	}
	marked := 0
	value, ok := doc.Root().Get("env", runtimeVerifyMarkerKey).AsString()
	if ok && strings.TrimSpace(value) != "" {
		name := markModelName(value, marker)
		if name != "" && name != value {
			if serr := doc.SetJSON(name, "env", runtimeVerifyMarkerKey); serr != nil {
				return nil, apierr.Wrap(apierr.CodeConfigInvalid, serr, "注入验证标记失败")
			}
			marked++
		}
	}
	if marked == 0 {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"配置里没有可注入唯一标记的档名键（%s）", runtimeVerifyMarkerKey)
	}
	return doc.Render(), nil
}

// ── 观测关联（唯一标记 + 五要素一致才升级） ─────────────────────────────────

// runtimeObservedRecord 是候选观测记录的最小投影（与用量记录同源）。
type runtimeObservedRecord struct {
	ID             string
	At             time.Time
	App            string
	Prefix         string
	ModelRequested string
	ModelUpstream  string
	Status         int
	DurationMs     int64
	InputTokens    int64
	OutputTokens   int64
	// RuleVersion 是记录携带的生效规则版本（UC-096 A6；空串 = 该记录不适用/未知）。
	// 证据面照抄它（受控验证只复核、不重算），因此证据与记账同一口径。
	RuleVersion string
}

// correlateRuntimeObservation 在候选集合里找「该工具自身、经可观测路径、带唯一标记、
// 五要素一致」的那一条。
//
// 负向矩阵的每个分支都在这里有一条独立判据（顺序即判据优先级）：
//   - 另一工具（app 不符）⇒ not_self；
//   - 旁路/错路径（前缀不在该工具车道）⇒ path_unobservable；
//   - 旧进程/旧配置版本的相同模型请求（没有本次唯一标记）⇒ marker_missing；
//   - 映射不符（上游名与声明不一致）⇒ mapping_mismatch；
//   - 已关联但上游非 2xx ⇒ upstream_rejected。
//
// 找不到任何可关联记录时给出**最接近**的拒绝原因（而不是笼统的「没有」），
// 一条都没有时才是 observation_missing。
func correlateRuntimeObservation(recs []runtimeObservedRecord, apps, prefixes []string, marker, expectedUpstream string) (runtimeObservedRecord, string, bool) {
	var best runtimeObservedRecord
	bestDepth, bestReason, haveBest := 0, "", false
	for _, rec := range recs {
		depth, reason := 0, ""
		switch {
		case !containsString(apps, rec.App):
			depth, reason = 1, reasonNotSelf
		case !containsString(prefixes, rec.Prefix):
			depth, reason = 2, reasonPathUnobservable
		case !strings.Contains(rec.ModelRequested, marker):
			depth, reason = 3, reasonMarkerMissing
		case rec.ModelUpstream != expectedUpstream:
			depth, reason = 4, reasonMappingMismatch
		case rec.Status < 200 || rec.Status >= 300:
			depth, reason = 5, reasonUpstreamRejected
		default:
			return rec, reasonVerified, true
		}
		if depth > bestDepth {
			best, bestDepth, bestReason, haveBest = rec, depth, reason, true
		}
	}
	if haveBest {
		return best, bestReason, false
	}
	return runtimeObservedRecord{}, reasonObservationMissing, false
}

// containsString 是集合判定的小工具。
func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// observeRuntimeRequest 读观测窗口内的候选记录（只读用量库，不等待成功——等待窗口
// 只是给代理的记账提交留一点可见性时间，找不到就是找不到）。
func (s *Server) observeRuntimeRequest(ctx context.Context, since time.Time) []runtimeObservedRecord {
	if s.Usage == nil {
		return nil
	}
	page, err := s.Usage.Requests(ctx, usage.RequestsQuery{
		Window: usage.Window{From: since},
		Order:  usage.OrderAsc,
		Limit:  usage.MaxLimit,
	})
	if err != nil {
		s.log.Warn("读取运行时观测记录失败", "err", err)
		return nil
	}
	out := make([]runtimeObservedRecord, 0, len(page.Items))
	for _, item := range page.Items {
		at, perr := time.Parse(time.RFC3339Nano, item.At)
		if perr != nil {
			at, perr = time.Parse(time.RFC3339, item.At)
			if perr != nil {
				continue
			}
		}
		out = append(out, runtimeObservedRecord{
			ID:             item.ID,
			At:             at,
			App:            item.App,
			Prefix:         item.Prefix,
			ModelRequested: item.ModelRequested,
			ModelUpstream:  item.ModelUpstream,
			Status:         item.Status,
			DurationMs:     item.DurationMs,
			InputTokens:    item.InputTokens,
			OutputTokens:   item.OutputTokens,
			RuleVersion:    item.RuleVersion,
		})
	}
	return out
}

// ── HTTP：GET /tools/{id}/runtime-evidence ─────────────────────────────────

// runtimeEvidencePayload 是 §4.8 GET 的响应形状。
type runtimeEvidencePayload struct {
	ToolID    string `json:"toolId"`
	State     string `json:"state"`
	Reason    string `json:"reason"`
	CheckedAt string `json:"checkedAt"`
	// Evidence 只在**读取时复核通过**且已验证时非 null（失效证据不在此字段回流）。
	Evidence *runtimeEvidenceRecord `json:"evidence"`
	// LastObtainedAt 是历史上最近一次证据的获得时间（供界面说明「上次验证于 X 已失效」）。
	LastObtainedAt string `json:"lastObtainedAt,omitempty"`
	// ReasonDetail 是失效/阻塞的非敏感细节（路径清单、事务身份等，绝不含配置值）。
	ReasonDetail map[string]any `json:"reasonDetail,omitempty"`
	Verify       struct {
		Supported         bool                   `json:"supported"`
		Reason            string                 `json:"reason"`
		WillCallModel     bool                   `json:"willCallModel"`
		DefaultHardCapUSD float64                `json:"defaultHardCapUsd"`
		Attempts          []runtimeVerifyAttempt `json:"attempts"`
		AttemptCount      int                    `json:"attemptCount"`
	} `json:"verify"`
}

// handleRuntimeEvidence 是 `GET /api/v1/tools/{id}/runtime-evidence`。
//
// 只读端点的纪律：状态**在服务端读取时刻判定**（多窗口/刷新同源），且不写任何字节。
// 「已验证」= 存在证据 **且** 读取时复核通过；任何一档失效都降级为未确认 + 机器可读原因。
func (s *Server) handleRuntimeEvidence(r *http.Request) (any, error) {
	id, err := s.requireFamily(r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	payload := runtimeEvidencePayload{ToolID: string(id), CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	supported, supportReason := runtimeVerifySupported(id)
	payload.Verify.Supported = supported
	payload.Verify.Reason = supportReason
	payload.Verify.WillCallModel = supported
	payload.Verify.DefaultHardCapUSD = runtimeVerifyDefaultCapUSD
	payload.Verify.Attempts = []runtimeVerifyAttempt{}
	payload.Verify.AttemptCount = 0

	rec := s.runtimeEvidence.tool(string(id))
	if len(rec.Attempts) > 0 {
		payload.Verify.Attempts = rec.Attempts
		payload.Verify.AttemptCount = len(rec.Attempts)
	}

	if rec.Evidence == nil {
		payload.State = "unconfirmed"
		if !supported {
			payload.Reason = supportReason
		} else {
			payload.Reason = reasonNeverVerified
		}
		return payload, nil
	}
	payload.LastObtainedAt = rec.Evidence.ObtainedAt
	reason, detail := s.runtimeEvidenceInvalidation(r.Context(), id, *rec.Evidence)
	if reason != "" {
		payload.State = "unconfirmed"
		payload.Reason = reason
		payload.ReasonDetail = detail
		return payload, nil
	}
	payload.State = "verified"
	payload.Reason = reasonVerified
	payload.Evidence = rec.Evidence
	return payload, nil
}

// runtimeEvidenceInvalidation 是**读取时**的失效判定（A10）：返回空原因 = 证据仍然有效。
//
// 三类失效与冻结文本一一对应：
//  1. 再次应用：应用事务身份（appliedTx）变化——即使字节相同（幂等重放）也使旧证据失效；
//     生效记账被恢复/重置清空时按 config_changed 报（配置版本已不是被验证的那一份）；
//  2. 外改：盘上逐文件 sha256 与证据记录不符（含文件被删除/路径变化）；
//  3. 模型/端点映射变化：声明映射重算不一致，或出网 origin 变化。
func (s *Server) runtimeEvidenceInvalidation(ctx context.Context, id family.ID, ev runtimeEvidenceRecord) (string, map[string]any) {
	cur := s.profiles.snapshot(string(id))
	switch {
	case cur.Applied == "":
		return reasonConfigChanged, map[string]any{"detail": "生效档位记账已清空（恢复或重置之后）"}
	case cur.AppliedTx != ev.Config.ApplyTx:
		return reasonReapplied, map[string]any{"evidenceApplyTx": ev.Config.ApplyTx, "currentApplyTx": cur.AppliedTx}
	}

	changed := make([]string, 0)
	for _, file := range ev.Config.Files {
		raw, err := tx.ReadForFingerprint(file.Path)
		if err != nil {
			return reasonConfigChanged, map[string]any{"detail": "盘上配置不可读", "path": file.Path}
		}
		if tx.HashBytes(raw) != file.SHA256 {
			changed = append(changed, file.Path)
		}
	}
	if len(changed) > 0 {
		return reasonConfigChanged, map[string]any{"changedPaths": changed}
	}

	// 声明映射重算比较已随 C 路模型映射删除（design §2.2）：失效判定保留端点 origin 比较。
	if origin := s.runtimeUpstreamOrigin(ev.Outbound.Endpoint.ObservedPrefix); origin != ev.Outbound.Endpoint.UpstreamOrigin {
		return reasonMappingChanged, map[string]any{"upstreamOrigin": origin,
			"evidenceUpstreamOrigin": ev.Outbound.Endpoint.UpstreamOrigin}
	}
	return "", nil
}

// ── HTTP：POST /tools/{id}/runtime-evidence/verify/preview ─────────────────

// runtimeVerifyPlan 是受控验证的可读计划（发起前必须能看清全字段）。
type runtimeVerifyPlan struct {
	PreviewID string `json:"previewId"`
	ExpiresAt string `json:"expiresAt"`
	ToolID    string `json:"toolId"`
	Supported bool   `json:"supported"`
	// Scope 是范围：目标工具、生效档、车道前缀、受控工作区与配置位置。
	Scope runtimeVerifyScope `json:"scope"`
	// WillCallModel 恒为 true：受控验证按定义会发起一次模型调用（被闸门拒绝时不发起）。
	WillCallModel bool `json:"willCallModel"`
	// Marker 是本次将注入的唯一标记（全字段可读）。
	Marker string `json:"marker"`
	// Estimate 是费用估算（报价未知时 quote=unknown 且金额为 null，不伪装免费）。
	Estimate   runtimeVerifyEstimate `json:"estimate"`
	HardCapUSD float64               `json:"hardCapUsd"`
	Quota      runtimeVerifyQuota    `json:"quota"`
	// Blockers 恒为数组：非空表示当前条件下会被拒绝（含原因，发起前即可读）。
	Blockers []runtimeVerifyBlocker `json:"blockers"`
}

// runtimeVerifyScope 是计划的作用范围。
type runtimeVerifyScope struct {
	ToolID      string   `json:"toolId"`
	ProfileID   string   `json:"profileId"`
	Prefixes    []string `json:"prefixes"`
	ConfigPaths []string `json:"configPaths"`
	// Workspace 是受控工作区（临时根；本次运行结束即删）的**预期位置前缀**。
	Workspace    string `json:"workspace"`
	ExecutedAs   string `json:"executedAs"`
	ObservableAt string `json:"observableAt"`
}

// runtimeVerifyEstimate 是费用估算（basis 明示估算口径与两个上界）。
//
// 金额为 **null = 未知**（与 §8 的 Summary.Cost 同一条纪律）：报价未知时给 0 会被读成
// 「不花钱」；这里给 null，且 `quote` 字段明示 unknown。
type runtimeVerifyEstimate struct {
	Quote             string   `json:"quote"`
	Currency          string   `json:"currency"`
	EstimatedCostUSD  *float64 `json:"estimatedCostUsd"`
	UpperBoundUSD     *float64 `json:"upperBoundUsd"`
	InputTokensBound  int      `json:"inputTokensBound"`
	OutputTokensBound int      `json:"outputTokensBound"`
	Basis             string   `json:"basis"`
}

// verifyPlanMeta 是塞进租约 Meta 的执行所需信息（+ 版本指纹，执行前复核）。
type verifyPlanMeta struct {
	Plan     runtimeVerifyPlan `json:"plan"`
	Identity string            `json:"identity"`
	CapUSD   float64           `json:"capUsd"`
}

// handleRuntimeVerifyPreview 是 `POST /api/v1/tools/{id}/runtime-evidence/verify/preview`。
//
// 预览与执行两步走（与 §4.3 的 preview/apply 同一形态）：预览给出全字段可读的计划
// （目标/范围/是否产生模型调用/预计费用/硬上限/阻塞清单），执行只吃一次性 previewId。
// 不支持的工具直接 not_supported（不是「暂时不能」而是没有检测入口）。
func (s *Server) handleRuntimeVerifyPreview(r *http.Request) (any, string, error) {
	id, err := s.requireFamily(r.PathValue("id"))
	if err != nil {
		return nil, "", err
	}
	if proxyOnlyFamily(id) {
		return nil, "", apierr.New(apierr.CodeNotSupported,
			"WorkBuddy 走独立入口（/workbuddy/*），不支持运行时证据与受控验证")
	}
	if ok, reason := runtimeVerifySupported(id); !ok {
		return nil, "", apierr.New(apierr.CodeNotSupported,
			"%s 不支持受控验证（%s）：没有「CLI 配方 + 唯一标记载体 + 本地可观测车道」三件齐备的检测入口",
			id, reason).WithDetails(map[string]any{"toolId": string(id), "reason": reason})
	}
	var body struct {
		MaxCostUSD float64 `json:"maxCostUsd"`
	}
	if err := decodeOptional(r, &body); err != nil {
		return nil, "", err
	}
	if body.MaxCostUSD < 0 {
		return nil, "", apierr.New(apierr.CodeInvalidRequest, "maxCostUsd 不得为负数")
	}
	if body.MaxCostUSD > runtimeVerifyMaxCapUSD {
		return nil, "", apierr.New(apierr.CodeInvalidRequest,
			"maxCostUsd 超过单次受控验证上限 %.2f", runtimeVerifyMaxCapUSD)
	}
	capUSD := body.MaxCostUSD
	if capUSD == 0 {
		capUSD = runtimeVerifyDefaultCapUSD
	}

	marker, err := newRuntimeVerifyMarker()
	if err != nil {
		return nil, "", err
	}
	facts, err := s.runtimeVerifyFactsFor(r.Context(), id, marker, capUSD)
	if err != nil {
		return nil, "", err
	}
	plan := runtimeVerifyPlan{
		ToolID:    string(id),
		Supported: true,
		Scope: runtimeVerifyScope{
			ToolID:       string(id),
			ProfileID:    facts.ProfileID,
			Prefixes:     facts.Prefixes,
			ConfigPaths:  runtimeVerifyConfigPaths(facts),
			Workspace:    os.TempDir() + string(filepath.Separator) + runtimeVerifyDirPrefix + "*",
			ExecutedAs:   profileTestRecipes[id].cli,
			ObservableAt: facts.LocalRouteBase + "/" + facts.RoutePrefix,
		},
		WillCallModel: true,
		Marker:        marker,
		HardCapUSD:    capUSD,
		Quota:         s.runtimeVerifyQuotaNow(r.Context()),
		Blockers:      facts.Blockers,
	}
	if plan.Scope.Prefixes == nil {
		plan.Scope.Prefixes = []string{}
	}
	if plan.Scope.ConfigPaths == nil {
		plan.Scope.ConfigPaths = []string{}
	}
	if plan.Blockers == nil {
		plan.Blockers = []runtimeVerifyBlocker{}
	}
	plan.Estimate = runtimeVerifyEstimateFor(facts)

	meta, merr := json.Marshal(verifyPlanMeta{Plan: plan, Identity: facts.identity(), CapUSD: capUSD})
	if merr != nil {
		return nil, "", apierr.Wrap(apierr.CodeInternal, merr, "编码验证计划失败")
	}
	lease := s.Leases.Put(&tx.Lease{
		ToolID:    string(id),
		Operation: "runtime-evidence.verify",
		Meta:      map[string]string{"plan": string(meta)},
	})
	plan.PreviewID = lease.ID
	plan.ExpiresAt = lease.ExpiresAt.Format(rfc3339Second)
	return plan, "", nil
}

// runtimeVerifyConfigPaths 给出计划涉及的受管文件路径（范围展示用）。
func runtimeVerifyConfigPaths(facts runtimeVerifyFacts) []string {
	out := make([]string, 0, len(facts.ConfigFiles))
	for _, file := range facts.ConfigFiles {
		out = append(out, file.Path)
	}
	return out
}

// ── HTTP：POST /tools/{id}/runtime-evidence/verify/apply ──────────────────

// runtimeVerifyResultPayload 是受控验证执行的响应形状。
type runtimeVerifyResultPayload struct {
	ToolID string `json:"toolId"`
	Status string `json:"status"` // verified | unconfirmed
	Reason string `json:"reason"`
	// ModelCalls 是本次尝试已发出的模型调用数（拒绝 = 0，执行 = 1；恒不自动重发）。
	ModelCalls int `json:"modelCalls"`
	// AttemptCount 是累计尝试条数（调用计数可核：GET 的 attempts 与之同源）。
	AttemptCount int                     `json:"attemptCount"`
	Observation  *runtimeEvidenceRequest `json:"observation"`
	Evidence     *runtimeEvidenceRecord  `json:"evidence"`
	Blockers     []runtimeVerifyBlocker  `json:"blockers"`
}

// handleRuntimeVerifyApply 是 `POST /api/v1/tools/{id}/runtime-evidence/verify/apply`。
//
// 执行纪律（与硬边界逐条对应）：
//   - 事前再读一遍全部门槛（额度/报价/预算/配置版本）：预览后条件变化一律拒绝发起；
//   - 只发起**一次**受控运行；失败/超时/观测缺失只记尝试与原因，不重发、不重启工具；
//   - 唯一标记把「该工具本次运行」与任何同模型的旧进程/旧配置版本请求分开；
//   - 受控工作区（带真凭据的配置副本）运行后立即删除，响应与日志只含脱敏值。
func (s *Server) handleRuntimeVerifyApply(r *http.Request) (any, string, error) {
	id, err := s.requireFamily(r.PathValue("id"))
	if err != nil {
		return nil, "", err
	}
	if proxyOnlyFamily(id) {
		return nil, "", apierr.New(apierr.CodeNotSupported,
			"WorkBuddy 走独立入口（/workbuddy/*），不支持运行时证据与受控验证")
	}
	var body struct {
		PreviewID string `json:"previewId"`
	}
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	lease, err := s.Leases.Take(body.PreviewID)
	if err != nil {
		return nil, "", err
	}
	if lease.ToolID != "" && lease.ToolID != string(id) {
		return nil, "", apierr.New(apierr.CodeInvalidRequest,
			"验证预览属于 %s，不能用 %s 执行", lease.ToolID, id)
	}
	var meta verifyPlanMeta
	if uerr := json.Unmarshal([]byte(lease.Meta["plan"]), &meta); uerr != nil {
		return nil, "", apierr.New(apierr.CodeInternal, "验证预览载荷不可解析")
	}
	marker := meta.Plan.Marker
	capUSD := meta.CapUSD

	facts, ferr := s.runtimeVerifyFactsFor(r.Context(), id, marker, capUSD)
	if ferr != nil {
		return nil, "", ferr
	}
	blockers := facts.Blockers
	if facts.identity() != meta.Identity {
		blockers = append([]runtimeVerifyBlocker{{
			Reason:  reasonConfigChanged,
			Message: "配置版本在预览之后发生了变化（外改/再次应用/映射变化）：重新预览再执行",
		}}, blockers...)
	}
	if len(blockers) > 0 {
		reason := blockers[0].Reason
		s.recordRuntimeAttempt(id, reason, 0)
		payload := runtimeVerifyResultPayload{
			ToolID: string(id), Status: "unconfirmed", Reason: reason,
			ModelCalls: 0, AttemptCount: s.runtimeEvidence.tool(string(id)).AttemptCount(),
			Blockers: blockers,
		}
		return payload, "", &loggedFailure{data: payload, code: runtimeVerifyFailureCode(reason)}
	}

	// 与 §4.6 共用同工具并发闸门（在飞运行表）：一次受控运行与一次配置测试都是满负载
	// CLI，互斥。受控验证在本波不开放停止入口（§4.6 的 cancel 端点只停配置测试；
	// 撞上在飞验证时如实报 kind=verify），因此登记把手不带 cancel。
	run := s.startControlledRun(id, controlledRunVerify, "", nil)
	if run == nil {
		s.recordRuntimeAttempt(id, reasonInProgress, 0)
		payload := runtimeVerifyResultPayload{
			ToolID: string(id), Status: "unconfirmed", Reason: reasonInProgress,
			ModelCalls: 0, AttemptCount: s.runtimeEvidence.tool(string(id)).AttemptCount(),
			Blockers: []runtimeVerifyBlocker{{Reason: reasonInProgress, Message: string(id) + " 上已有一次受控运行或配置测试在跑，等它结束后再试"}},
		}
		return payload, "", &loggedFailure{data: payload, code: apierr.CodeTestInProgress}
	}
	defer s.finishControlledRun(run)

	recipe := profileTestRecipes[id]
	exe := s.resolveTestCLI(id, recipe.cli)
	if exe == "" {
		reason := reasonUnsupportedTool
		s.recordRuntimeAttempt(id, reason, 0)
		payload := runtimeVerifyResultPayload{
			ToolID: string(id), Status: "unconfirmed", Reason: reason,
			ModelCalls: 0, AttemptCount: s.runtimeEvidence.tool(string(id)).AttemptCount(),
			Blockers: []runtimeVerifyBlocker{{Reason: reason, Message: "找不到 " + recipe.cli + " 可执行文件（PATH 与常见 bin 目录均未命中）"}},
		}
		return payload, "", &loggedFailure{data: payload, code: apierr.CodeNotSupported}
	}

	root, ws, werr := s.buildRuntimeVerifyWorkspace(id, facts.Plans, marker)
	if werr != nil {
		reason := reasonWorkspaceFailed
		if aerr := apierr.From(werr); aerr.Code == apierr.CodeConfigChanged || aerr.Code == apierr.CodeConfigInvalid {
			reason = reasonConfigChanged
		}
		s.recordRuntimeAttempt(id, reason, 0)
		payload := runtimeVerifyResultPayload{
			ToolID: string(id), Status: "unconfirmed", Reason: reason,
			ModelCalls: 0, AttemptCount: s.runtimeEvidence.tool(string(id)).AttemptCount(),
			Blockers: []runtimeVerifyBlocker{{Reason: reason, Message: maskCredentials(werr.Error())}},
		}
		return payload, "", &loggedFailure{data: payload, code: apierr.CodeInternal}
	}
	defer func() { _ = os.RemoveAll(root) }()

	exec0 := s.profileTestExecute
	if exec0 == nil {
		exec0 = execProfileTestCLI
	}
	started := time.Now()
	// env 与 §4.6 共用同一构造（含 claude 代理卡的占位认证注入，见
	// [`Server.profileTestRouteInject`]）：§4.8 只支持 claude，缺占位时同样会在登录门
	// 上 `Not logged in`。本通道把 **facts.MarkedModel**（与写进工作区 settings.json 的
	// 标记值同源同值）一并交给构造：**CC 宿主形态**（CLAUDE_CODE_HOST_AUTH_ENV_VAR /
	// CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST 在场）下 CLI 只认进程 env、忽略 settings，
	// 该形态把带标记的档位值写进进程 env 才可达（R225③，不遮标记）；正常形态
	// settings 可达、构造不注入，标记仍只在 settings 里（现状零回归）。
	res := exec0(r.Context(), profileTestExecRequest{
		Root: root, Exe: exe, Args: recipe.args, Env: s.profileTestEnvMarked(root, id, recipe, facts.Plans, facts.MarkedModel),
	})
	finished := time.Now()

	reason := reasonVerified
	var matched *runtimeObservedRecord
	if res.TimedOut {
		reason = reasonVerifyTimeout
	} else if res.Err != nil {
		reason = reasonVerifyFailed
	} else {
		recs := s.observeRuntimeRequest(r.Context(), started)
		if len(recs) == 0 {
			// 代理在响应流结束后才提交记录：给一个**有界**的可见性窗口再读一次。
			// 等待本身不构成任何证据——读不到仍然是「未确认 + 原因」。
			time.Sleep(runtimeVerifyObservationWait)
			recs = s.observeRuntimeRequest(r.Context(), started)
		}
		if rec, why, ok := correlateRuntimeObservation(recs, []string{string(id)}, facts.Prefixes, marker, facts.ExpectedUpstream); ok {
			matched = &rec
		} else {
			reason = why
		}
	}

	payload := runtimeVerifyResultPayload{ToolID: string(id), Blockers: []runtimeVerifyBlocker{}}
	if matched == nil {
		payload.Status = "unconfirmed"
		payload.Reason = reason
		payload.ModelCalls = 1
		s.recordRuntimeAttempt(id, reason, 1)
		payload.AttemptCount = s.runtimeEvidence.tool(string(id)).AttemptCount()
		s.log.Info("受控验证未通过", "tool", string(id), "reason", reason,
			"timedOut", res.TimedOut, "exitCode", res.ExitCode)
		return payload, "", &loggedFailure{data: payload, code: runtimeVerifyFailureCode(reason)}
	}

	ev := runtimeEvidenceRecord{
		ToolID:     string(id),
		Source:     "controlled",
		ObtainedAt: finished.UTC().Format(time.RFC3339Nano),
		Instance: runtimeEvidenceInstance{
			ToolID: string(id), Kind: "controlled-run", CLI: recipe.cli, PID: res.PID,
			StartedAt: started.UTC().Format(time.RFC3339Nano), FinishedAt: finished.UTC().Format(time.RFC3339Nano),
		},
		Config: runtimeEvidenceConfig{
			ApplyTx: facts.ApplyTx, AppliedAt: facts.AppliedAt, ProfileID: facts.ProfileID,
			Files: facts.ConfigFiles,
			DecideKey: runtimeEvidenceDecideKey{
				Key: facts.DecideKey, MaskedValue: facts.DecideMasked, Digest: facts.DecideDigest,
				Path: runtimeVerifyConfigPath(facts),
			},
		},
		Workspace: ws,
		Request: runtimeEvidenceRequest{
			RecordID: matched.ID, At: matched.At.UTC().Format(time.RFC3339Nano), Status: matched.Status,
			App: matched.App, Prefix: matched.Prefix, DurationMs: matched.DurationMs,
			InputTokens: matched.InputTokens, OutputTokens: matched.OutputTokens,
		},
		Outbound: runtimeEvidenceOutbound{
			ModelRequested: matched.ModelRequested, ModelUpstream: matched.ModelUpstream,
		},
		Marker: marker,
		AppliesTo: runtimeEvidenceScopeRef{
			ToolID: string(id), ProfileID: facts.ProfileID, Prefixes: facts.Prefixes,
		},
		// UC-096 A6 additive：规则版本从被关联的用量记录照抄（同一份记账，不重算）。
		RuleVersion: matched.RuleVersion,
	}
	ev.Outbound.Endpoint.ClientBaseURL = facts.ClientBaseURL
	ev.Outbound.Endpoint.ObservedPrefix = matched.Prefix
	ev.Outbound.Endpoint.UpstreamOrigin = facts.UpstreamOrigin

	if perr := s.runtimeEvidence.putEvidence(string(id), ev); perr != nil {
		// 证据写盘失败 ⇒ 不宣称已验证（写盘成功不等于证据成立，反过来同样成立）。
		payload.Status = "unconfirmed"
		payload.Reason = reasonWorkspaceFailed
		payload.ModelCalls = 1
		s.recordRuntimeAttempt(id, reasonWorkspaceFailed, 1)
		payload.AttemptCount = s.runtimeEvidence.tool(string(id)).AttemptCount()
		return payload, "", &loggedFailure{data: payload, code: apierr.CodeStorage}
	}
	s.recordRuntimeAttempt(id, reasonVerified, 1)
	payload.Status = "verified"
	payload.Reason = reasonVerified
	payload.ModelCalls = 1
	payload.AttemptCount = s.runtimeEvidence.tool(string(id)).AttemptCount()
	payload.Observation = &ev.Request
	payload.Evidence = &ev
	s.log.Info("受控验证通过", "tool", string(id), "recordId", matched.ID,
		"modelRequested", matched.ModelRequested, "modelUpstream", matched.ModelUpstream)
	return payload, "", nil
}

// runtimeVerifyConfigPath 给出承载决定键/档名键的受管文件路径（记录用；路径不含配置值）。
func runtimeVerifyConfigPath(facts runtimeVerifyFacts) string {
	for _, plan := range facts.Plans {
		if plan.Path == facts.ConfigPath {
			return plan.Path
		}
	}
	return ""
}

// recordRuntimeAttempt 追加一条尝试（只写证据记账；写失败只记进程日志，不改判定）。
func (s *Server) recordRuntimeAttempt(id family.ID, reason string, calls int) {
	outcome := "unconfirmed"
	if reason == reasonVerified {
		outcome = "verified"
	}
	if err := s.runtimeEvidence.putAttempt(string(id), runtimeVerifyAttempt{
		At: time.Now().UTC().Format(time.RFC3339Nano), Outcome: outcome, Reason: reason, ModelCalls: calls,
	}); err != nil {
		s.log.Error("记录受控验证尝试失败", "tool", string(id), "err", err)
	}
}

// runtimeVerifyFailureCode 把尝试原因折成操作日志错误码（词表见 doc §4.8）。
//
// 全部**闸门前置拒绝**（额度用尽/不明、报价未知、预算越界：都是「未发起任何上游请求」
// 的拒绝）统一用 quota_exceeded——它就是「拒绝发起」那条码的同一语义，且同类必须同码
// （quote_unknown 此前落 internal 是同一类拒绝里的分叉，操作日志会因此把同一件事记成
// 两个码）；其余执行侧失败折 internal（与 §4.6 配置测试同口径）。
func runtimeVerifyFailureCode(reason string) apierr.Code {
	switch reason {
	case reasonQuotaExhausted, reasonQuotaUnknown, reasonQuoteUnknown, reasonBudgetExceeded:
		return apierr.CodeQuotaExceeded
	default:
		return apierr.CodeInternal
	}
}

// attemptCount 是记账条数的小工具。
func (rec toolRuntimeEvidenceDoc) AttemptCount() int { return len(rec.Attempts) }

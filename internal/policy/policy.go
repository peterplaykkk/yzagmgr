package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/privfile"
)

// 契约默认值：网关没给窗口字段时沿用 v1 的口径，而不是给 0
// （0 会让工具侧的上下文窗口设置变成一个「什么都不能发」的配置）。
const (
	defaultMaxInputTokens  = 1_000_000
	defaultMaxOutputTokens = 131_072
)

// Model 是契约 §3 里 models[] 的形状。字段是封闭的：多一个字段都会出现在响应里，
// 因此窗口/思考能力这些内部字段走 ModelSpec。
type Model struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	SupportsImages bool   `json:"supportsImages"`
}

// ModelSpec 是策略条目的全文（受管卡与家族写链需要的输入）。
type ModelSpec struct {
	ID                string
	Name              string
	IsDefault         bool
	MaxInputTokens    int
	MaxOutputTokens   int
	SupportsReasoning bool
	SupportsToolCall  bool
	SupportsImages    bool
}

// Policy 是 GET /api/v1/policy 的响应体，也是磁盘上持久化的口径。
// （UC-034 下发段 Deliveries 已随登录链删除——design §1.1。）
type Policy struct {
	Version      int                        `json:"version"`
	DefaultModel string                     `json:"defaultModel"`
	Models       []Model                    `json:"models"`
	Catalogs     map[string]json.RawMessage `json:"catalogs"`

	// fetchedAt 与 specs 是内部状态：契约的响应体里没有它们（`json:"-"`），
	// 但它们必须随策略一起持久化，否则离线兜底会丢掉窗口字段与来源时间。
	FetchedAt string      `json:"-"`
	Specs     []ModelSpec `json:"-"`
	Origin    string      `json:"-"`
}

// Default 返回策略的默认模型。
func (p *Policy) Default() (Model, bool) {
	if p == nil {
		return Model{}, false
	}
	for _, model := range p.Models {
		if model.ID == p.DefaultModel {
			return model, true
		}
	}
	if len(p.Models) > 0 {
		return p.Models[0], true
	}
	return Model{}, false
}

// ManagedModels 返回**受管模型集合**：恒为单元素，即策略 default。
//
// 这是用户裁定的产品语义：本产品只会把 default 写进工具配置，也只会以受管身份展示它。
// 任何消费方都必须走这个选择器，不得直接遍历 Models（UC-018 A4）。
func (p *Policy) ManagedModels() []Model {
	model, ok := p.Default()
	if !ok {
		return []Model{}
	}
	return []Model{model}
}

// Spec 按模型 id 取条目全文。
func (p *Policy) Spec(id string) (ModelSpec, bool) {
	if p == nil {
		return ModelSpec{}, false
	}
	for _, spec := range p.Specs {
		if spec.ID == id {
			return spec, true
		}
	}
	return ModelSpec{}, false
}

// Catalog 返回某入口前缀的目录载荷。
//
// 命中条件是「键存在，且值是一个非空 JSON **对象**」：
//   - 网关侧的判据只是 `len(bytes.TrimSpace(v)) > 0`，那是为了不写出空 body；
//   - 本产品必须更严一格：契约（§J.2/J.3.1）说的「目录」就是一个 JSON 对象，
//     而 `null` / 裸标量 / 带引号的空白串在网关侧会被判成命中并原样下发，
//     到了客户端却是必然解码失败的目录。既然网关的写路径本就只接受对象，
//     这里要求对象既不放宽也不收紧合法载荷，只是把坏载荷挡在回落到既有行为上。
//
// 命中时返回的是**原始字节**，调用方必须原样写出，不得再经过编码
// （那是逐字节等同契约的落点）。本方法只做判定，绝不改写返回的字节。
func (p *Policy) Catalog(prefix string) (json.RawMessage, bool) {
	if p == nil || len(p.Catalogs) == 0 {
		return nil, false
	}
	raw, ok := p.Catalogs[prefix]
	if !ok || !isJSONObject(raw) {
		return nil, false
	}
	return raw, true
}

// isJSONObject 报告原始字节是否是一个 JSON 对象（跳过前导空白后以 { 开头）。
// 只看首字节：这里要回答的是「能不能拿来当目录」，而不是「整段是否合法」——
// 完整校验是网关写路径的职责，客户端重复解析一遍只会白白变大。
func isJSONObject(raw json.RawMessage) bool {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			return true
		default:
			return false
		}
	}
	return false
}

// Options 构造 Store。
type Options struct {
	Clock func() time.Time
}

// Store 是策略的持有者：内存缓存 + 本地策略文件。
//
// 源已从「匿名拉网关 /api/app/models」切换为**本地策略文件**（design §2.3）：
// path 即 env.PolicyFile()（`<data_root>/managed/policy.json`，可由
// `YZAGMGR_POLICY_FILE` 在装配期播种），Refresh = 重读该文件，全程零出网。
type Store struct {
	path   string
	clock  func() time.Time
	mu     sync.Mutex
	policy *Policy
}

// NewStore 构造策略仓库。path 是 env.PolicyFile()。
func NewStore(path string, opts Options) *Store {
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Store{path: path, clock: clock}
}

// Load 返回当前已知的最好策略，**不发网络请求**。
// 从未见过合法策略时返回 (nil, nil)：策略缺失是一种状态而不是错误，调用方自行决定
// 要不要拒绝生成步骤（本包不发明错误码去表达它）。
func (s *Store) Load() (*Policy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.policy != nil {
		return s.policy, nil
	}
	policy, err := s.loadDisk()
	if err != nil {
		return nil, err
	}
	s.policy = policy
	return policy, nil
}

// Refresh 从本地策略文件重读并采纳（源=本地文件，零网络请求，design §2.3）。
//
// 三段判定（都不打扰用户）：
//   - 读不到（文件缺席/IO 失败）→ 退回上一份可用策略；
//   - 读到了但非法（缺版本、models 为空、非 JSON）→ 退回上一份可用策略，**不改内存**；
//   - 读到了且合法 → 采纳（盘上文件即事实源；版本回退是编辑者在文件上的显式动作，
//     不再做版本单调拦截——那道闸的原理由是网关侧回滚，网关已不存在）。
//
// 只有「从未见过任何合法策略」才会失败，错误码恒为 gateway_unexpected_response
// （保留既有契约码，措辞点明本地文件）。
func (s *Store) Refresh(ctx context.Context) (*Policy, error) {
	_ = ctx // 源是本地文件：没有要等待的网络调用，但保留 ctx 以沿用调用方签名。
	next, err := s.loadDisk()
	if err != nil {
		return s.fallback(err)
	}
	if next == nil {
		return s.fallback(apierr.New(apierr.CodeGatewayUnexpectedResponse,
			"模型策略暂不可用：本地策略文件缺失或结构非法"))
	}
	s.mu.Lock()
	s.policy = next
	s.mu.Unlock()
	return next, nil
}

// current 返回内存或磁盘上的策略（调用方必须持锁）。
func (s *Store) current() (*Policy, error) {
	if s.policy != nil {
		return s.policy, nil
	}
	return s.loadDisk()
}

// fallback 在重读失败时给出上一份可用策略；一份都没有才把错误上抛。
func (s *Store) fallback(cause error) (*Policy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	policy, err := s.current()
	if err == nil && policy != nil {
		return policy, nil
	}
	if err == nil {
		err = cause
	}
	// 不退化成 internal：调用方需要知道是「策略读不出」而不是程序内部错误。
	return nil, err
}

// parse 解析并校验网关下发的策略。校验不通过即整体拒绝：半份策略比没有策略更危险。
func parse(raw json.RawMessage, origin string) (*Policy, error) {
	var payload struct {
		Version      json.RawMessage            `json:"version"`
		DefaultModel string                     `json:"defaultModel"`
		Models       []json.RawMessage          `json:"models"`
		Catalogs     map[string]json.RawMessage `json:"catalogs"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, apierr.New(apierr.CodeGatewayUnexpectedResponse, "模型策略不是预期结构")
	}
	version, err := parseVersion(payload.Version)
	if err != nil {
		return nil, err
	}

	specs := make([]ModelSpec, 0, len(payload.Models))
	models := make([]Model, 0, len(payload.Models))
	for _, entry := range payload.Models {
		spec, ok := parseModel(entry)
		if !ok {
			continue
		}
		specs = append(specs, spec)
		models = append(models, Model{ID: spec.ID, Name: spec.Name, SupportsImages: spec.SupportsImages})
	}
	if len(specs) == 0 {
		return nil, apierr.New(apierr.CodeGatewayUnexpectedResponse, "模型策略没有可用模型")
	}

	return &Policy{
		Version:      version,
		DefaultModel: defaultModelID(payload.DefaultModel, specs),
		Models:       models,
		Catalogs:     payload.Catalogs,
		Specs:        specs,
		Origin:       origin,
	}, nil
}

// parseVersion 接受数字或数字字符串两种版本表达。
func parseVersion(raw json.RawMessage) (int, error) {
	if len(raw) == 0 {
		return 0, apierr.New(apierr.CodeGatewayUnexpectedResponse, "模型策略缺少版本号")
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		if value, err := strconv.Atoi(number.String()); err == nil && value > 0 {
			return value, nil
		}
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if value, err := strconv.Atoi(strings.TrimSpace(text)); err == nil && value > 0 {
			return value, nil
		}
	}
	return 0, apierr.New(apierr.CodeGatewayUnexpectedResponse, "模型策略的版本号非法")
}

// parseModel 解析一个模型条目。条目可以是字符串（只有名字）或对象；两者都要认，
// 因为网关历史上两种都出现过。
func parseModel(entry json.RawMessage) (ModelSpec, bool) {
	var name string
	if err := json.Unmarshal(entry, &name); err == nil {
		name = strings.TrimSpace(name)
		if name == "" {
			return ModelSpec{}, false
		}
		return ModelSpec{
			ID:              name,
			Name:            name,
			MaxInputTokens:  defaultMaxInputTokens,
			MaxOutputTokens: defaultMaxOutputTokens,
		}, true
	}

	var payload struct {
		ID                string `json:"id"`
		Name              string `json:"name"`
		Label             string `json:"label"`
		Model             string `json:"model"`
		IsDefault         bool   `json:"isDefault"`
		MaxInputTokens    *int   `json:"maxInputTokens"`
		MaxOutputTokens   *int   `json:"maxOutputTokens"`
		SupportsReasoning bool   `json:"supportsReasoning"`
		SupportsToolCall  bool   `json:"supportsToolCall"`
		SupportsImages    bool   `json:"supportsImages"`
	}
	if err := json.Unmarshal(entry, &payload); err != nil {
		return ModelSpec{}, false
	}
	id := firstNonEmpty(payload.ID, payload.Name, payload.Model)
	if id == "" {
		return ModelSpec{}, false
	}
	return ModelSpec{
		ID:                id,
		Name:              firstNonEmpty(payload.Name, payload.Label, id),
		IsDefault:         payload.IsDefault,
		MaxInputTokens:    positiveOr(payload.MaxInputTokens, defaultMaxInputTokens),
		MaxOutputTokens:   positiveOr(payload.MaxOutputTokens, defaultMaxOutputTokens),
		SupportsReasoning: payload.SupportsReasoning,
		SupportsToolCall:  payload.SupportsToolCall,
		SupportsImages:    payload.SupportsImages,
	}, true
}

// defaultModelID 决定 default：显式字段 > isDefault 标记 > 首个模型。
// 三种表达都要认，因为契约暴露的是 defaultModel 字符串，而网关当前只用 isDefault 标记。
func defaultModelID(explicit string, specs []ModelSpec) string {
	if explicit != "" {
		for _, spec := range specs {
			if spec.ID == explicit {
				return explicit
			}
		}
	}
	for _, spec := range specs {
		if spec.IsDefault {
			return spec.ID
		}
	}
	return specs[0].ID
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func positiveOr(value *int, fallback int) int {
	if value == nil || *value <= 0 || math.IsNaN(float64(*value)) {
		return fallback
	}
	return *value
}

// ── 磁盘形态 ───────────────────────────────────────────────────────────────

// policyFile 是磁盘上的形状：一段固定表头 + 网关响应的**原始字节**。
//
// 为什么把 payload 存成原始字节而不是结构化字段：目录（catalogs）的契约是「逐字节等同」，
// 而 json.Marshal 会对 json.RawMessage 做压缩与 `<>&` 转义，那会让再一次读出来的目录
// 与网关下发的字节不同。存整段原文同时也让网关新增字段无需客户端改动即可保留。
type policyFile struct {
	Schema    int             `json:"schema"`
	Origin    string          `json:"origin"`
	FetchedAt string          `json:"fetchedAt"`
	Payload   json.RawMessage `json:"payload"`
}

const policySchema = 1

func (s *Store) loadDisk() (*Policy, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, apierr.Wrap(apierr.CodeStorage, err, "读取模型策略失败")
	}
	var saved policyFile
	if err := json.Unmarshal(raw, &saved); err != nil || saved.Schema != policySchema {
		return nil, nil
	}
	policy, err := parse(saved.Payload, saved.Origin)
	if err != nil {
		return nil, nil
	}
	policy.FetchedAt = saved.FetchedAt
	return policy, nil
}

// persist 落盘：表头用 json 编码（含 origin 等需要转义的字符串），payload 手工拼接，
// 保证它的字节与源文件载荷的一模一样（写盘纪律经 internal/privfile：0600+原子替换）。
func (s *Store) persist(payload json.RawMessage, policy *Policy) error {
	if len(bytes.TrimSpace(payload)) == 0 {
		return apierr.New(apierr.CodeStorage, "模型策略为空，拒绝落盘")
	}
	head, err := json.Marshal(struct {
		Schema    int    `json:"schema"`
		Origin    string `json:"origin"`
		FetchedAt string `json:"fetchedAt"`
	}{Schema: policySchema, Origin: policy.Origin, FetchedAt: policy.FetchedAt})
	if err != nil {
		return apierr.New(apierr.CodeStorage, "模型策略序列化失败")
	}
	head = head[:len(head)-1] // 去掉表头的右花括号，接上 payload 字段

	var document bytes.Buffer
	document.Grow(len(head) + len(payload) + 16)
	document.Write(head)
	document.WriteString(`,"payload":`)
	document.Write(payload)
	document.WriteByte('}')

	if dir := filepath.Dir(s.path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return apierr.Wrap(apierr.CodeStorage, err, "建立策略目录失败")
		}
	}
	return privfile.WritePrivateFile(s.path, document.Bytes(), 0o600)
}

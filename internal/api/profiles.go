// 档位卡记账（UC-018 / doc/api-v1.md §4.2）。
//
// 为什么它在 API 层：契约要求「档位卡」这个一等对象，但仓库内**没有** profiles 服务包
// （`internal/family` 只到「家族 → 受管文件 → 目标字节」这一层，没有卡片这一概念）。
// 本文件承载记账与卡片派生，把「卡片 → 目标字节」的翻译仍交给 `internal/family`，
// 因此这里不重复任何配置层判断。交付说明里登记为服务包缺口。
//
// 三条纪律：
//  1. **受管卡与官方卡是派生的、不是存储的**：它们由策略与家族能力（OfficialSeeded）算出，
//     因此永远不会出现「第二张受管卡」（UC-018 A2）。
//  2. **生效项由盘上字节判定**：`applied` 只是记账，与盘上不一致时报 drift 并清空
//     `activeProfileId`（UC-018 A7、契约 §4.1）。
//  3. 卡片载荷不含明文令牌：`settings` 只有可编辑键（`editableSettings`），凭据以引用表达
//     （UC-018 A12）。
package api

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

// 卡片类别（契约 §4.2 的封闭取值）。
const (
	ProfileKindManaged  = "managed"
	ProfileKindOfficial = "official"
	ProfileKindUser     = "user"
	// ProfileKindRouter 是「本地 router 形态」派生卡（UC-024）：一族恰一张，
	// 只给实现 [`family.RouterForm`] 的家族（claude/codex/opencode/pi）出。
	ProfileKindRouter = "router"
)

// OfficialProfileID 是官方卡的固定 id。它不是一个 uuid，因为每个工具最多只有一张。
const OfficialProfileID = "official"

// ManagedProfilePrefix 是受管卡 id 的前缀；后缀是网关用户 id（openclaw/opencode/pi
// 的 live 配置里托管 provider 也用同一形态 `yzagmgr-<uid>`，因此卡片与盘上互相对得上）。
const ManagedProfilePrefix = "yzagmgr-"

// RouterProfileID 是 router 形态卡的固定 id（cli-rewrite-router.sh 按 kind 发现、
// pid 动态读取；这里给稳定值，界面与脚本都不写死消费方侧的第二套命名）。
const RouterProfileID = ManagedProfilePrefix + "router"

// editableSettingsKeys 是 `PUT .../profiles/{pid}` 允许改的 settings 键。
//
// 只有 modelId：受管模板的其余内容由策略派生（UC-018 A8），其余键一律只读回显。
// 自建卡的独立配置三要素（endpoint/credRef/models）是 **Profile 顶层键**（UC-032），
// 不进 settings——settings 的键级可编辑语义不因此扩张。
var editableSettingsKeys = []string{"modelId"}

// Profile 是契约 §4.2 的卡片形状。
//
// `Endpoint` / `CredRef` 是 UC-032 的 additive 键（`omitempty`：**缺席**与空值对
// managed/official/router 派生卡是契约——「键缺席，不伪造空值」；自建卡按登记内容回显）。
// `Models` 沿既有键：自建卡独立清单在册时逐项回显（含显式空清单 `[]`），
// 否则回落策略 default 单元素（legacy 语义不动）。
//
// `MaxOutputTokens` 是 UC-040 的 additive 键（`omitempty`：**缺席** = 该卡没有声明
// 最大输出能力位，派生卡与未声明的自建卡都不出现这个键——键在场即表示「用户声明过」）。
//
// `Rev` / `Capabilities` / `CapabilityAcks` / `CopiedFrom` 是 UC-094 B-5 的 additive 键
// （kind=user 卡专属，除 `Rev` 外都只在有内容时出现）：
//   - Rev：内容修订指纹（乐观并发的「基于版本保存」用它）；
//   - Capabilities：能力声明回显（缺席 = 未声明）；
//   - CapabilityAcks：已确认的能力检查记录（缺席 = 没有确认）；
//   - CopiedFrom：复制来源提示（缺席 = 不是从派生卡复制来的）。
//
// `ReadOnlyReason` 是 UC-094 A6 的 additive 键：**不可编辑的卡**（官方/受管/router/
// 下发）带一句机器可读的只读原因；自建卡缺席（它可编辑）。
//
// `CapabilityCheck` 是 UC-094 A7 的 additive 键：能力校验报告，**只在建卡/改卡响应**
// 出现（列表/读取不带——它是「本次保存的校验结果」，不是卡的常驻属性）。
type Profile struct {
	ID                 string                `json:"id"`
	Name               string                `json:"name"`
	Kind               string                `json:"kind"`
	Active             bool                  `json:"active"`
	Tier               string                `json:"tier"`
	ModelID            string                `json:"modelId"`
	Models             []string              `json:"models"`
	Editable           bool                  `json:"editable"`
	Deletable          bool                  `json:"deletable"`
	HasUsageScript     bool                  `json:"hasUsageScript"`
	Settings           map[string]any        `json:"settings"`
	EditableSettings   []string              `json:"editableSettings"`
	Endpoint           string                `json:"endpoint,omitempty"`
	EndpointIsFullBase bool                  `json:"endpointIsFullBase,omitempty"`
	CredRef            string                `json:"credRef,omitempty"`
	MaxOutputTokens    int                   `json:"maxOutputTokens,omitempty"`
	Rev                string                `json:"rev,omitempty"`
	Capabilities       *userCardCapabilities `json:"capabilities,omitempty"`
	CapabilityAcks     []capabilityAck       `json:"capabilityAcks,omitempty"`
	CopiedFrom         *copiedFromHint       `json:"copiedFrom,omitempty"`
	ReadOnlyReason     string                `json:"readOnlyReason,omitempty"`
	CapabilityCheck    *capabilityReport     `json:"capabilityCheck,omitempty"`
}

// profileListPayload 是 §4.2 的列表响应。
type profileListPayload struct {
	Items           []Profile `json:"items"`
	Total           int       `json:"total"`
	ActiveProfileID string    `json:"activeProfileId"`
}

// profileStore 是自建卡与顺序的持久化记账。
type profileStore struct {
	mu   sync.Mutex
	path string
	doc  profileDoc
}

// profileDoc 是盘上形状。带 schema 版本：读到不认识的版本按「没有记账」处理而不是误读。
type profileDoc struct {
	Schema int                       `json:"schema"`
	Tools  map[string]toolProfileDoc `json:"tools"`
}

// toolProfileDoc 是单个工具的记账。
type toolProfileDoc struct {
	// Order 是自建卡的展示顺序（受管卡恒首位、官方卡恒次位，由渲染时保证）。
	Order []string `json:"order"`
	// User 是自建卡本体。
	User []userProfileDoc `json:"user"`
	// Applied 是最近一次成功应用的档位 id（记账口径，判定仍以盘上字节为准）。
	Applied string `json:"applied"`
	// AppliedTx / AppliedAt 是最近一次成功应用的**事务身份与时刻**（UC-092 A10 additive）。
	//
	// 为什么运行时证据需要它：证据要能失效于「**再次应用**」——而幂等重放（字节相同）
	// 改变不了盘上指纹，只有事务身份能把它区分出来；`appliedAt` 供界面说明时刻，
	// `appliedTx` 是判据本体（apply 时取租约 ID：同一份计划每次事务都是新身份）。
	// 老记账文件缺这两个键 ⇒ 空值 ⇒ 任何旧证据都关联不上（安全方向：宁可未确认）。
	AppliedTx string `json:"appliedTx,omitempty"`
	AppliedAt string `json:"appliedAt,omitempty"`
	// Ensured 是「受管卡已建档」的记账，值是建档时的受管卡 id（`yzagmgr-<uid>`）。
	//
	// 为什么与 Applied 分两个字段：它们是**两条轴**——
	//   - Applied 回答「最近一次成功应用到盘上的是哪张卡」，由 setApplied 在真写入后写；
	//   - Ensured 回答「这个受管单元的受管卡有没有建过档」，由 ensureManaged 写，
	//     与 live 盘上状态无关（建档**绝不写 live**，UC-001 A6）。
	// 合成一个字段就会出现两种错：拿 Applied 当建档标志，「只建档、未应用」永远判成
	// 首次建档（每次问都写一次盘）；拿 Applied 的写入当建档，「切过一次档」又会被
	// 误读成「刚建过档」而漏建。加字段的代价是老记账文件首次读到时空值 ⇒ 升级后
	// 第一次 ensure 会如实报一次 created，只写一条记账、不碰任何 live 文件。
	Ensured string `json:"ensured"`
}

// userProfileDoc 是一条自建卡（个人配置）的盘上内容。
type userProfileDoc struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	ModelID  string         `json:"modelId"`
	Settings map[string]any `json:"settings"`
	// Endpoint / CredRef / Models 是 UC-032 的独立配置三要素（kind=user 卡专属）。
	//   - Endpoint：自建 provider 的端点（空 = 未登记，启用走托管网关语义）；
	//   - EndpointIsFullBase：true 表示 endpoint 已含 family 所需完整 API 路径；
	//     缺席/false 保持旧 family suffix 行为（例如默认追加 `/v1`）。
	//   - CredRef：托管凭据引用（yzagmgr-managed-token://<uid>，**只落引用**，UC-018 A12）；
	//   - Models：独立模型清单。**刻意不带 omitempty**——nil（legacy/未登记）与
	//     `[]`（显式空清单，启用时报「无模型可选」）必须在盘上可分辨。
	Endpoint           string   `json:"endpoint,omitempty"`
	EndpointIsFullBase bool     `json:"endpointIsFullBase,omitempty"`
	CredRef            string   `json:"credRef,omitempty"`
	Models             []string `json:"models"`
	// MaxOutputTokens 是 UC-040 的自建卡能力声明（上游最大输出，逐字采用不夹取）。
	// **0 = 未声明**（`omitempty`：盘上不出现该键），因此它不需要「nil 与空值可辨」
	// 那套（清单类的 nil/[] 语义在这里没有对应物：0 个输出 token 不是合法声明）。
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`
	// Rev 是本文档**内容**的修订指纹（UC-094 A5 additive，`v1-` + sha256 前 16 hex）。
	//
	// 口径：指纹是内容的纯函数（[`cardRevOf`] 每次写入重算），不是单调计数器——
	// 同内容恒同 rev，因此「保存基于的版本没变」可以被精确判定。回读恒按当前内容重算，
	// 手改盘上的 rev 键不影响任何判定（它只落盘供人读）。老文件缺该键 = 空值，
	// 不影响判定（判定永远用重算值）。
	Rev string `json:"rev,omitempty"`
	// Prev 是**上一版内容**的快照（UC-094 A5 additive）：版本竞争被拒时，它是
	// 「两版可读面」里的「你基于的那一版」的载体（服务端保留恰一版；再早的版本不保留，
	// 响应里如实缺席，不伪造）。内容未变化的保存不动它（历史不是「保存过几次」）。
	Prev *userCardVersion `json:"prev,omitempty"`
	// Capabilities 是能力声明（UC-094 A7 additive，可选）：本卡所选模型「要满足的能力」。
	// 缺席 = 未声明（旧卡/未填都同形，不伪造空对象）。
	Capabilities *userCardCapabilities `json:"capabilities,omitempty"`
	// CapabilityAcks 是「能力检查已获显式确认」的落卡记录（UC-094 A7 additive）。
	// 键缺席 = 没有任何确认；每条绑定 (code, model, capsHash)，声明或模型变了即失效。
	CapabilityAcks []capabilityAck `json:"capabilityAcks,omitempty"`
	// CopiedFrom 是「复制自哪张卡」的来源提示（UC-094 A6 additive）。
	// **只有从派生卡（当前是下发卡）复制出来的卡才带**：复制是产生新个人配置，
	// 来源提示保证它不被伪装成下游的原对象；用户卡之间的复制不带（同kind 无来源歧义）。
	CopiedFrom *copiedFromHint `json:"copiedFrom,omitempty"`
}

// userCardVersion 是「某一版卡片内容」的可读快照（UC-094 A5 的冲突面载体）。
//
// 它只含**内容**字段（不含 acks 这类元数据）：两版差异回答的是「配置本身哪里变了」。
type userCardVersion struct {
	Rev                string                `json:"rev"`
	ID                 string                `json:"id"`
	Name               string                `json:"name"`
	ModelID            string                `json:"modelId"`
	Settings           map[string]any        `json:"settings,omitempty"`
	Endpoint           string                `json:"endpoint,omitempty"`
	EndpointIsFullBase bool                  `json:"endpointIsFullBase,omitempty"`
	CredRef            string                `json:"credRef,omitempty"`
	Models             []string              `json:"models,omitempty"`
	MaxOutputTokens    int                   `json:"maxOutputTokens,omitempty"`
	Capabilities       *userCardCapabilities `json:"capabilities,omitempty"`
}

// userCardCapabilities 是自建卡的能力声明（UC-094 A7 additive，可选）。
//
// 语义：「这张卡所选模型**要满足的能力**」。保存/切换模型时服务端把它与所选模型的
// 策略事实校验，不兼容的组合被标出（机器可读 checks）；未获显式确认前**拒绝保存**
// ——「用户确认修正前不能保存伪能力」。字段封闭（严格解码拒未知键）：
//
//   - Modalities：模态声明，取值 text|vision。含 vision 而所选模型事实
//     supportsImages=false ⇒ 文本/视觉不兼容（vision_unsupported）；
//     text 是任何模型都具备的基础模态，不单独校验。
//   - ThinkingTier：思考档位声明，取值 = 上游 effort 白名单档（none|minimal|low|
//     medium|high|xhigh|max，与请求映射的 effort 词表同源）。非 none 档而所选模型事实
//     supportsReasoning=false ⇒ 思考档位不兼容（thinking_unsupported）。
type userCardCapabilities struct {
	Modalities   []string `json:"modalities,omitempty"`
	ThinkingTier string   `json:"thinkingTier,omitempty"`
}

// capabilityAck 是「用户已对某条能力检查显式确认」的落卡记录（UC-094 A7）。
//
// 绑定三元组 (code, model, capsHash)：声明内容或所选模型一变，旧确认即失效
// （capsHash 是「声明 + 模型」的指纹），不会让一条旧确认替新声明背书。
type capabilityAck struct {
	Code     string `json:"code"`
	Model    string `json:"model,omitempty"`
	CapsHash string `json:"capsHash,omitempty"`
	At       string `json:"at,omitempty"`
}

// copiedFromHint 是「复制自哪张卡」的来源提示（UC-094 A6 additive）。
//
// 它是**提示**而不是身份：复制产生的是新的个人配置（kind=user），来源字段只说明
// 它由哪张派生卡复制而来——「可复制成个人但**不伪装来源**」由它承载。
type copiedFromHint struct {
	// Kind 是来源类别（当前只有 delivered）。
	Kind string `json:"kind"`
	// ToolID / ProfileID / Name 是来源卡的归属与当时的可读名。
	ToolID    string `json:"toolId,omitempty"`
	ProfileID string `json:"profileId,omitempty"`
	Name      string `json:"name,omitempty"`
	// At 是复制时刻（RFC3339，UTC）。
	At string `json:"at,omitempty"`
}

// userCardSpec 是新建自建卡的完整内容（UC-032 三要素 + 既有字段 + UC-040 能力位）。
type userCardSpec struct {
	Name               string
	ModelID            string
	Endpoint           string
	EndpointIsFullBase bool
	CredRef            string
	Models             []string // nil = 不登记独立清单（legacy 语义）
	Settings           map[string]any
	// MaxOutputTokens 0 = 不声明（与 `Profile.maxOutputTokens` 同口径）。
	MaxOutputTokens int
	// Capabilities 是 UC-094 A7 的能力声明（nil = 未声明）。
	Capabilities *userCardCapabilities
	// CapabilityAcks 是随本次保存落卡的能力确认（建卡确认与从源卡复制都不继承）。
	CapabilityAcks []capabilityAck
	// CopiedFrom 是复制来源提示（UC-094 A6；nil = 不是从派生卡复制）。
	CopiedFrom *copiedFromHint
}

// userCardPatch 是改卡的部分更新。指针 nil = 本次不改该字段。
type userCardPatch struct {
	Name               *string
	ModelID            *string
	Endpoint           *string // 非 nil 且值非空 ⇒ 覆盖（空值 = 保持原值，routes set 同口径）
	EndpointIsFullBase *bool
	CredRef            *string   // 非 nil 且值非空 ⇒ 覆盖
	Models             *[]string // 非 nil ⇒ 整体替换（含显式清空）
	Settings           map[string]any
	// MaxOutputTokens 非 nil 且值 >0 ⇒ 覆盖、==0 ⇒ **清除声明**（REST 层拒负值/越界；
	// 0 在这里只可能是内部构造，按「不改」处理——盘上 0 与「未声明」同形）。
	MaxOutputTokens *int
	// BaseRev 是乐观并发的「保存所基于的版本」（UC-094 A5）。
	// nil 或空串 = 不带闸（legacy 行为：最后写入者获胜）；非空且与当前 rev 不符 ⇒
	// 整体拒绝（version_conflict + 两版可读面），不产生任何写入。
	BaseRev *string
	// Capabilities 非 nil ⇒ 覆盖能力声明（指向 nil = 清为未声明，指针 nil = 不改）。
	Capabilities **userCardCapabilities
	// CapabilityAcks 非 nil ⇒ 整组替换确认记录（由能力闸门算好的「仍有效 + 本次确认」集）。
	CapabilityAcks *[]capabilityAck
}

const profileSchema = 1

// newProfileStore 打开（或初始化）档位卡记账文件。
func newProfileStore(env configEnv) (*profileStore, error) {
	path := filepath.Join(env.ManagedDir(), "profiles.json")
	s := &profileStore{path: path, doc: profileDoc{Schema: profileSchema, Tools: map[string]toolProfileDoc{}}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// configEnv 是这里需要的最小环境接口面（只用到数据根下的受管目录）。
type configEnv interface {
	ManagedDir() string
}

func (s *profileStore) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return apierr.Wrap(apierr.CodeStorage, err, "读取档位卡记账失败")
	}
	var doc profileDoc
	if err := json.Unmarshal(b, &doc); err != nil || doc.Schema != profileSchema {
		// 记账损坏不阻塞启动：按空记账处理，盘上的 live 配置不受影响（判定本来就看盘）。
		return nil
	}
	if doc.Tools == nil {
		doc.Tools = map[string]toolProfileDoc{}
	}
	s.doc = doc
	return nil
}

// save 原子落盘（同目录临时文件 + rename），保证读者只看到完整文档。
func (s *profileStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "建立档位卡目录失败")
	}
	blob, err := json.MarshalIndent(s.doc, "", "  ")
	if err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "编码档位卡记账失败")
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".profiles-*.json")
	if err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "创建临时文件失败")
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(append(blob, '\n')); err != nil {
		_ = tmp.Close()
		return apierr.Wrap(apierr.CodeStorage, err, "写入档位卡记账失败")
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return apierr.Wrap(apierr.CodeStorage, err, "设置档位卡记账权限失败")
	}
	if err := tmp.Close(); err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "关闭临时文件失败")
	}
	if err := os.Rename(name, s.path); err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "替换档位卡记账失败")
	}
	return nil
}

// toolLocked 取（必要时创建）某工具的记账条目。
func (s *profileStore) toolLocked(id string) toolProfileDoc {
	t, ok := s.doc.Tools[id]
	if !ok {
		t = toolProfileDoc{}
	}
	return t
}

// snapshot 返回某工具的记账副本（调用方不需要持锁）。
func (s *profileStore) snapshot(id string) toolProfileDoc {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.toolLocked(id)
	t.Order = append([]string(nil), t.Order...)
	t.User = append([]userProfileDoc(nil), t.User...)
	return t
}

// ensureManaged 幂等登记受管卡的记账（UC-018 A2：重复调用不产生第二张）。
// 返回是否为首次创建。
//
// 幂等判据是 `Ensured`（**建档**这条轴），不是 `Applied`（**当前生效**那条轴，
// 见 toolProfileDoc 的说明）。原实现要求 `t.Applied == managedID` 才算已建档，
// 而 ensureManaged 自己从不写 Applied ⇒ 守卫不可达 ⇒ 九族每次调用都报 created、
// 每次都重写 profiles.json（界面因此每次都谎报「已新建十张卡」）。
//
// 「幂等」在盘上的可判定形式就是**这次没有产生写入**：已经是同一张受管卡就直接返回，
// 不落盘（连 mtime 都不动）。uid 变了（受管卡 id 由 uid 派生）才算另一张卡、重新建档。
func (s *profileStore) ensureManaged(id, managedID, defaultModel string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.toolLocked(id)
	if t.Ensured == managedID {
		return false, nil
	}
	t.Ensured = managedID
	s.doc.Tools[id] = t
	if err := s.saveLocked(); err != nil {
		return false, err
	}
	return true, nil
}

// setApplied 记下最近一次成功应用的档位 id 与事务身份。
//
// txID 为租约 ID（本次事务的唯一身份）；profileID 为空（恢复/重置收敛）时事务身份一并清空。
func (s *profileStore) setApplied(id, profileID, txID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.toolLocked(id)
	t.Applied = profileID
	if profileID == "" {
		t.AppliedTx = ""
		t.AppliedAt = ""
	} else {
		t.AppliedTx = txID
		if at.IsZero() {
			at = time.Now()
		}
		t.AppliedAt = at.UTC().Format(time.RFC3339Nano)
	}
	s.doc.Tools[id] = t
	return s.saveLocked()
}

// create 新建一张自建卡（UC-032：可承载端点/凭据引用/模型清单三要素）。
//
// 同工具重名在**服务端权威**拒绝（UC-094 A2 后端半）：重名卡在任何界面都是
// 「两个看起来一样的目标」，禁用重名比让用户事后分辨更省事。判据 = 同工具的自建卡
// 名称（trim 后的精确串；派生卡的名称不参与——它们不由用户改）。
func (s *profileStore) create(id string, spec userCardSpec) (userProfileDoc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.toolLocked(id)
	cardID, err := newProfileID()
	if err != nil {
		return userProfileDoc{}, err
	}
	if err := checkCardNameUnique(t, spec.Name, ""); err != nil {
		return userProfileDoc{}, err
	}
	card := userProfileDoc{
		ID:                 cardID,
		Name:               spec.Name,
		ModelID:            spec.ModelID,
		Settings:           spec.Settings,
		Endpoint:           spec.Endpoint,
		EndpointIsFullBase: spec.EndpointIsFullBase,
		CredRef:            spec.CredRef,
		Models:             spec.Models,
		MaxOutputTokens:    spec.MaxOutputTokens,
		Capabilities:       spec.Capabilities,
		CapabilityAcks:     spec.CapabilityAcks,
		CopiedFrom:         spec.CopiedFrom,
	}
	card.Rev = cardRevOf(card)
	t.User = append(t.User, card)
	t.Order = append(t.Order, card.ID)
	s.doc.Tools[id] = t
	if err := s.saveLocked(); err != nil {
		return userProfileDoc{}, err
	}
	return card, nil
}

// find 取一张自建卡。
func (s *profileStore) find(id, pid string) (userProfileDoc, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.toolLocked(id).User {
		if c.ID == pid {
			return c, true
		}
	}
	return userProfileDoc{}, false
}

// cardRef 是「某张卡引用某凭据」的汇总条（UC-094 A10 影响面用；不含明文、不含判定）。
type cardRef struct {
	toolID string
	cardID string
	name   string
}

// referencingCards 汇总全部工具里**登记引用**某凭据的自建卡（UC-094 A10）。
//
// 「登记引用」= 卡三要素里的 credRef 与之相等；不做任何解析（保存与启用分离：
// 坏引用可保存，影响面按登记口径列出）。排序确定性：toolID → cardID。
func (s *profileStore) referencingCards(ref string) []cardRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []cardRef{}
	for toolID, t := range s.doc.Tools {
		for _, c := range t.User {
			if c.CredRef != ref {
				continue
			}
			out = append(out, cardRef{toolID: toolID, cardID: c.ID, name: c.Name})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].toolID != out[j].toolID {
			return out[i].toolID < out[j].toolID
		}
		return out[i].cardID < out[j].cardID
	})
	return out
}

// update 改一张自建卡（UC-032 三要素可改；指针 nil = 该字段不改）。
//
// UC-094 A5（乐观并发）：`p.BaseRev` 非空时先做**基于版本**的判定——与当前内容指纹
// 不符即整体拒绝（version_conflict + 两版可读面），一个字节都不写；相符才继续。
// 判定与写入在同一把锁内完成（先判后写若不原子，「两个写者都通过判定」就是假防线）。
// 每次内容真的变化时保留**上一版快照**（`Prev`），冲突响应据此给出「两版可读面」。
//
// 快照纪律（B-5 修复 F1）：prev 必须是**提交前盘上版本**的逐字节快照。两条缺一不可：
//  1. 在任何构造/改写**之前**取快照（此刻 cardVersionOf(old) 读到的才是提交前内容）；
//  2. 构造 next 绝不原地改 old 的 map/slice（settings 走「拷出再合并」的新 map）。
//
// `next := old` 只是结构体浅拷贝——原地改 `next.Settings[k]` 会连 old 一起改，
// 提交前的版本（及其快照）随之被改后内容污染：prev.rev 变成新 rev、settings 成
// 新旧混合体，冲突响应的 base/diff 因此缺失（当上一版恰是提交所基于的版本时，
// §4.2 additive 明文要求两者在场）。
func (s *profileStore) update(id, pid string, p userCardPatch) (userProfileDoc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.toolLocked(id)
	for i := range t.User {
		if t.User[i].ID != pid {
			continue
		}
		old := t.User[i]
		oldRev := cardRevOf(old)
		if p.BaseRev != nil && *p.BaseRev != "" && *p.BaseRev != oldRev {
			return userProfileDoc{}, versionConflictError(old, oldRev, *p.BaseRev)
		}
		// 先取快照再做任何构造：这是提交前盘上版本（cardVersionOf 逐字节拷出，
		// 与 old / next 深隔离），此后任何写入都碰不到它。
		prevSnapshot := cardVersionOf(old)
		next := old
		if p.Name != nil {
			next.Name = *p.Name
		}
		if p.ModelID != nil {
			next.ModelID = *p.ModelID
		}
		if p.Endpoint != nil && *p.Endpoint != "" {
			next.Endpoint = *p.Endpoint
		}
		if p.EndpointIsFullBase != nil {
			next.EndpointIsFullBase = *p.EndpointIsFullBase
		}
		if p.CredRef != nil && *p.CredRef != "" {
			next.CredRef = *p.CredRef
		}
		if p.Models != nil {
			next.Models = *p.Models
		}
		// UC-040/UC-042：非 nil 一律落盘——>0 是覆盖，**0 是清除声明**（与盘上
		// omitempty 的「不声明」同形）。越界值在 handler 侧已拒，这里不必再分档。
		if p.MaxOutputTokens != nil {
			next.MaxOutputTokens = *p.MaxOutputTokens
		}
		if p.Capabilities != nil {
			next.Capabilities = *p.Capabilities
		}
		if p.CapabilityAcks != nil {
			next.CapabilityAcks = *p.CapabilityAcks
		}
		if p.Settings != nil {
			// 拷出 old.Settings 再合并（新建 map），绝不原地改 next.Settings：
			// next 与 old 共享同一 map，原地改会连 old（提交前版本）一起改写——
			// 快照取在改前也救不了共享 map：rev 是旧的真、内容是新的，
			// 「上一版」成了新旧混合体（B-5 修复 F1 的根因）。
			merged := copySettings(old.Settings)
			if merged == nil {
				merged = map[string]any{}
			}
			for k, v := range p.Settings {
				merged[k] = v
			}
			next.Settings = merged
		}
		if err := checkCardNameUnique(t, next.Name, pid); err != nil {
			return userProfileDoc{}, err
		}
		nextRev := cardRevOf(next)
		if nextRev != oldRev {
			next.Prev = &prevSnapshot
		} else {
			// 内容未变：上一版保持不动（历史是「内容变过」的记录，不是保存次数）。
			next.Prev = old.Prev
		}
		next.Rev = nextRev
		t.User[i] = next
		s.doc.Tools[id] = t
		if err := s.saveLocked(); err != nil {
			return userProfileDoc{}, err
		}
		return next, nil
	}
	return userProfileDoc{}, apierr.New(apierr.CodeNotFound, "档位卡不存在: %s", pid)
}

// ── UC-094 A5：修订指纹与两版可读面 ─────────────────────────────────────────

// cardRevOf 计算自建卡**内容**的修订指纹（`v1-` + sha256 前 16 hex）。
//
// 内容是「用户能改的全部字段」：name/modelId/settings/endpoint/endpointIsFullBase/credRef/models/
// maxOutputTokens/capabilities。刻意**不含** acks/copiedFrom（元数据不是内容）、
// 也不含 ID（指纹回答「内容是否相同」，两张内容相同的卡同指纹是正确的）。
// encoding/json 对 map 键排序 ⇒ 序列化是确定性的，同内容恒同指纹。
func cardRevOf(card userProfileDoc) string {
	blob, err := json.Marshal(userCardVersion{
		Name:               card.Name,
		ModelID:            card.ModelID,
		Settings:           card.Settings,
		Endpoint:           card.Endpoint,
		EndpointIsFullBase: card.EndpointIsFullBase,
		CredRef:            card.CredRef,
		Models:             card.Models,
		MaxOutputTokens:    card.MaxOutputTokens,
		Capabilities:       card.Capabilities,
	})
	if err != nil {
		// 内容不可序列化（settings 里塞了无法编码的值）是数据缺陷：给一个稳定的哨兵，
		// 让基于版本的判定退化到「任何保存都视为内容变化」，而不是 panic 或空指纹。
		return "v1-unmarshalable"
	}
	return "v1-" + tx.HashBytes(blob)[:16]
}

// cardVersionOf 把卡内容翻成可读快照（UC-094 A5「两版可读面」的载体）。
//
// **逐字节快照**：引用字段（settings/models/capabilities）全部拷出，与来源卡深隔离。
// 快照会被存进 `Prev` 并在冲突响应里作为「你基于的那一版」回放——若与来源共享
// map/slice，后续任何原地改写都会隔着别名污染历史（B-5 修复 F1 的教训：隔离做在
// 快照构造点，任何调用方都不再需要记得拷贝）。
func cardVersionOf(card userProfileDoc) userCardVersion {
	var models []string
	if card.Models != nil {
		models = append([]string{}, card.Models...)
	}
	var caps *userCardCapabilities
	if card.Capabilities != nil {
		c := *card.Capabilities
		if card.Capabilities.Modalities != nil {
			c.Modalities = append([]string{}, card.Capabilities.Modalities...)
		}
		caps = &c
	}
	return userCardVersion{
		Rev:                cardRevOf(card),
		ID:                 card.ID,
		Name:               card.Name,
		ModelID:            card.ModelID,
		Settings:           copySettings(card.Settings),
		Endpoint:           card.Endpoint,
		EndpointIsFullBase: card.EndpointIsFullBase,
		CredRef:            card.CredRef,
		Models:             models,
		MaxOutputTokens:    card.MaxOutputTokens,
		Capabilities:       caps,
	}
}

// copySettings 复制 settings 顶层 map（新 map + 逐键拷入；nil 保持 nil，
// 空 map 保持空——不共享、也不改变「缺席/空」的既有可辨形态）。
func copySettings(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// cardFieldDiff 是两版之间一个字段的差异（`before`/`after` 是 JSON 可读值）。
type cardFieldDiff struct {
	Field  string `json:"field"`
	Before any    `json:"before,omitempty"`
	After  any    `json:"after,omitempty"`
}

// cardVersionDiff 逐字段比较两版内容（固定字段序，可读且确定性）。
//
// 比较口径：标量按值、settings/capabilities 按规范 JSON（键序无关）。
func cardVersionDiff(before, after userCardVersion) []cardFieldDiff {
	out := make([]cardFieldDiff, 0, 8)
	add := func(field string, b, a any, equal bool) {
		if equal {
			return
		}
		out = append(out, cardFieldDiff{Field: field, Before: b, After: a})
	}
	strDiff := func(field, b, a string) {
		add(field, orNil(b), orNil(a), b == a)
	}
	strDiff("name", before.Name, after.Name)
	strDiff("modelId", before.ModelID, after.ModelID)
	add("settings", jsonOrNil(before.Settings), jsonOrNil(after.Settings),
		canonicalEqual(before.Settings, after.Settings))
	strDiff("endpoint", before.Endpoint, after.Endpoint)
	add("endpointIsFullBase", before.EndpointIsFullBase, after.EndpointIsFullBase,
		before.EndpointIsFullBase == after.EndpointIsFullBase)
	strDiff("credRef", before.CredRef, after.CredRef)
	add("models", jsonOrNil(before.Models), jsonOrNil(after.Models),
		canonicalEqual(before.Models, after.Models))
	add("maxOutputTokens", orNilInt(before.MaxOutputTokens), orNilInt(after.MaxOutputTokens),
		before.MaxOutputTokens == after.MaxOutputTokens)
	add("capabilities", jsonOrNil(before.Capabilities), jsonOrNil(after.Capabilities),
		canonicalEqual(before.Capabilities, after.Capabilities))
	return out
}

// canonicalEqual 比较两个值经规范 JSON 序列化后是否相同。
func canonicalEqual(a, b any) bool {
	ab, aerr := json.Marshal(a)
	bb, berr := json.Marshal(b)
	if aerr != nil || berr != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

func orNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func orNilInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

// jsonOrNil 把「缺席即无」的值翻成可进 omitempty 的形态：nil 接口才会被省略，
// 而 typed-nil（nil 的切片/映射/指针装进接口）既不会被省略、编码出来还是 `null`。
func jsonOrNil(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case []string:
		if t == nil {
			return nil
		}
	case map[string]any:
		if t == nil {
			return nil
		}
	case *userCardCapabilities:
		if t == nil {
			return nil
		}
	}
	return v
}

// versionConflictError 组装冲突错误：机器可读的明确冲突码 + 两版可读面。
//
// `base`/`diff` 只在**服务端保留的那一版恰是提交所基于的版本**时给出（保留恰一版）；
// 否则如实缺席（不伪造一份「看起来像你那版」的差异——那比不给更糟）。
func versionConflictError(cur userProfileDoc, curRev, baseRev string) error {
	err := apierr.New(apierr.CodeVersionConflict,
		"保存冲突：你基于的版本（%s）已不是当前版本（%s）；看两版差异后基于当前版本重新提交",
		baseRev, curRev).WithDetails(map[string]any{
		"rev":     curRev,
		"baseRev": baseRev,
		"current": cardVersionOf(cur),
	})
	if cur.Prev != nil && cur.Prev.Rev == baseRev {
		err.WithDetails(map[string]any{
			"base": *cur.Prev,
			"diff": cardVersionDiff(*cur.Prev, cardVersionOf(cur)),
		})
	}
	return err
}

// checkCardNameUnique 校验同工具内名称唯一（UC-094 A2「同工具重名」服务端权威）。
//
// 判据 = trim 后的精确串；`excludeID` 排除正在改的那张卡自己。派生卡
// （受管/官方/router/下发）的名称不在比较范围内——它们不由用户设置。
func checkCardNameUnique(t toolProfileDoc, name, excludeID string) error {
	for _, c := range t.User {
		if c.ID == excludeID {
			continue
		}
		if c.Name == name {
			return apierr.New(apierr.CodeInvalidRequest,
				"同工具已存在同名配置 %q（名称在单个工具内必须唯一）", name).
				WithDetails(map[string]any{"field": "name", "reason": "duplicate"})
		}
	}
	return nil
}

// remove 删一张自建卡。
func (s *profileStore) remove(id, pid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.toolLocked(id)
	kept := t.User[:0:0]
	found := false
	for _, c := range t.User {
		if c.ID == pid {
			found = true
			continue
		}
		kept = append(kept, c)
	}
	if !found {
		return apierr.New(apierr.CodeNotFound, "档位卡不存在: %s", pid)
	}
	t.User = kept
	order := make([]string, 0, len(t.Order))
	for _, pid2 := range t.Order {
		if pid2 != pid {
			order = append(order, pid2)
		}
	}
	t.Order = order
	s.doc.Tools[id] = t
	return s.saveLocked()
}

// setOrder 存自建卡顺序。`order` 必须是现存自建卡的一个排列（全排列校验）。
func (s *profileStore) setOrder(id string, order []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.toolLocked(id)
	existing := map[string]bool{}
	for _, c := range t.User {
		existing[c.ID] = true
	}
	if len(order) != len(existing) {
		return apierr.New(apierr.CodeInvalidRequest,
			"顺序必须是现存自建卡的排列：现有 %d 张，给出 %d 个", len(existing), len(order))
	}
	seen := map[string]bool{}
	for _, pid := range order {
		if !existing[pid] {
			return apierr.New(apierr.CodeInvalidRequest, "顺序里出现未知档位卡: %s", pid)
		}
		if seen[pid] {
			return apierr.New(apierr.CodeInvalidRequest, "顺序里有重复项: %s", pid)
		}
		seen[pid] = true
	}
	t.Order = append([]string(nil), order...)
	s.doc.Tools[id] = t
	return s.saveLocked()
}

// orderedUsers 按记账顺序返回自建卡（未登记的卡排在后面，保证不丢卡）。
func orderedUsers(t toolProfileDoc) []userProfileDoc {
	byID := map[string]userProfileDoc{}
	for _, c := range t.User {
		byID[c.ID] = c
	}
	out := make([]userProfileDoc, 0, len(t.User))
	for _, pid := range t.Order {
		if c, ok := byID[pid]; ok {
			out = append(out, c)
			delete(byID, pid)
		}
	}
	rest := make([]string, 0, len(byID))
	for pid := range byID {
		rest = append(rest, pid)
	}
	sort.Strings(rest)
	for _, pid := range rest {
		out = append(out, byID[pid])
	}
	return out
}

// newProfileID 生成 uuid v4 形态的卡片 id（与契约示例的 `<user-uuid>` 一致）。
//
// 随机源不可用是进程级故障：**报错**而不是 panic —— 请求路径上不允许出现 panic
// （一个坏掉的 /dev/urandom 不该把正在跑的服务打崩）。
func newProfileID() (string, error) {
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		return "", apierr.Wrap(apierr.CodeInternal, err, "生成档位卡 id 失败")
	}
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	buf := make([]byte, 36)
	hex.Encode(buf[0:8], u[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], u[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], u[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], u[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], u[10:16])
	return string(buf), nil
}

// parseProfileID 校验卡片 id 形态（受管/官方/router/自建四种）。
func parseProfileID(uid, pid string) error {
	if pid == "" {
		return apierr.New(apierr.CodeInvalidRequest, "档位卡 id 必填")
	}
	if pid == OfficialProfileID || pid == RouterProfileID || pid == ManagedProfilePrefix+uid {
		return nil
	}
	if len(pid) == 36 && strings.Count(pid, "-") == 4 {
		return nil
	}
	return apierr.New(apierr.CodeInvalidRequest, "档位卡 id 形态非法: %s", pid)
}

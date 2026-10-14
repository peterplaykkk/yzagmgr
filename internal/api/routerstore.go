// router 多臂配置的持久化与托管凭据库（UC-023/028/029）。
//
// 两份盘上文件、两种密级：
//   - `managed/router.json`：臂配置 + 逐臂 remap 表，**只含凭据引用**
//     （yzagmgr-managed-token://<uid>，UC-018 A12），任何回读载荷都不含明文（UC-023 A2）；
//   - `private/managed-tokens.json`（0600）：托管凭据库，uid → 令牌记录（明文 +
//     可读名称/供应商/更新时间，UC-094 A9）。
//     这是全产品唯一允许落令牌明文的位置（UC-023 契约：「守护进程把令牌收进托管
//     凭据库，配置只落引用」），日志与回读恒不触碰它。
//
// 内存态在 internal/proxy（请求路径读它）；本文件负责「落盘 + 启动时装回」。
package api

import (
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
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/proxy"
)

const routerConfigSchema = 1

// managedTokensSchema 是托管凭据库的盘上 schema。
//
// schema 2（2026-09-27，UC-094 A9 additive）：记录从「uid → 明文」升级为「uid → 条目」
// （增加可读名称/供应商/更新时间三个展示元数据）。schema 1 的老文件在**装载时**就地迁移
// （uid 不变 ⇒ 既有引用全部照常解析），下一次落盘写成 schema 2。
const managedTokensSchema = 2

// routerConfigDoc 是 `managed/router.json` 的盘上形状。
//
// `modelCaps` 是 UC-030 additive 的能力位段（arm → alias → 能力位）：老文件缺段 =
// 全部不声明，schema 不变（additive 向后兼容）。
//
// （`deliveredArms` 归属段已随登录下发删除——design §2.2 routerstore 行；
// 旧文件中的该段装载时被忽略。）
type routerConfigDoc struct {
	Schema      int                              `json:"schema"`
	Routes      []proxy.ArmEntry                 `json:"routes"`
	ModelRemap  map[string][]proxy.ModelMapEntry `json:"modelRemap"`
	EffortRemap map[string]proxy.EffortTable     `json:"effortRemap"`
	ModelCaps   map[string]map[string]modelCaps  `json:"modelCaps,omitempty"`
}

// managedTokenEntry 是托管凭据库的一条记录（schema 2）。
//
// **名称与引用身份分离**（UC-094 A9）：引用身份是 map 键（uid，不可变），名称/供应商
// 只是可读标签——轮换（A10）换 Token、引用与名称都不动。
type managedTokenEntry struct {
	// Name 是可读名称（创建时给）。空 = 未命名（schema 1 迁移来的旧记录），不伪造。
	Name string `json:"name,omitempty"`
	// Provider 是供应商标签（可读展示用，不参与任何判定）。
	Provider string `json:"provider,omitempty"`
	// UpdatedAt 是最后写入时刻（RFC3339，UTC）。旧记录为空。
	UpdatedAt string `json:"updatedAt,omitempty"`
	// Mask 是展示掩码（单源实现 credentialMask）。UC-094 A10 additive：落盘保存，
	// 撤销清空 Token 之后展示面仍能显示「撤掉的是哪一枚」（掩码不是秘密材料，见
	// credentialMask 的口径）。schema 2 之前/迁移来的旧记录缺席 ⇒ 回读时现算。
	Mask string `json:"mask,omitempty"`
	// Revoked / RevokedAt 是撤销状态（UC-094 A10 additive）：被撤销的凭据 Token
	// **清零**（明文不再保留），引用身份与名称保留（撤销展示引用它的配置、不静默删配置；
	// 用它的新请求明确拒绝）。
	Revoked   bool   `json:"revoked,omitempty"`
	RevokedAt string `json:"revokedAt,omitempty"`
	// Token 是令牌明文（全产品唯一允许落盘的位置；绝不进任何响应/日志）。
	// 撤销后为空串。
	Token string `json:"token"`
}

// managedTokensDoc 是 `private/managed-tokens.json` 的盘上形状（0600）。
type managedTokensDoc struct {
	Schema  int                          `json:"schema"`
	Entries map[string]managedTokenEntry `json:"entries"`
	// Tokens 是 schema 1 的只读兼容段：装载老文件时读出后立刻迁移进 Entries；
	// 落盘**不再写它**（写路径只写 Entries）。
	Tokens map[string]string `json:"tokens,omitempty"`
}

// managedCredential 是托管凭据库对外的一条**只读视图**：引用身份（UID）与展示元数据。
// 刻意不含 Token：明文只允许存在于 vault 内部，任何对外面（创建/列出/读取/轮换/撤销）
// 都拿不到。
type managedCredential struct {
	UID       string
	Name      string
	Provider  string
	UpdatedAt string
	// Mask 是展示掩码（单源实现 credentialMask）。
	Mask string
	// Revoked / RevokedAt 是撤销状态（UC-094 A10：已保存/未提供/已失效三态可辨）。
	Revoked   bool
	RevokedAt string
}

// tokenVault 是托管凭据库：实现 proxy.ArmTokenSource，落 0600 的私有文件。
type tokenVault struct {
	mu      sync.Mutex
	path    string
	entries map[string]managedTokenEntry
}

// newTokenVault 打开（或初始化）托管凭据库。文件损坏按空库处理并**响亮记日志**：
// 凭据缺失会让用臂时显式报缺凭据（UC-023 边界），比拒绝启动更可诊断。
//
// schema 1（`tokens`: uid → 明文）在装载时就地迁移：引用身份不变，展示元数据缺席
// （不伪造名称/时间；下一次任何写入把整库落成 schema 2）。
func newTokenVault(env *config.Env) *tokenVault {
	v := &tokenVault{path: env.ManagedTokensFile(), entries: map[string]managedTokenEntry{}}
	b, err := os.ReadFile(v.path)
	if err != nil {
		return v
	}
	var doc managedTokensDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return v
	}
	switch doc.Schema {
	case managedTokensSchema:
		if doc.Entries != nil {
			v.entries = doc.Entries
		}
	case 1:
		for uid, tok := range doc.Tokens {
			v.entries[uid] = managedTokenEntry{Token: tok}
		}
	}
	return v
}

// Put 收进一枚令牌（无名称形态），返回凭据引用。既有调用方沿用（等价于 PutNamed 的
// 未命名形态）。
func (v *tokenVault) Put(token string) (string, error) {
	mc, err := v.PutNamed(token, "", "")
	if err != nil {
		return "", err
	}
	return proxy.ArmTokenScheme + mc.UID, nil
}

// PutNamed 收进一枚令牌并登记可读名称/供应商，返回新记录的视图（不含明文）。
// 每次提交独立 uid ⇒ 引用互异（UC-023 A1）；名称只是标签，引用身份恒为新 uid
// （与名称无关、可重名）。
func (v *tokenVault) PutNamed(token, name, provider string) (managedCredential, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return managedCredential{}, apierr.Wrap(apierr.CodeInternal, err, "生成托管令牌 uid 失败")
	}
	uid := "mt-" + hex.EncodeToString(b[:])
	entry := managedTokenEntry{
		Name:      name,
		Provider:  provider,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Mask:      credentialMask(token),
		Token:     token,
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.saveLocked(map[string]managedTokenEntry{uid: entry}); err != nil {
		return managedCredential{}, err
	}
	v.entries[uid] = entry
	return v.viewLocked(uid, entry), nil
}

// Rotate 用新值轮换一条凭据（UC-094 A10）：**引用身份不变**（uid/名称/供应商都不动），
// 只换 Token、重算掩码并刷新 UpdatedAt。
//
// 三条硬口径：
//   - **只接受新值**：新值由调用方在 REST 层校验（非空、无控制字符、非掩码形态）；
//   - **失败保留旧凭据**：先落盘成功、再提交内存（落盘失败 ⇒ 内存与盘上都还是旧值，
//     调用方得到明确错误而不是一句「像成功了」）；
//   - 已撤销的凭据不能轮换（撤销是终态；恢复路径 = 新建凭据并重新关联配置）。
func (v *tokenVault) Rotate(uid, token string) (managedCredential, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	old, ok := v.entries[uid]
	if !ok {
		return managedCredential{}, apierr.New(apierr.CodeNotFound, "凭据不存在: %s", uid)
	}
	if old.Revoked {
		return managedCredential{}, apierr.New(apierr.CodeInvalidRequest,
			"凭据 %s 已撤销，不能轮换（撤销是终态；新建凭据并重新关联配置，或在配置中改换引用）", uid)
	}
	next := old
	next.Token = token
	next.Mask = credentialMask(token)
	next.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	merged := v.mergedLocked(map[string]managedTokenEntry{uid: next})
	if err := writePrivateJSON(v.path, managedTokensDoc{Schema: managedTokensSchema, Entries: merged}); err != nil {
		return managedCredential{}, err
	}
	v.entries[uid] = next
	return v.viewLocked(uid, next), nil
}

// Revoke 撤销一条凭据（UC-094 A10）：Token **清零**（明文不再保留），引用身份/名称/
// 掩码与撤销时刻保留。**不删任何配置**（引用它的配置原样保留；这些配置的新请求在
// 数据面与启用链上明确拒绝，影响面由引用清单端点在撤销前/后如实列出）。
//
// 幂等：重复撤销返回同一条视图（撤销时刻保持首次值），不产生第二份语义。
// 落盘失败 ⇒ 内存不变（撤销未发生），调用方拿到 storage 错误。
func (v *tokenVault) Revoke(uid string) (managedCredential, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	old, ok := v.entries[uid]
	if !ok {
		return managedCredential{}, apierr.New(apierr.CodeNotFound, "凭据不存在: %s", uid)
	}
	if old.Revoked {
		return v.viewLocked(uid, old), nil
	}
	next := old
	if next.Mask == "" {
		next.Mask = credentialMask(old.Token)
	}
	next.Token = ""
	next.Revoked = true
	next.RevokedAt = time.Now().UTC().Format(time.RFC3339)
	merged := v.mergedLocked(map[string]managedTokenEntry{uid: next})
	if err := writePrivateJSON(v.path, managedTokensDoc{Schema: managedTokensSchema, Entries: merged}); err != nil {
		return managedCredential{}, err
	}
	v.entries[uid] = next
	return v.viewLocked(uid, next), nil
}

// Discard 硬删一枚**从未被任何配置引用**的令牌（登记中途失败的孤儿回收口，UC-023 A4
// 「不部分写入」）。
//
// 与 [Revoke] 的边界刻意不同：Revoke 是用户对**在册配置引用着**的凭据的动作——记录
// 保留（引用身份/名称/掩码/撤销时刻），供影响面展示与恢复指引；Discard 针对的是**刚
// 收下、登记尚未成功**的令牌（如 POST /routes 收下令牌后 AddArm 被拒）：它从未进过
// 任何配置（router.json 只在登记成功后落盘），留一条「撤销记录」只会在凭据列表里留下
// 用户没见过的残骸 ⇒ 连条目一并移除（0600 原子落盘，与其它写路径同一实现）。
//
// 幂等：不在册（已回收）即空操作。只接受本产品引用形态——形态非法 = 调用方 bug，
// 显式报错不静默（本方法只有登记路径的内部调用方，不构成对外删除面）。
func (v *tokenVault) Discard(ref string) error {
	uid, ok := strings.CutPrefix(ref, proxy.ArmTokenScheme)
	if !ok || uid == "" || strings.ContainsAny(uid, "/?# \t\r\n") {
		return apierr.New(apierr.CodeInternal, "内部调用错误：待回收的凭据引用形态非法: %q", ref)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, exists := v.entries[uid]; !exists {
		return nil
	}
	next := make(map[string]managedTokenEntry, len(v.entries)-1)
	for k, e := range v.entries {
		if k != uid {
			next[k] = e
		}
	}
	if err := writePrivateJSON(v.path, managedTokensDoc{Schema: managedTokensSchema, Entries: next}); err != nil {
		return err
	}
	v.entries = next
	return nil
}

// InferenceTokenFor 实现 proxy.ArmTokenSource。
//
// 缺失返回空串（由调用方显式报缺凭据）；**已撤销**返回 [`proxy.ErrCredentialRevoked`]
// ——两条语义必须可辨：「没提供/找不到」与「已被用户撤销」的拒绝文案与处置方向不同
// （前者去补凭据，后者去轮换或重新关联）。
func (v *tokenVault) InferenceTokenFor(ref string) (string, error) {
	uid, ok := strings.CutPrefix(ref, proxy.ArmTokenScheme)
	if !ok {
		return "", nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	entry := v.entries[uid]
	if entry.Revoked {
		return "", proxy.ErrCredentialRevoked
	}
	return entry.Token, nil
}

// Views 列出全部托管凭据（只含引用身份与展示元数据，**不含明文**）。
//
// 排序：UpdatedAt 新 → 旧（未命名旧记录殿后），同刻按 uid 字典序——确定性排序，
// 界面与测试都不依赖 map 遍历顺序。
func (v *tokenVault) Views() []managedCredential {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]managedCredential, 0, len(v.entries))
	for uid, e := range v.entries {
		out = append(out, v.viewLocked(uid, e))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt != out[j].UpdatedAt {
			if out[i].UpdatedAt == "" {
				return false
			}
			if out[j].UpdatedAt == "" {
				return true
			}
			return out[i].UpdatedAt > out[j].UpdatedAt
		}
		return out[i].UID < out[j].UID
	})
	return out
}

// View 读单条凭据视图；uid 不存在时 ok=false。
func (v *tokenVault) View(uid string) (managedCredential, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	e, ok := v.entries[uid]
	if !ok {
		return managedCredential{}, false
	}
	return v.viewLocked(uid, e), true
}

func (v *tokenVault) viewLocked(uid string, e managedTokenEntry) managedCredential {
	mask := e.Mask
	if mask == "" {
		// 迁移来的旧记录没有落盘掩码：现算（单源实现，不另写一份口径）。
		mask = credentialMask(e.Token)
	}
	return managedCredential{
		UID:       uid,
		Name:      e.Name,
		Provider:  e.Provider,
		UpdatedAt: e.UpdatedAt,
		Mask:      mask,
		Revoked:   e.Revoked,
		RevokedAt: e.RevokedAt,
	}
}

// mergedLocked 把一批变更并进当前条目集（调用方持锁）。
func (v *tokenVault) mergedLocked(add map[string]managedTokenEntry) map[string]managedTokenEntry {
	merged := make(map[string]managedTokenEntry, len(v.entries)+len(add))
	for k, val := range v.entries {
		merged[k] = val
	}
	for k, val := range add {
		merged[k] = val
	}
	return merged
}

// saveLocked 原子落盘（合并既有条目后整库重写，0600）。
func (v *tokenVault) saveLocked(add map[string]managedTokenEntry) error {
	doc := managedTokensDoc{Schema: managedTokensSchema, Entries: v.mergedLocked(add)}
	return writePrivateJSON(v.path, doc)
}

// writePrivateJSON 以 0600 原子写一份 JSON（同目录临时文件 + rename）。
func writePrivateJSON(path string, doc any) error {
	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "编码托管凭据库失败")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "建立凭据库目录失败")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".managed-*.json")
	if err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "创建临时文件失败")
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(append(blob, '\n')); err != nil {
		_ = tmp.Close()
		return apierr.Wrap(apierr.CodeStorage, err, "写入托管文件失败")
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return apierr.Wrap(apierr.CodeStorage, err, "设置托管文件权限失败")
	}
	if err := tmp.Close(); err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "关闭临时文件失败")
	}
	if err := os.Rename(name, path); err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "替换托管文件失败")
	}
	return nil
}

// ── router 配置持久化（臂 + remap 表）────────────────────────────────────────

// persistRouterConfig 把代理引擎的当前 router 配置快照落盘（只含引用）。
func (s *Server) persistRouterConfig() error {
	doc := routerConfigDoc{
		Schema:      routerConfigSchema,
		Routes:      s.Proxy.Arms(),
		ModelRemap:  map[string][]proxy.ModelMapEntry{},
		EffortRemap: map[string]proxy.EffortTable{},
	}
	for _, a := range doc.Routes {
		if m := s.Proxy.ModelMapFor(a.Name); len(m) > 0 {
			doc.ModelRemap[a.Name] = m
		}
		// effortRemap[arm] 的**存在**只表示「该臂立过 effort 配置记录」（供管理面回读与
		// 重启后形态一致）；数据面是否钳制由表内容与 Force 决定——空 map 且无 Force
		// 即未配置（透传，UC-029 修订）。清空后仍留键，语义不漂移。
		if s.Proxy.EffortClampEngaged(a.Name) {
			t, _ := s.Proxy.EffortFor(a.Name)
			doc.EffortRemap[a.Name] = t
		}
	}
	// 能力位只落「在册臂 × 在册 alias 的声明」：被摘除的 alias/臂不带残骸（additive 段可整体缺省）。
	liveAliases := map[string]map[string]bool{}
	for _, a := range doc.Routes {
		set := map[string]bool{}
		for _, e := range doc.ModelRemap[a.Name] {
			set[e.Alias] = true
		}
		liveAliases[a.Name] = set
	}
	caps := s.modelCapsSnapshot()
	for arm, aliases := range caps {
		kept := map[string]modelCaps{}
		for alias, c := range aliases {
			if liveAliases[arm][alias] && c.declared() {
				kept[alias] = c
			}
		}
		if len(kept) > 0 {
			if doc.ModelCaps == nil {
				doc.ModelCaps = map[string]map[string]modelCaps{}
			}
			doc.ModelCaps[arm] = kept
		}
	}
	return writePrivateJSON(s.Env.RouterConfigFile(), doc)
}

// loadRouterConfig 启动时把盘上的 router 配置装回引擎（重启存活，UC-023 A3 / UC-028 A4）。
//
// 单条坏数据跳过并记警告，不让一份损坏的 remap 行拖垮整个守护进程：臂配置本身的
// 校验在写入端已做过（AddArm/Put* 会拒绝坏值），这里只防御「手改盘」的意外。
func (s *Server) loadRouterConfig() {
	b, err := os.ReadFile(s.Env.RouterConfigFile())
	if err != nil {
		return
	}
	var doc routerConfigDoc
	if err := json.Unmarshal(b, &doc); err != nil || doc.Schema != routerConfigSchema {
		s.log.Warn("router 配置不可解析，按空配置启动", "path", s.Env.RouterConfigFile(), "err", err)
		return
	}
	for _, a := range doc.Routes {
		if _, err := s.Proxy.AddArm(a); err != nil {
			s.log.Warn("请求映射配置跳过", "name", a.Name, "err", err)
		}
	}
	for arm, entries := range doc.ModelRemap {
		for _, e := range entries {
			if _, err := s.Proxy.PutModelMap(arm, e.Alias, e.Target); err != nil {
				s.log.Warn("router 模型映射跳过", "arm", arm, "alias", e.Alias, "err", err)
			}
		}
	}
	for arm, table := range doc.EffortRemap {
		// 键的存在 = 立过配置记录（重启回填登记位）；数据面是否钳制由 map/Force 内容决定。空表=透传。
		if err := s.Proxy.EngageEffortClamp(arm); err != nil {
			s.log.Warn("router effort 登记跳过", "arm", arm, "err", err)
			continue
		}
		for _, e := range table.Items {
			if _, err := s.Proxy.PutEffortEntry(arm, e.From, e.To); err != nil {
				s.log.Warn("router effort 映射跳过", "arm", arm, "from", e.From, "err", err)
			}
		}
		if _, err := s.Proxy.SetEffortForce(arm, table.ForceEffort); err != nil && table.ForceEffort != "" {
			s.log.Warn("router effort force 跳过", "arm", arm, "err", err)
		}
	}
	// 能力位（UC-030 additive）：随映射一并装回；指向已丢映射的声明无害（派生按在册 alias 取）。
	s.loadModelCaps(doc.ModelCaps)
}

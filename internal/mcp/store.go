package mcp

import (
	"encoding/json"
	"os"
	"sync"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/backup"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

// Options 是 Store 的可选依赖。
type Options struct {
	// Leases 是共享租约表（main 装配的同一 tx.Cache；nil 则自建，仅测试便利）。
	Leases *tx.Cache
}

// Store 承载注册表与物化写链（UC-111）。
//
// 两条纪律（对齐 workbuddy.Store）：
//  1. registryMu 序列化注册表的读-改-写（PUT/DELETE 与 preview 读快照互斥）；
//  2. 写盘只经 tx.Apply——指纹复核、写前备份、原子写、失败回滚全部复用，
//     本包不发明第三套装配路径。
type Store struct {
	registryPath string
	targetPath   func(tool string) (string, error)
	backups      *backup.Registry
	leases       *tx.Cache

	registryMu sync.Mutex
}

// NewStore 构造 Store；backups 为 nil 直接 panic（没有备份就等于没有写）。
func NewStore(env *config.Env, backups *backup.Registry, opts Options) *Store {
	if env == nil {
		panic("mcp: NewStore 需要 env")
	}
	if backups == nil {
		panic("mcp: NewStore 需要 backups（写链必须有写前备份）")
	}
	leases := opts.Leases
	if leases == nil {
		leases = tx.NewCache(0, nil)
	}
	return &Store{
		registryPath: env.MCPServersFile(),
		targetPath:   targetPathFor(env),
		backups:      backups,
		leases:       leases,
	}
}

// targetPathFor 解析逐工具的物化目标绝对路径（未知工具报 invalid_request）。
func targetPathFor(env *config.Env) func(string) (string, error) {
	return func(tool string) (string, error) {
		switch tool {
		case "claude":
			return env.ClaudeGlobalJSON(), nil
		case "claude-desktop":
			return env.ClaudeDesktopLiveConfig(), nil
		default:
			return "", apierr.New(apierr.CodeInvalidRequest,
				"未支持的工具: %q（允许 %s）", tool, joinTools())
		}
	}
}

func joinTools() string {
	out := ""
	for i, t := range SupportedTools {
		if i > 0 {
			out += "|"
		}
		out += t
	}
	return out
}

// ── 注册表 CRUD（读写面，无租约） ────────────────────────────────────────────

// ListServers 返回字典序注册快照。
func (s *Store) ListServers() ([]NamedServer, error) {
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	servers, err := s.loadRegistry()
	if err != nil {
		return nil, err
	}
	out := make([]NamedServer, 0, len(servers))
	for _, name := range sortedNames(servers) {
		out = append(out, NamedServer{Name: name, Server: servers[name]})
	}
	return out, nil
}

// PutServer 校验并覆盖一条注册（幂等 upsert，UC-111 A1）。
func (s *Store) PutServer(name string, sv Server) (NamedServer, error) {
	if err := ValidateName(name); err != nil {
		return NamedServer{}, err
	}
	if err := sv.Validate(); err != nil {
		return NamedServer{}, err
	}
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	servers, err := s.loadRegistry()
	if err != nil {
		return NamedServer{}, err
	}
	servers[name] = sv
	if err := s.saveRegistry(servers); err != nil {
		return NamedServer{}, err
	}
	return NamedServer{Name: name, Server: sv}, nil
}

// DeleteServer 移除一条注册；不存在报 not_found。只动注册表（live 清理走
// op=remove 的 preview/apply，UC-111 主流程第 5 步）。
func (s *Store) DeleteServer(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	servers, err := s.loadRegistry()
	if err != nil {
		return err
	}
	if _, ok := servers[name]; !ok {
		return apierr.New(apierr.CodeNotFound, "MCP server 不存在: %s", name)
	}
	delete(servers, name)
	return s.saveRegistry(servers)
}

// ── 逐工具状态（读面，UC-111 A9） ──────────────────────────────────────────

// Presence 是一条注册名在某工具 live 文件里的在位状态。
type Presence struct {
	Name    string `json:"name"`
	Present bool   `json:"present"`
}

// ToolStatus 是一件工具的物化目标与逐条目状态。
type ToolStatus struct {
	Tool    string     `json:"tool"`
	Path    string     `json:"path"`
	Exists  bool       `json:"exists"`
	Servers []Presence `json:"servers"`
}

// Status 读 live 文件真实状态（非记账推断）：文件缺席 = 全部 missing；
// 文件在场但不是合法 JSON = config_invalid（显式，不静默）。
func (s *Store) Status() ([]ToolStatus, error) {
	s.registryMu.Lock()
	registry, err := s.loadRegistry()
	s.registryMu.Unlock()
	if err != nil {
		return nil, err
	}
	names := sortedNames(registry)
	out := make([]ToolStatus, 0, len(SupportedTools))
	for _, tool := range SupportedTools {
		path, err := s.targetPath(tool)
		if err != nil {
			return nil, err
		}
		st := ToolStatus{Tool: tool, Path: path, Servers: make([]Presence, 0, len(names))}
		raw, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, apierr.Wrap(apierr.CodeStorage, err, "读物化目标失败: %s", path)
		}
		present := map[string]bool{}
		if err == nil && len(raw) > 0 {
			doc, perr := parseObject(raw)
			if perr != nil {
				return nil, configInvalid(path, perr)
			}
			st.Exists = true
			if ms := doc.Root().Get("mcpServers"); ms != nil {
				for _, m := range ms.Members {
					present[m.Key] = true
				}
			}
		} else if err == nil {
			st.Exists = true // 空文件也算在场（内容不可解析，按缺席内容处理）
		}
		for _, name := range names {
			st.Servers = append(st.Servers, Presence{Name: name, Present: present[name]})
		}
		out = append(out, st)
	}
	return out, nil
}

// ── preview / apply（租约写链） ─────────────────────────────────────────────

// resolve 把 Op 归一成租约要携带的意图快照（含注册表解析与 tools 过滤）。
func (s *Store) resolve(op Op) (intent, string, error) {
	path, err := s.targetPath(op.Tool)
	if err != nil {
		return intent{}, "", err
	}
	switch op.Mode {
	case "apply", "remove":
	default:
		return intent{}, "", apierr.New(apierr.CodeInvalidRequest,
			"op 取值非法: %q（允许 apply|remove）", op.Mode)
	}
	s.registryMu.Lock()
	registry, err := s.loadRegistry()
	s.registryMu.Unlock()
	if err != nil {
		return intent{}, "", err
	}

	defaulted := len(op.Names) == 0
	names := resolveNames(op.Names)
	if len(names) == 0 {
		names = sortedNames(registry)
	}
	it := intent{Tool: op.Tool, Op: op.Mode, Names: names}
	if op.Mode == "remove" && defaulted {
		// 缺省 remove = 撤销本工具的 apply：按条目 tools 范围过滤（与 apply 对称），
		// 不碰「注册表限定别的工具、但用户在本工具手写过同名」的条目（A8 扩展）。
		// 显式给出 names 是强制路径，不做过滤——那是清理手写/残留条目的出口。
		kept := make([]string, 0, len(names))
		for _, name := range names {
			if sv, ok := registry[name]; ok && sv.covers(op.Tool) {
				kept = append(kept, name)
			}
		}
		it.Names = kept
	}
	if op.Mode == "apply" {
		it.Entries = map[string]Server{}
		kept := make([]string, 0, len(names))
		for _, name := range names {
			sv, ok := registry[name]
			if !ok {
				return intent{}, "", apierr.New(apierr.CodeInvalidRequest,
					"未注册的 MCP server: %s（apply 的 names 必须在注册表内）", name)
			}
			if !sv.covers(op.Tool) {
				continue // 条目 tools 范围不含本工具：整体不物化（UC-111 A8）
			}
			kept = append(kept, name)
			it.Entries[name] = sv
		}
		it.Names = kept
	}
	return it, path, nil
}

// Preview 产出零副作用的文件计划（UC-111 A3），并把意图快照与「我方条目」
// 现状指纹钉进租约。
func (s *Store) Preview(op Op) (*tx.Lease, error) {
	it, path, err := s.resolve(op)
	if err != nil {
		return nil, err
	}
	raw, err := tx.ReadForFingerprint(path)
	if err != nil {
		return nil, err
	}
	after, err := render(path, raw, it)
	if err != nil {
		return nil, err
	}
	intentJSON, err := json.Marshal(it)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "序列化 MCP 意图失败")
	}
	return s.leases.Put(&tx.Lease{
		ToolID:    op.Tool,
		Operation: op.Mode, // 回声请求 op（previewPayload.operation；family 的
		// restore/reset 语义不在此枚举内，备份 reason 由 ApplyPreview 显式给定）
		Files: []family.FilePlan{{
			Path:    path,
			Existed: fileExisted(path),
			Before:  raw,
			After:   after,
		}},
		Meta: map[string]string{
			"mcp.intent":   string(intentJSON),
			"mcp.minehash": subtreeHash(raw, it.Names),
		},
	}), nil
}

// ApplyPreview 执行一份已确认的租约：
//
//  1. Take 一次性消费（未知/过期由 tx 报 invalid_request/preview_expired）；
//  2. 读**最新**盘上字节，复核我方条目指纹——我方条目被外改 ⇒ config_changed
//     零写入；仅他键漂移 ⇒ 在最新字节上重算意图并吸收（UC-111 A5）；
//  3. 重算 After 与指纹后交 tx.Apply（备份/原子写/回滚/毫秒窗指纹复核）。
func (s *Store) ApplyPreview(id string) (tx.Result, string, error) {
	lease, err := s.leases.Take(id)
	if err != nil {
		return tx.Result{}, "", err
	}
	// 跨端点拿错租约（家族 §4.3 的预览被 /mcp/apply 消费）：Take 已按一次性语义
	// 消费，这里给可理解的 invalid_request 而不是 internal（UC-111 A10 错误码族）。
	var it intent
	if lease.Meta["mcp.intent"] == "" {
		return tx.Result{}, "", apierr.New(apierr.CodeInvalidRequest,
			"该预览不是 MCP 租约（或已失效），请重新预览")
	}
	if err := json.Unmarshal([]byte(lease.Meta["mcp.intent"]), &it); err != nil {
		return tx.Result{}, "", apierr.New(apierr.CodeInvalidRequest,
			"MCP 租约意图损坏，请重新预览")
	}
	if len(lease.Files) != 1 {
		return tx.Result{}, "", apierr.New(apierr.CodeInvalidRequest,
			"MCP 租约文件计划形状异常，请重新预览")
	}
	path := lease.Files[0].Path
	raw, err := tx.ReadForFingerprint(path)
	if err != nil {
		return tx.Result{}, lease.ToolID, err
	}
	if got := subtreeHash(raw, it.Names); got != lease.Meta["mcp.minehash"] {
		return tx.Result{}, lease.ToolID, apierr.New(apierr.CodeConfigChanged,
			"我方 MCP 条目在预览后被外部修改，已中止写入: %s", path).
			WithDetails(map[string]any{"path": path, "toolId": lease.ToolID})
	}
	after, err := render(path, raw, it)
	if err != nil {
		return tx.Result{}, lease.ToolID, err
	}
	lease.Files = []family.FilePlan{{
		Path:    path,
		Existed: fileExisted(path),
		Before:  raw,
		After:   after,
	}}
	lease.Fingerprints = map[string]string{path: tx.HashBytes(raw)}
	res, err := tx.Apply(lease, tx.Options{
		WriteMode: 0o600,
		Backuper:  s.backups,
		Reason:    backup.ReasonApply,
	})
	return res, lease.ToolID, err
}

func fileExisted(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

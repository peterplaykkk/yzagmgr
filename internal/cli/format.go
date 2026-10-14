package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
)

// ── 契约载荷（doc/api-v1.md §2/§4/§5/§3 的子集，只取 CLI 需要的字段）────────

type healthData struct {
	Status       string `json:"status"`
	State        string `json:"state"`
	UptimeSec    int64  `json:"uptimeSec"`
	DataRoot     string `json:"dataRoot"`
	APIPort      int    `json:"apiPort"`
	ProxyPort    int    `json:"proxyPort"`
	ProxyRunning bool   `json:"proxyRunning"`
	UsageDB      struct {
		State  string `json:"state"`
		Detail string `json:"detail"`
	} `json:"usageDb"`
}

type pathInfo struct {
	Path        string `json:"path"`
	Exists      bool   `json:"exists"`
	Managed     bool   `json:"managed"`
	CreatedByUs bool   `json:"createdByUs"`
	SizeBytes   int64  `json:"sizeBytes"`
	SHA256      string `json:"sha256"`
}

type toolBrief struct {
	ID string `json:"id"`
}

type toolDetail struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Installed 是旧布尔（口径 = 配置目录在场即计）；人读面不再直接渲染它，
	// 只在 InstalledState 缺席（旧载荷）时作为回退依据，见 installedStateOf。
	Installed bool `json:"installed"`
	// InstalledState 是 §4.1 的 additive 安装三态（installed / not_installed / unknown，
	// 2026-09-27「状态拆分」修订）：只认安装证据，优先于旧布尔；人读面按它呈现。
	InstalledState  string     `json:"installedState"`
	InstallCopies   int        `json:"installCopies"`
	Version         string     `json:"version"`
	Tier            string     `json:"tier"`
	DecideKey       string     `json:"decideKey"`
	Paths           []pathInfo `json:"paths"`
	ActiveProfileID string     `json:"activeProfileId"`
	CompanionKeys   []struct {
		Name   string `json:"name"`
		Value  string `json:"value"`
		Masked bool   `json:"masked"`
		Order  int    `json:"order"`
	} `json:"companionKeys"`
}

// installedState 的封闭取值（契约 §4.1，与 api / diagnose 包逐字一致）。
const (
	installedStateInstalled    = "installed"
	installedStateNotInstalled = "not_installed"
	installedStateUnknown      = "unknown"
)

type profileList struct {
	Items []struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		Active   bool   `json:"active"`
		Tier     string `json:"tier"`
		ModelID  string `json:"modelId"`
		Editable bool   `json:"editable"`
	} `json:"items"`
	Total           int    `json:"total"`
	ActiveProfileID string `json:"activeProfileId"`
}

type filePlan struct {
	Path         string `json:"path"`
	Existed      bool   `json:"existed"`
	Delete       bool   `json:"delete"`
	SHA256Before string `json:"sha256Before"`
	BytesBefore  int    `json:"bytesBefore"`
	Diff         []struct {
		Op   string `json:"op"`
		Text string `json:"text"`
	} `json:"diff"`
	Warning string `json:"warning"`
}

type previewData struct {
	PreviewID string     `json:"previewId"`
	ExpiresAt string     `json:"expiresAt"`
	Operation string     `json:"operation"`
	Files     []filePlan `json:"files"`
	Warning   string     `json:"warning"`
}

type applyData struct {
	Status string `json:"status"`
	Files  []struct {
		Path   string `json:"path"`
		Status string `json:"status"`
	} `json:"files"`
	BackupIDs  []string `json:"backupIds"`
	Activation struct {
		Tier            string `json:"tier"`
		ActiveProfileID string `json:"activeProfileId"`
	} `json:"activation"`
}

type backupItem struct {
	BackupID string `json:"backupId"`
	At       string `json:"at"`
	Reason   string `json:"reason"`
	Initial  bool   `json:"initial"`
	Files    []struct {
		Path      string `json:"path"`
		Existed   bool   `json:"existed"`
		SHA256    string `json:"sha256"`
		SizeBytes int64  `json:"sizeBytes"`
	} `json:"files"`
}

type backupPage struct {
	Items      []backupItem `json:"items"`
	NextCursor string       `json:"nextCursor"`
	Total      int          `json:"total"`
}

type policyData struct {
	Version      int    `json:"version"`
	DefaultModel string `json:"defaultModel"`
	Models       []struct {
		ID             string `json:"id"`
		Name           string `json:"name"`
		SupportsImages bool   `json:"supportsImages"`
	} `json:"models"`
	Catalogs map[string]json.RawMessage `json:"catalogs"`
}

// toolsShowData 是 `tools show` 的组合视图（单发 REST 的组合，doc/cli.md 登记形状）。
type toolsShowData struct {
	Tool     json.RawMessage `json:"tool"`
	Profiles json.RawMessage `json:"profiles"`
}

// restoreData 是 `restore` 的组合视图：恢复到哪个工具的哪份备份 + 终态。
type restoreData struct {
	ToolID   string          `json:"toolId"`
	BackupID string          `json:"backupId"`
	Apply    json.RawMessage `json:"apply"`
}

// backupsListData 是 `backups list` 的跨工具视图（新 → 旧）。
type backupsListData struct {
	Items []backupRefWire `json:"items"`
	Total int             `json:"total"`
}

// backupRefWire 是 `backups list` 的一行：备份形状 + 归属工具。
type backupRefWire struct {
	ToolID   string `json:"toolId"`
	BackupID string `json:"backupId"`
	At       string `json:"at"`
	Reason   string `json:"reason"`
	Initial  bool   `json:"initial"`
	Files    []struct {
		Path      string `json:"path"`
		Existed   bool   `json:"existed"`
		SHA256    string `json:"sha256"`
		SizeBytes int64  `json:"sizeBytes"`
	} `json:"files"`
}

// ── 人类可读格式化 ──────────────────────────────────────────────────────────

func formatHealth(data json.RawMessage) (string, error) {
	var h healthData
	if err := json.Unmarshal(data, &h); err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "daemon\tstate=%s status=%s uptime=%ds\n", h.State, h.Status, h.UptimeSec)
	fmt.Fprintf(&b, "proxy\trunning=%t port=%d\n", h.ProxyRunning, h.ProxyPort)
	if h.UsageDB.State == "ok" {
		fmt.Fprintf(&b, "usageDb\tstate=ok\n")
	} else {
		fmt.Fprintf(&b, "usageDb\tstate=%s detail=%s\n", h.UsageDB.State, h.UsageDB.Detail)
	}
	fmt.Fprintf(&b, "dataRoot\t%s\n", h.DataRoot)
	fmt.Fprintf(&b, "apiPort\t%d\n", h.APIPort)
	return b.String(), nil
}

// installedStateOf 给出人读面的安装状态（判定与 web 端 installStateOf 同一语义，
// 契约 §4.1：两字段并排出现时以 installedState 为准）。
//
//   - installedState 在闭集内 ⇒ 原样呈现（unknown 是「不可判」，不得折成未安装）；
//   - 字段缺席或取值不在闭集内（旧载荷/未知扩展）⇒ 回退旧布尔口径：
//     true → installed、false → not_installed（对旧守护进程保持可读，防御性回退）。
//
// 只作用于人读面；`--json` 面逐字节转发 REST 载荷（emit 直写原文），不经本函数。
func installedStateOf(t toolDetail) string {
	switch t.InstalledState {
	case installedStateInstalled, installedStateNotInstalled, installedStateUnknown:
		return t.InstalledState
	}
	if t.Installed {
		return installedStateInstalled
	}
	return installedStateNotInstalled
}

func formatToolsList(data json.RawMessage) (string, error) {
	var tools []toolDetail
	if err := json.Unmarshal(data, &tools); err != nil {
		return "", err
	}
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tTIER\tINSTALL_STATE\tVERSION\tACTIVE")
	for _, t := range tools {
		version := t.Version
		if version == "" {
			version = "—"
		}
		active := t.ActiveProfileID
		if active == "" {
			active = "—"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			t.ID, t.Name, t.Tier, installedStateOf(t), version, active)
	}
	_ = w.Flush()
	return b.String(), nil
}

func formatToolsShow(data json.RawMessage) (string, error) {
	var show toolsShowData
	if err := json.Unmarshal(data, &show); err != nil {
		return "", err
	}
	var t toolDetail
	if err := json.Unmarshal(show.Tool, &t); err != nil {
		return "", err
	}
	var p profileList
	if err := json.Unmarshal(show.Profiles, &p); err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "tool\t%s（%s）\n", t.ID, t.Name)
	fmt.Fprintf(&b, "tier\t%s\n", t.Tier)
	fmt.Fprintf(&b, "installedState\t%s\n", installedStateOf(t))
	fmt.Fprintf(&b, "version\t%s\n", orDash(t.Version))
	fmt.Fprintf(&b, "decideKey\t%s\n", t.DecideKey)
	fmt.Fprintf(&b, "activeProfile\t%s\n", orDash(t.ActiveProfileID))
	for _, k := range t.CompanionKeys {
		fmt.Fprintf(&b, "companion\t%s=%s\n", k.Name, orDash(k.Value))
	}
	fmt.Fprintf(&b, "files:\n")
	for _, pi := range t.Paths {
		state := "absent"
		if pi.Exists {
			state = fmt.Sprintf("%dB sha=%s", pi.SizeBytes, short(pi.SHA256))
		}
		fmt.Fprintf(&b, "  %s\t%s\n", pi.Path, state)
	}
	fmt.Fprintf(&b, "profiles:\n")
	for _, card := range p.Items {
		marker := " "
		if card.Active {
			marker = "*"
		}
		fmt.Fprintf(&b, "  %s %s\t%s\tkind=%s\tmodel=%s\n", marker, card.ID, card.Name, card.Kind, orDash(card.ModelID))
	}
	return b.String(), nil
}

func formatPreview(data json.RawMessage) (string, error) {
	var pv previewData
	if err := json.Unmarshal(data, &pv); err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "preview\tpreviewId=%s expiresAt=%s operation=%s\n", pv.PreviewID, pv.ExpiresAt, pv.Operation)
	if pv.Warning != "" {
		fmt.Fprintf(&b, "warning\t%s\n", pv.Warning)
	}
	for _, f := range pv.Files {
		add, del, ctx := 0, 0, 0
		for _, d := range f.Diff {
			switch d.Op {
			case "add":
				add++
			case "del":
				del++
			case "ctx":
				ctx++
			}
		}
		state := "create"
		if f.Existed {
			state = "modify"
		}
		if f.Delete {
			state = "delete"
		}
		fmt.Fprintf(&b, "file\t%s\t%s\tbefore=%dB sha=%s\t+%d -%d ~%d\n",
			f.Path, state, f.BytesBefore, short(f.SHA256Before), add, del, ctx)
		if f.Warning != "" {
			fmt.Fprintf(&b, "  warning\t%s\n", f.Warning)
		}
	}
	return b.String(), nil
}

func formatApply(data json.RawMessage) (string, error) {
	return formatApplyPrefixed("", data)
}

// formatApplyPrefixed 先输出一行前缀（restore 用它标明恢复了哪份备份），再输出 apply 终态。
func formatApplyPrefixed(prefix string, data json.RawMessage) (string, error) {
	var ap applyData
	if err := json.Unmarshal(data, &ap); err != nil {
		return "", err
	}
	var b strings.Builder
	if prefix != "" {
		b.WriteString(prefix)
	}
	fmt.Fprintf(&b, "apply\tstatus=%s\n", ap.Status)
	for _, f := range ap.Files {
		fmt.Fprintf(&b, "file\t%s\t%s\n", f.Path, f.Status)
	}
	if len(ap.BackupIDs) > 0 {
		fmt.Fprintf(&b, "backups\t%s\n", strings.Join(ap.BackupIDs, ","))
	}
	fmt.Fprintf(&b, "activation\ttier=%s activeProfile=%s\n", ap.Activation.Tier, orDash(ap.Activation.ActiveProfileID))
	return b.String(), nil
}

func formatRestore(data json.RawMessage) (string, error) {
	var r restoreData
	if err := json.Unmarshal(data, &r); err != nil {
		return "", err
	}
	prefix := fmt.Sprintf("restore\ttool=%s backupId=%s\n", r.ToolID, r.BackupID)
	return formatApplyPrefixed(prefix, r.Apply)
}

func formatBackupsList(data json.RawMessage) (string, error) {
	var list backupsListData
	if err := json.Unmarshal(data, &list); err != nil {
		return "", err
	}
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TOOL\tBACKUP\tAT\tREASON\tINITIAL\tFILES")
	for _, r := range list.Items {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%t\t%d\n",
			r.ToolID, r.BackupID, r.At, r.Reason, r.Initial, len(r.Files))
	}
	_ = w.Flush()
	fmt.Fprintf(&b, "total\t%d\n", list.Total)
	return b.String(), nil
}

func formatPolicy(data json.RawMessage) (string, error) {
	var p policyData
	if err := json.Unmarshal(data, &p); err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "version\t%d\n", p.Version)
	fmt.Fprintf(&b, "defaultModel\t%s\n", p.DefaultModel)
	ids := make([]string, 0, len(p.Models))
	for _, m := range p.Models {
		ids = append(ids, m.ID)
	}
	fmt.Fprintf(&b, "models\t%s\n", strings.Join(ids, ","))
	keys := make([]string, 0, len(p.Catalogs))
	for k := range p.Catalogs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(&b, "catalogs\t%s\n", strings.Join(keys, ","))
	return b.String(), nil
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func short(sha string) string {
	if len(sha) >= 12 {
		return sha[:12]
	}
	return orDash(sha)
}

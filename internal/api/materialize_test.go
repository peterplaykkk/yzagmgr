package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/family/codex"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

// ── 夹具 ────────────────────────────────────────────────────────────────────

// materializePost 触发一次物化并解析清单。
func materializePost(t *testing.T, srv *contractServer) materializePayload {
	t.Helper()
	res := srv.callRaw(t, http.MethodPost, "/api/v1/materialize", "", true)
	if !res.ok {
		t.Fatalf("POST /materialize 失败: %s", res.body)
	}
	var payload materializePayload
	if err := json.Unmarshal(res.data, &payload); err != nil {
		t.Fatalf("物化清单不是预期结构: %v（%s）", err, string(res.data))
	}
	return payload
}

// materializeGet 只读盘点（与 POST 同形，但不写任何字节）。
func materializeGet(t *testing.T, srv *contractServer) materializePayload {
	t.Helper()
	res := srv.callRaw(t, http.MethodGet, "/api/v1/materialize", "", true)
	if !res.ok {
		t.Fatalf("GET /materialize 失败: %s", res.body)
	}
	var payload materializePayload
	if err := json.Unmarshal(res.data, &payload); err != nil {
		t.Fatalf("物化清单不是预期结构: %v（%s）", err, string(res.data))
	}
	return payload
}

// familyOf 取清单里某个单元的条目（缺席即失败：单元列表本身是契约的一部分）。
func familyOf(t *testing.T, payload materializePayload, tool family.ID) materializeFamily {
	t.Helper()
	for _, entry := range payload.Families {
		if entry.ToolID == string(tool) {
			return entry
		}
	}
	t.Fatalf("物化清单里没有单元 %s（有 %d 个）", tool, len(payload.Families))
	return materializeFamily{}
}

// profileOf 取某单元某张卡的条目。
func profileOf(t *testing.T, entry materializeFamily, profileID string) materializeProfile {
	t.Helper()
	for _, p := range entry.Profiles {
		if p.ProfileID == profileID {
			return p
		}
	}
	t.Fatalf("%s 的清单里没有档位卡 %s（有 %d 张）", entry.ToolID, profileID, len(entry.Profiles))
	return materializeProfile{}
}

// managedCardID 是未登录时的受管卡 id（uid 回落成 local，见 gatewayinput.go）。
const managedCardID = "yzagmgr-local"

// applyCard 走完整的预览 → 应用通道把某张卡写进 live。
func applyCard(t *testing.T, srv *contractServer, tool family.ID, pid string) {
	t.Helper()
	preview := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+string(tool)+"/preview",
		`{"profileId":"`+pid+`"}`, true)
	if !preview.ok {
		t.Fatalf("%s 预览失败: %s", tool, preview.body)
	}
	var plan struct {
		PreviewID string `json:"previewId"`
	}
	if err := json.Unmarshal(preview.data, &plan); err != nil {
		t.Fatalf("预览载荷不是预期结构: %v", err)
	}
	apply := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+string(tool)+"/apply",
		`{"previewId":"`+plan.PreviewID+`"}`, true)
	if !apply.ok {
		t.Fatalf("%s 应用失败: %s", tool, apply.body)
	}
}

// createUserCard 通过 API 建一张自建卡并返回它的 id。
func createUserCard(t *testing.T, srv *contractServer, tool family.ID, name string) string {
	t.Helper()
	res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+string(tool)+"/profiles",
		`{"name":"`+name+`"}`, true)
	if !res.ok {
		t.Fatalf("%s 建卡失败: %s", tool, res.body)
	}
	var view struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(res.data, &view); err != nil || view.ID == "" {
		t.Fatalf("建卡响应里没有 id: %v（%s）", err, string(res.data))
	}
	return view.ID
}

// mustReadFile 读一个必须存在的文件。
func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return raw
}

// mustNotExist 断言路径不存在（物化不该产出的目录、不该被写入的 live 文件）。
func mustNotExist(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s 不该存在（%s），err=%v", path, why, err)
	}
}

// liveSnapshot 记录「live 侧」的全部可观察状态：各单元受管路径（含缺席）、备份目录、
// 档位卡记账。物化前后必须逐字节相同（A2 的硬边界）。
func liveSnapshot(t *testing.T, srv *contractServer) map[string]string {
	t.Helper()
	out := map[string]string{}
	record := func(label, path string) {
		if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
			out[label] = "sha:" + tx.HashBytes(mustReadFile(t, path))
			return
		}
		out[label] = "<absent>"
	}
	for _, id := range srv.Families.IDs() {
		f := srv.Families.MustLookup(id)
		for _, path := range f.Paths(srv.env) {
			record(string(id)+":"+path, path)
		}
	}
	// 备份与非受管文件同样要在场断言：物化不得产出备份、不得碰 live 之外的路径。
	record("profile-store", filepath.Join(srv.env.ManagedDir(), "profiles.json"))
	record("records", srv.env.RecordsFile())
	entries, err := os.ReadDir(srv.env.BackupsDir())
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("读取备份目录失败: %v", err)
	}
	for _, entry := range entries {
		record("backup:"+entry.Name(), filepath.Join(srv.env.BackupsDir(), entry.Name()))
	}
	return out
}

// materializedState 记录物化根下每个文件的字节与 mtime（A5 的「字节与 mtime 均不变」）。
func materializedState(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		st, statErr := d.Info()
		if statErr != nil {
			return statErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		out[filepath.ToSlash(rel)] = tx.HashBytes(mustReadFile(t, path)) + "@" + st.ModTime().String()
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("遍历物化根失败: %v", err)
	}
	return out
}

// dirFileNames 列出目录里的文件（相对路径，用于断言布局）。
func dirFileNames(t *testing.T, dir string) []string {
	t.Helper()
	out := materializedFileNames(dir)
	sort.Strings(out)
	return out
}

// ── A1 目录布局 ─────────────────────────────────────────────────────────────

// TestMaterializeWritesPerProfileDirectories 守住 A1：每个单元 × 每张卡一个目录，
// 文件名取 live 路径的 basename，同一单元的不同卡互不覆盖。
func TestMaterializeWritesPerProfileDirectories(t *testing.T) {
	srv := newContractServer(t)
	userCard := createUserCard(t, srv, family.Claude, "对照卡")
	payload := materializePost(t, srv)

	if want := filepath.Join(srv.env.Home, materializeDirName, materializeLeafName); payload.Root != want {
		t.Fatalf("物化根 = %q，期望 %q", payload.Root, want)
	}

	cases := []struct {
		tool  family.ID
		files []string
	}{
		// codex 的受管目录文件（数据根下）与 config.toml 同批落盘：basename 各自成立。
		{family.Codex, []string{"codex-model-catalog.json", "config.toml"}},
		{family.Claude, []string{"settings.json"}},
		{family.Pi, []string{"models.json", "settings.json"}},
		{family.Gemini, []string{".env", "settings.json"}},
		// opencode 是 A1 的逐族布局例外：它的配置根**是目录**
		// （`$XDG_CONFIG_HOME/opencode/opencode.json`），故物化名多一层。
		{family.OpenCode, []string{"opencode/opencode.json"}},
	}
	for _, tc := range cases {
		entry := familyOf(t, payload, tc.tool)
		profile := profileOf(t, entry, managedCardID)
		if want := filepath.Join(payload.Root, string(tc.tool), managedCardID); profile.Dir != want {
			t.Errorf("%s 的目录 = %q，期望 %q", tc.tool, profile.Dir, want)
		}
		got := dirFileNames(t, profile.Dir)
		if strings.Join(got, ",") != strings.Join(tc.files, ",") {
			t.Errorf("%s 目录里的文件 = %v，期望 %v", tc.tool, got, tc.files)
		}
		if strings.Join(profile.Files, ",") != strings.Join(got, ",") {
			t.Errorf("%s 清单里的 files = %v，与盘上 %v 不一致", tc.tool, profile.Files, got)
		}
	}

	// claude-desktop 的两个标记文件**同名**（Claude/ 与 Claude-3p/ 下各一个）：
	// 扁平布局会互相覆盖，因此退回父目录名作前缀，四个受管文件全部在场。
	desktop := profileOf(t, familyOf(t, payload, family.ClaudeDesktop), managedCardID)
	wantDesktop := []string{
		"Claude-3p/claude_desktop_config.json",
		"Claude/claude_desktop_config.json",
		"_meta.json",
		config.ClaudeDesktopProfileID + ".json",
	}
	sort.Strings(wantDesktop)
	if got := dirFileNames(t, desktop.Dir); strings.Join(got, ",") != strings.Join(wantDesktop, ",") {
		t.Errorf("claude-desktop 目录里的文件 = %v，期望 %v", got, wantDesktop)
	}

	// 同一单元的不同卡各自成目录：受管卡、官方卡、自建卡三者并存。
	claude := familyOf(t, payload, family.Claude)
	managed := profileOf(t, claude, managedCardID)
	official := profileOf(t, claude, OfficialProfileID)
	user := profileOf(t, claude, userCard)
	dirs := map[string]bool{}
	for _, p := range []materializeProfile{managed, official, user} {
		if p.Dir == "" || dirs[p.Dir] {
			t.Fatalf("三张卡的目录必须两两不同，得到 %q", p.Dir)
		}
		dirs[p.Dir] = true
		mustReadFile(t, filepath.Join(p.Dir, "settings.json"))
	}
	if bytes.Equal(mustReadFile(t, filepath.Join(managed.Dir, "settings.json")),
		mustReadFile(t, filepath.Join(official.Dir, "settings.json"))) {
		t.Errorf("受管卡与官方卡的内容不该相同（官方档是官方种子形态）")
	}
}

// ── A2 零触碰 live ──────────────────────────────────────────────────────────

// TestMaterializeNeverTouchesLiveConfig 守住 A2：物化不写 live、不改记账、不产备份。
func TestMaterializeNeverTouchesLiveConfig(t *testing.T) {
	srv := newContractServer(t)
	// 先放一份「用户自己的」live 内容：物化的边界是「连它的字节都不许变」。
	if err := os.MkdirAll(filepath.Dir(srv.env.CodexConfig()), 0o700); err != nil {
		t.Fatalf("建立 codex 目录失败: %v", err)
	}
	userConfig := []byte("model = \"gpt-5.6-sol\"\n\n[projects.\"/tmp/x\"]\ntrust_level = \"trusted\"\n")
	if err := os.WriteFile(srv.env.CodexConfig(), userConfig, 0o600); err != nil {
		t.Fatalf("写用户 codex 配置失败: %v", err)
	}
	before := liveSnapshot(t, srv)
	appliedBefore := srv.profiles.snapshot("codex").Applied

	materializePost(t, srv)

	after := liveSnapshot(t, srv)
	for label, want := range before {
		if got := after[label]; got != want {
			t.Errorf("物化改动了 live 侧 %s：%s → %s", label, want, got)
		}
	}
	if appliedBefore != srv.profiles.snapshot("codex").Applied {
		t.Errorf("物化改动了生效档记账：%q → %q", appliedBefore, srv.profiles.snapshot("codex").Applied)
	}
	// 用户自己的键逐字节存活（真正被碰过就会在这里现形）。
	if got := string(mustReadFile(t, srv.env.CodexConfig())); got != string(userConfig) {
		t.Errorf("codex live 配置被改动：\n%s", got)
	}
	// 物化确实写了东西（否则上面的「零改动」是空断言）。
	root := filepath.Join(srv.env.Home, materializeDirName, materializeLeafName)
	if st, err := os.Stat(filepath.Join(root, "codex", managedCardID, "config.toml")); err != nil || st.IsDir() {
		t.Fatalf("物化没有写出 codex 的受管卡目录: %v", err)
	}
}

// ── A3 内容等价 ─────────────────────────────────────────────────────────────

// TestMaterializedBytesEqualApplyBytes 守住 A3：物化文件 == 该卡经 preview/apply 写进
// live 的字节（同一 `planForProfile`；唯一例外是 A4 的路径改写，单独断言）。
func TestMaterializedBytesEqualApplyBytes(t *testing.T) {
	cases := []struct {
		tool family.ID
		name string
	}{
		{family.Claude, "claude"},
		{family.Codex, "codex"},
		{family.Gemini, "gemini"},
		{family.Pi, "pi"},
		{family.OpenCode, "opencode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newContractServer(t)
			// 起点先物化一次：证明「apply 之后的目录刷新」是它自己的功劳，不是这一轮的残留。
			materializePost(t, srv)
			applyCard(t, srv, tc.tool, managedCardID)
			// 不显式 POST：apply 的收尾必须已经刷新了目录（live 变了，计划就变了）。
			payload := materializeGet(t, srv)
			profile := profileOf(t, familyOf(t, payload, tc.tool), managedCardID)

			f := srv.Families.MustLookup(tc.tool)
			for _, livePath := range f.Paths(srv.env) {
				if _, err := os.Stat(livePath); err != nil {
					continue // 该档不涉及这个文件（家族以「缺席」表达）
				}
				live := mustReadFile(t, livePath)
				// 物化名的规则在这里**独立写出**（不复用实现里的取名函数）：扁平取 basename，
				// opencode 与 claude-desktop 各有一条逐族例外（A1）。
				rel := filepath.Base(livePath)
				if tc.tool == family.OpenCode {
					rel = filepath.ToSlash(filepath.Join("opencode", rel))
				}
				materializedPath := filepath.Join(profile.Dir, filepath.FromSlash(rel))
				materialized := mustReadFile(t, materializedPath)
				if tc.tool == family.Codex && strings.HasSuffix(livePath, "config.toml") {
					// codex 的 config.toml 里嵌着指向数据根的 `model_catalog_json`：
					// 把 A4 的那一次替换**只**施加在 live 侧字节上，其余必须逐字节相同。
					dataCatalog := codex.CatalogPath(srv.env)
					dirCatalog := filepath.Join(profile.Dir, filepath.Base(dataCatalog))
					live = bytes.ReplaceAll(live, []byte(dataCatalog), []byte(dirCatalog))
				}
				if !bytes.Equal(live, materialized) {
					t.Errorf("%s 与 live 不一致：\n--- live (%s)\n%s\n--- 物化 (%s)\n%s",
						livePath, livePath, live, materializedPath, materialized)
				}
			}
		})
	}
}

// ── A4 路径自洽 ─────────────────────────────────────────────────────────────

// TestMaterializeRewritesDataRootReferences 守住 A4 的前半：指向数据根的绝对路径被改写成
// 物化目录内的路径，且**只做这一件事**（其余字节一个不动）。
func TestMaterializeRewritesDataRootReferences(t *testing.T) {
	srv := newContractServer(t)
	payload := materializePost(t, srv)
	profile := profileOf(t, familyOf(t, payload, family.Codex), managedCardID)

	dataCatalog := codex.CatalogPath(srv.env)
	if !strings.HasPrefix(dataCatalog, srv.env.DataRoot()) {
		t.Fatalf("codex 的托管目录不在数据根下（%s），本条断言的场景不成立", dataCatalog)
	}
	configPath := filepath.Join(profile.Dir, "config.toml")
	got := string(mustReadFile(t, configPath))
	if strings.Contains(got, dataCatalog) {
		t.Errorf("物化后的 config.toml 仍指向数据根：%s", dataCatalog)
	}
	dirCatalog := filepath.Join(profile.Dir, filepath.Base(dataCatalog))
	if !strings.Contains(got, dirCatalog) {
		t.Errorf("物化后的 config.toml 没有指向目录内的目录文件 %s：\n%s", dirCatalog, got)
	}

	// 「只做这一件事」：把同一张卡写进 live，再对 live 字节施加同一替换，两侧必须逐字节相等。
	applyCard(t, srv, family.Codex, managedCardID)
	live := mustReadFile(t, srv.env.CodexConfig())
	want := bytes.ReplaceAll(live, []byte(dataCatalog), []byte(dirCatalog))
	if !bytes.Equal(want, mustReadFile(t, configPath)) {
		t.Errorf("改写范围超出「数据根前缀 → 物化目录」这一条：\n--- 期望\n%s\n--- 实际\n%s",
			want, mustReadFile(t, configPath))
	}
}

// TestCodexMaterializedCatalogPointerResolves 守住 A4 的后半：物化后的
// `model_catalog_json` 指针必须在**目录内**有对应文件，目录因此可独立使用、可整体搬走。
func TestCodexMaterializedCatalogPointerResolves(t *testing.T) {
	srv := newContractServer(t)
	payload := materializePost(t, srv)
	profile := profileOf(t, familyOf(t, payload, family.Codex), managedCardID)

	raw := string(mustReadFile(t, filepath.Join(profile.Dir, "config.toml")))
	pointer := ""
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(line, "model_catalog_json") {
			continue
		}
		_, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("model_catalog_json 行不是键值形态: %q", line)
		}
		pointer = strings.Trim(strings.TrimSpace(value), `"`)
	}
	if pointer == "" {
		t.Fatalf("物化后的 config.toml 里没有 model_catalog_json：\n%s", raw)
	}
	if !filepath.IsAbs(pointer) {
		t.Fatalf("model_catalog_json 必须是绝对路径（codex 不展开 ~）：%q", pointer)
	}
	if !strings.HasPrefix(pointer, profile.Dir+string(filepath.Separator)) {
		t.Fatalf("model_catalog_json 没有落在物化目录内：%q（目录 %q）", pointer, profile.Dir)
	}
	catalog := mustReadFile(t, pointer)
	if len(bytes.TrimSpace(catalog)) == 0 {
		t.Fatalf("指针指向的目录文件是空的：%s", pointer)
	}
	var doc struct {
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
	}
	if err := json.Unmarshal(catalog, &doc); err != nil || len(doc.Models) == 0 {
		t.Fatalf("指针指向的文件不是可用的 codex 目录（err=%v）：%s", err, catalog)
	}
}

// ── A5 幂等与清理 ───────────────────────────────────────────────────────────

// TestMaterializeIsIdempotent 守住 A5 的前半：同一状态重复物化，字节与 mtime 均不变。
func TestMaterializeIsIdempotent(t *testing.T) {
	srv := newContractServer(t)
	first := materializePost(t, srv)
	root := first.Root
	before := materializedState(t, root)
	if len(before) == 0 {
		t.Fatalf("首次物化什么都没写")
	}
	materializePost(t, srv)
	after := materializedState(t, root)
	if len(before) != len(after) {
		t.Fatalf("重复物化改变了文件数：%d → %d", len(before), len(after))
	}
	for name, want := range before {
		if got := after[name]; got != want {
			t.Errorf("重复物化改动了 %s：%s → %s", name, want, got)
		}
	}
}

// TestMaterializeRemovesDeletedCardDirectories 守住 A5 的后半：卡片删除后其目录在下次
// 物化时被移除，其它卡的目录不受影响。
func TestMaterializeRemovesDeletedCardDirectories(t *testing.T) {
	srv := newContractServer(t)
	card := createUserCard(t, srv, family.Claude, "临时卡")
	payload := materializePost(t, srv)
	dir := profileOf(t, familyOf(t, payload, family.Claude), card).Dir
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("自建卡的物化目录没有建立: %v", err)
	}
	managedDir := profileOf(t, familyOf(t, payload, family.Claude), managedCardID).Dir

	if res := srv.callRaw(t, http.MethodDelete, "/api/v1/tools/claude/profiles/"+card, "", true); !res.ok {
		t.Fatalf("删除自建卡失败: %s", res.body)
	}
	// 删除本身不触发物化（A5 的语义是「**下次**物化时被移除」）。
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("删除卡片不该立刻动目录（应由下次物化收敛）: %v", err)
	}
	materializePost(t, srv)
	mustNotExist(t, dir, "卡片已删除，目录是孤儿")
	if _, err := os.Stat(managedDir); err != nil {
		t.Fatalf("受管卡的目录被误删: %v", err)
	}
}

// ── A6 隔离 ─────────────────────────────────────────────────────────────────

// TestMaterializeRespectsIsolatedHome 守住 A6：物化根取 `Env.Home` 下的 `.yzagmgr/materialize`，
// 因此 `YZAGMGR_HOME` 一旦设定即自动隔离，隔离运行绝不写真实 `~/.yzagmgr/materialize`。
func TestMaterializeRespectsIsolatedHome(t *testing.T) {
	gate := t.TempDir()
	t.Setenv(config.EnvHomeVar, gate)
	// 总闸的解析链：`YZAGMGR_HOME` → `Env.Home`（物化根的唯一输入）。
	isolated, err := config.FromProcess()
	if err != nil {
		t.Fatalf("按进程环境构造 Env 失败: %v", err)
	}
	if !isolated.Isolated || isolated.Home != gate {
		t.Fatalf("YZAGMGR_HOME 未生效：Isolated=%v Home=%q（期望 %q）", isolated.Isolated, isolated.Home, gate)
	}

	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("取真实主目录失败: %v", err)
	}
	realRoot := filepath.Join(realHome, materializeDirName, materializeLeafName)
	_, realErr := os.Stat(realRoot)
	realExistedBefore := realErr == nil

	srv := newContractServer(t) // 它的 Env.Home 是注入点（生产里就是总闸的值）
	payload := materializePost(t, srv)
	if want := filepath.Join(srv.env.Home, materializeDirName, materializeLeafName); payload.Root != want {
		t.Fatalf("物化根没有跟着 Env.Home：%q，期望 %q", payload.Root, want)
	}
	for _, entry := range payload.Families {
		for _, profile := range entry.Profiles {
			if !strings.HasPrefix(profile.Dir, payload.Root+string(filepath.Separator)) {
				t.Errorf("%s 的目录逃出了物化根：%q", entry.ToolID, profile.Dir)
			}
		}
	}
	// 总闸指的那个目录里不该出现物化根（物化只认 Env.Home，不去读进程 env）。
	mustNotExist(t, filepath.Join(gate, materializeDirName, materializeLeafName), "物化根只认 Env.Home")
	// 照行真实 ~/.yzagmgr/materialize：本轮之前不存在的话，之后也不该存在（隔离运行的硬要求）。
	if !realExistedBefore {
		mustNotExist(t, realRoot, "隔离运行绝不写真实 ~/.yzagmgr/materialize")
	}
}

// ── A7 启动命令 ─────────────────────────────────────────────────────────────

// TestOpenCodeMaterializeUsesXDGConfigRoot 守住 opencode 的 A1/A7 例外：它的配置根是
// **目录**（`$XDG_CONFIG_HOME/opencode/opencode.json`），因此物化名多一层、命令用
// `XDG_CONFIG_HOME` 指目录。
//
// 反例（改回扁平 `opencode.json` + `OPENCODE_CONFIG=<dir>/opencode.json`）经真机复核
// **不是隔离运行**：设了 `OPENCODE_CONFIG`，opencode 仍会去读 `$XDG_CONFIG_HOME/opencode/*`
// （它自己的日志逐条列出被探测的路径）。
func TestOpenCodeMaterializeUsesXDGConfigRoot(t *testing.T) {
	srv := newContractServer(t)
	payload := materializePost(t, srv)
	profile := profileOf(t, familyOf(t, payload, family.OpenCode), managedCardID)

	wantFile := "opencode/opencode.json"
	if got := strings.Join(dirFileNames(t, profile.Dir), ","); got != wantFile {
		t.Errorf("opencode 目录里的文件 = %q，期望 %q", got, wantFile)
	}
	if got := strings.Join(profile.Files, ","); got != wantFile {
		t.Errorf("清单里的 files = %q，期望 %q", got, wantFile)
	}
	if want := "XDG_CONFIG_HOME=" + profile.Dir + " opencode"; profile.LaunchCommand != want {
		t.Errorf("启动命令 = %q，期望 %q", profile.LaunchCommand, want)
	}
	if !strings.HasPrefix(profile.LaunchCommand, "XDG_CONFIG_HOME=") {
		t.Fatalf("命令没有把配置根指到物化目录：%q", profile.LaunchCommand)
	}
	// 命令里既不能出现已作废的 `OPENCODE_CONFIG`，也不能顺手把 data/cache 一起隔离
	// （实测那样会让 opencode 去新缓存里抓 provider 包并零输出挂死）。
	for _, banned := range []string{"OPENCODE_CONFIG", "XDG_DATA_HOME", "XDG_CACHE_HOME"} {
		if strings.Contains(profile.LaunchCommand, banned) {
			t.Errorf("命令里不该出现 %s：%q", banned, profile.LaunchCommand)
		}
	}
	// 盘上事实：那一份确实在 `opencode/` 子目录下；同时不该留一份扁平副本
	// （两份会让人分不清哪份生效——opencode 只读 `opencode/` 下的那份）。
	nested := filepath.Join(profile.Dir, "opencode", "opencode.json")
	if _, err := os.Stat(nested); err != nil {
		t.Fatalf("`<dir>/opencode/opencode.json` 不存在: %v", err)
	}
	var doc struct {
		Model      string `json:"model"`
		SmallModel string `json:"small_model"`
		Provider   map[string]struct {
			Options struct {
				BaseURL string `json:"baseURL"`
			} `json:"options"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(mustReadFile(t, nested), &doc); err != nil {
		t.Fatalf("物化产物不是合法 JSON: %v", err)
	}
	if !strings.HasPrefix(doc.Model, managedCardID+"/") {
		t.Errorf("顶层 model 必须指向受管 provider（%s/…），got=%q", managedCardID, doc.Model)
	}
	// 第二个路由指针必须同样落在物化产物里：启动命令把客户端指到这个目录（`XDG_CONFIG_HOME`），
	// 少写它就会出现「主模型走网关、标题生成/压缩这类辅助调用仍走用户自己的 provider」
	// ——物化产物是隔离实例的 live 配置，它必须与 apply 会写的字节一致（A3）。
	if doc.SmallModel != doc.Model {
		t.Errorf("顶层 small_model 必须与 model 同值（%q），got=%q", doc.Model, doc.SmallModel)
	}
	mustNotExist(t, filepath.Join(profile.Dir, "opencode.json"), "opencode 只读 opencode/ 下的那份，扁平副本会误导")
}

// TestMaterializeLaunchCommandsPerFamily 守住 A7：命令由后端产出、把配置根指到物化目录，
// 且「已验证 / 未验证」如实分档（未验证的族必须给出非空原因）。
func TestMaterializeLaunchCommandsPerFamily(t *testing.T) {
	srv := newContractServer(t)
	payload := materializePost(t, srv)

	// 「已验证」是真机结论而不是实现细节，因此在这里**钉住集合**：某天把未验证的族悄悄翻成
	// true（或反过来）都必须先改这张表。出处：UC-019 附录的「真机验证登记」（2026-09-22）。
	wantVerified := map[family.ID]bool{
		family.Claude:   true,
		family.Codex:    true,
		family.Pi:       true,
		family.OpenCode: true,
	}
	verified := func(id family.ID) bool { return wantVerified[id] }

	for _, id := range family.All {
		if id == family.WorkBuddy {
			continue // 独立入口：见下方 partialFailures 的断言
		}
		entry := familyOf(t, payload, id)
		if entry.LaunchVerified != verified(id) {
			t.Errorf("%s 的 launchVerified = %v，期望 %v", id, entry.LaunchVerified, verified(id))
		}
		if !entry.LaunchVerified && strings.TrimSpace(entry.LaunchNote) == "" {
			t.Errorf("%s 未验证却没有给出原因（launchNote 为空）", id)
		}
		if entry.LaunchVerified && strings.TrimSpace(entry.LaunchNote) == "" {
			t.Errorf("%s 标记为已验证，但没写明验证口径（launchNote 为空）", id)
		}
		for _, profile := range entry.Profiles {
			if profile.LaunchCommand == "" {
				continue // 无 CLI 入口的单元（桌面应用、未装且变量未证实）如实留空
			}
			if !strings.Contains(profile.LaunchCommand, profile.Dir) {
				t.Errorf("%s 的命令没有带上该目录：%q（目录 %q）", id, profile.LaunchCommand, profile.Dir)
			}
			if strings.Contains(profile.LaunchCommand, srv.env.DataRoot()) {
				t.Errorf("%s 的命令引用了产品数据根，目录不可整体搬走：%q", id, profile.LaunchCommand)
			}
		}
	}

	// claude：隔离配置目录**不足以**驱动真 CLI（登录门在 route 之前），因此命令必须显式
	// 给出 settings.json 里那三个 route 键，且值必须与盘上那份逐字相同（同源）。
	claude := familyOf(t, payload, family.Claude)
	claudeProfile := profileOf(t, claude, managedCardID)
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(mustReadFile(t, filepath.Join(claudeProfile.Dir, "settings.json")), &settings); err != nil {
		t.Fatalf("claude 的 settings.json 不是预期结构: %v", err)
	}
	wantCommand := "CLAUDE_CONFIG_DIR=" + shellValue(claudeProfile.Dir)
	for _, key := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN"} {
		if settings.Env[key] == "" {
			t.Fatalf("夹具里没有 %s，本条断言失去意义", key)
		}
		wantCommand += " " + key + "=" + settings.Env[key]
	}
	if settings.Env["ANTHROPIC_AUTH_TOKEN"] != "PROXY_MANAGED" {
		t.Fatalf("managed Claude fixture must keep the placeholder token, got %q", settings.Env["ANTHROPIC_AUTH_TOKEN"])
	}
	if settings.Env["ANTHROPIC_MODEL"] != "claude-fable-5-1[1m]" {
		t.Fatalf("managed Claude model = %q, want exact 1M model id", settings.Env["ANTHROPIC_MODEL"])
	}
	wantCommand += " ANTHROPIC_MODEL='claude-fable-5-1[1m]'"
	wantCommand += " claude"
	if claudeProfile.LaunchCommand != wantCommand {
		t.Errorf("claude 的启动命令 = %q，期望 %q", claudeProfile.LaunchCommand, wantCommand)
	}
	// 官方卡没有 route 键（官方档走用户自己的上游登录态）：命令只指配置目录。
	officialCommand := profileOf(t, claude, OfficialProfileID).LaunchCommand
	if strings.Contains(officialCommand, "ANTHROPIC_") {
		t.Errorf("claude 官方卡的命令不该声明 route 键：%q", officialCommand)
	}

	// codex：命令必须把 base_url 覆盖到本地代理（网关直连需要目录里没有的凭据，实测 401）。
	codexCommand := profileOf(t, familyOf(t, payload, family.Codex), managedCardID).LaunchCommand
	if !strings.HasPrefix(codexCommand, "CODEX_HOME="+mustDir(t, payload, family.Codex)+" codex") {
		t.Errorf("codex 的命令没有以 CODEX_HOME 开头：%q", codexCommand)
	}
	if !strings.Contains(codexCommand, "-c model_providers.custom.base_url=http://127.0.0.1:") {
		t.Errorf("codex 的命令没有把 base_url 指到本地代理：%q", codexCommand)
	}

	// 其余族的配置根变量逐条对齐 acceptance-recipe §B.2（或仓库内既有实测用法）。
	roots := map[family.ID]string{
		family.Pi:       "PI_CODING_AGENT_DIR=",
		family.Hermes:   "HERMES_HOME=",
		family.OpenCode: "XDG_CONFIG_HOME=",
		family.OpenClaw: "OPENCLAW_CONFIG_PATH=",
	}
	for id, prefix := range roots {
		command := profileOf(t, familyOf(t, payload, id), managedCardID).LaunchCommand
		if !strings.HasPrefix(command, prefix) {
			t.Errorf("%s 的命令没有用 %q 开头：%q", id, prefix, command)
		}
	}

	//（WorkBuddy 跳过/不产目录断言随 family.WorkBuddy 退出 All 注销——design §1.1。）
}

// mustDir 取某单元受管卡的目录（命令断言的拼接用）。
func mustDir(t *testing.T, payload materializePayload, tool family.ID) string {
	t.Helper()
	return profileOf(t, familyOf(t, payload, tool), managedCardID).Dir
}

// hasFailure 报告清单里是否登记了某个失败项。
func hasFailure(payload materializePayload, tool family.ID, profileID string) bool {
	for _, failure := range payload.PartialFailures {
		if failure.ToolID == string(tool) && failure.ProfileID == profileID {
			return true
		}
	}
	return false
}

// ── A8 官方卡与 additive 族 ─────────────────────────────────────────────────

// TestMaterializeSkipsFamiliesWithoutOfficialTier 守住 A8：有官方形态的单元物化官方档字节；
// additive 族（无官方形态）既**不产空目录**也不报错。
func TestMaterializeSkipsFamiliesWithoutOfficialTier(t *testing.T) {
	srv := newContractServer(t)
	payload := materializePost(t, srv)

	additive := []family.ID{family.Pi, family.OpenCode, family.OpenClaw, family.Hermes}
	seeded := []family.ID{family.Claude, family.ClaudeDesktop, family.Codex, family.Gemini, family.Grokbuild}
	for _, id := range additive {
		f := srv.Families.MustLookup(id)
		if f.OfficialSeeded() {
			t.Fatalf("%s 有官方形态，本条的夹具假设不成立", id)
		}
		for _, profile := range familyOf(t, payload, id).Profiles {
			if profile.ProfileID == OfficialProfileID || profile.Kind == ProfileKindOfficial {
				t.Errorf("%s 没有官方形态，却列出了官方卡", id)
			}
		}
		mustNotExist(t, filepath.Join(payload.Root, string(id), OfficialProfileID), "additive 族不该产官方目录")
		if hasFailure(payload, id, OfficialProfileID) {
			t.Errorf("%s 没有官方形态属于正常，不该登记为失败", id)
		}
	}
	for _, id := range seeded {
		if !srv.Families.MustLookup(id).OfficialSeeded() {
			t.Fatalf("%s 无官方形态，本条的夹具假设不成立", id)
		}
		profileOf(t, familyOf(t, payload, id), OfficialProfileID)
	}

	// 官方卡的字节必须是 `PlanOfficial` 的产出（不是受管档、也不是空文件）。
	claudeProfile := profileOf(t, familyOf(t, payload, family.Claude), OfficialProfileID)
	plans, err := srv.Families.MustLookup(family.Claude).PlanOfficial(srv.env)
	if err != nil {
		t.Fatalf("计算 claude 官方档失败: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("claude 官方档的计划数 = %d，期望 1", len(plans))
	}
	got := mustReadFile(t, filepath.Join(claudeProfile.Dir, filepath.Base(plans[0].Path)))
	if !bytes.Equal(got, plans[0].After) {
		t.Errorf("官方卡物化的字节不是官方档字节：\n--- 期望\n%s\n--- 实际\n%s", plans[0].After, got)
	}
}

// ── A9 失败不半成品 ─────────────────────────────────────────────────────────

// TestMaterializeReportsPartialFailureWithoutSideEffects 守住 A9：单卡算不出来只登记这一项，
// 其余单元照常完成；失败不触碰 live，也不把上一轮的目录删掉。
func TestMaterializeReportsPartialFailureWithoutSideEffects(t *testing.T) {
	srv := newContractServer(t)
	// 造一张「模型已不在策略内」的卡：不是编造的边角情形——策略换过模型之后，
	// 既有的自建卡正好落在这个状态，而它必须能被单独报出来而不是带崩整轮物化。
	card, err := srv.profiles.create(string(family.Claude), userCardSpec{Name: "漂移卡", ModelID: "not-in-policy"})
	if err != nil {
		t.Fatalf("造漂移卡失败: %v", err)
	}
	stale := filepath.Join(srv.env.Home, materializeDirName, materializeLeafName, string(family.Claude), card.ID)
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatalf("预置上一轮目录失败: %v", err)
	}
	sentinel := filepath.Join(stale, "settings.json")
	if err := os.WriteFile(sentinel, []byte("上一轮的字节\n"), 0o600); err != nil {
		t.Fatalf("预置上一轮内容失败: %v", err)
	}

	before := liveSnapshot(t, srv)
	payload := materializePost(t, srv) // 部分成功是正常结果：信封仍然是 ok
	after := liveSnapshot(t, srv)
	for label, want := range before {
		if got := after[label]; got != want {
			t.Errorf("部分失败的一轮改动了 live 侧 %s：%s → %s", label, want, got)
		}
	}

	if !hasFailure(payload, family.Claude, card.ID) {
		t.Fatalf("漂移卡没有被登记为失败：%+v", payload.PartialFailures)
	}
	reason := ""
	for _, failure := range payload.PartialFailures {
		if failure.ToolID == string(family.Claude) && failure.ProfileID == card.ID {
			reason = failure.Reason
		}
	}
	if !strings.Contains(reason, "不在策略内") {
		t.Errorf("失败原因没有说清楚（%q）", reason)
	}
	// 同一单元里其它卡照常完成。
	for _, pid := range []string{managedCardID, OfficialProfileID} {
		profile := profileOf(t, familyOf(t, payload, family.Claude), pid)
		if len(profile.Files) == 0 {
			t.Errorf("%s 的目录没有物化（同族的另一张卡失败不该影响它）", pid)
		}
	}
	// 其它单元照常完成。
	if len(profileOf(t, familyOf(t, payload, family.Codex), managedCardID).Files) == 0 {
		t.Errorf("codex 的目录没有物化（claude 的一张卡失败不该影响别的单元）")
	}
	// 失败卡的目录保持原状，不删不留半成品。
	if got := string(mustReadFile(t, sentinel)); got != "上一轮的字节\n" {
		t.Errorf("失败卡上一轮的目录被改动了：%q", got)
	}
}

// ── §4.4 的 byDesign：设计性跳过与真失败必须可机器区分 ──────────────────────

// wireFailures 触发一次物化，并按**线上 JSON**取回 `partialFailures`。
//
// 每条是一个「键 → 原始值」的映射：**缺席与显式 false 只有在这一层才分得开**，
// 而 `omitempty` 的契约恰恰有一半落在缺席上（见 TestMaterializeByDesignFlagOnTheWire）。
func wireFailures(t *testing.T, srv *contractServer) []map[string]json.RawMessage {
	t.Helper()
	res := srv.callRaw(t, http.MethodPost, "/api/v1/materialize", "", true)
	if !res.ok {
		t.Fatalf("POST /materialize 失败: %s", res.body)
	}
	var wire struct {
		PartialFailures []map[string]json.RawMessage `json:"partialFailures"`
	}
	if err := json.Unmarshal(res.data, &wire); err != nil {
		t.Fatalf("物化清单不是预期结构: %v（%s）", err, string(res.data))
	}
	return wire.PartialFailures
}

// wireFailureOf 按 toolId/profileId 取一条失败条目（取不到即 fatal：条目本身必须可见）。
func wireFailureOf(
	t *testing.T, entries []map[string]json.RawMessage, toolID, profileID string,
) map[string]json.RawMessage {
	t.Helper()
	for _, entry := range entries {
		if wireString(t, entry, "toolId") == toolID && wireString(t, entry, "profileId") == profileID {
			return entry
		}
	}
	t.Fatalf("partialFailures 里没有 %s/%q 的条目（共 %d 条）", toolID, profileID, len(entries))
	return nil
}

// wireBool 报告布尔字段是否**在场**，以及在场时的取值。缺席不是错误——这正是要断言的一半。
func wireBool(t *testing.T, entry map[string]json.RawMessage, key string) (value, present bool) {
	t.Helper()
	raw, ok := entry[key]
	if !ok {
		return false, false
	}
	var out bool
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("字段 %s 不是布尔：%s", key, raw)
	}
	return out, true
}

// wireString 读线上 JSON 里的字符串字段（键缺席按空串）。
func wireString(t *testing.T, entry map[string]json.RawMessage, key string) string {
	t.Helper()
	raw, ok := entry[key]
	if !ok {
		return ""
	}
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("字段 %s 不是字符串：%s", key, raw)
	}
	return out
}

// TestMaterializeByDesignFlagOnTheWire 守住 §4.4 的 `byDesign`：**设计性跳过**（WorkBuddy 走
// 独立入口，`proxyOnlyFamily`）与**真失败**必须能在同一份清单里被机器区分——界面只按此旗标
// 过滤，**禁止**用 `reason` 文案匹配（doc/design/screens.md §2、information-architecture.md #29）。
//
// 断言刻意读**线上 JSON**而不是 struct：契约是两条的一半一半——
//   - 跳过条目带 `byDesign: true`，且照旧留在 `partialFailures` 里（跳过得可见：单元面板
//     对它显示一句专门的说明，不计入任何「未完成」计数与首屏告警）；
//   - 真失败条目**没有**这个键（`omitempty`：键在场就只可能表示 true，按「键是否存在」判定的
//     消费方不会被显式的 false 骗到；缺席 = 计入未完成，失败安全——宁可多报，不可静默丢弃）。
//
// 解进 struct 会让「缺席」与「显式 false」混同，上面第二条就没人守了。
//
// 夹具用「策略缺失」造真失败（不是编造的边角情形）：它是 UC-019 边界与异常的第一条，
// 也让两类条目出现在**同一次**物化、同一份载荷里。
func TestMaterializeByDesignFlagOnTheWire(t *testing.T) {
	srv := newContractServer(t)
	freshPolicyRoot(t, srv) // 一份策略都没见过 ⇒ 受管卡算不出来，登记一条真失败
	entries := wireFailures(t, srv)

	cases := []struct {
		name        string
		toolID      string
		profileID   string
		wantPresent bool // `byDesign` 必须在场（true）还是必须缺席
		wantValue   bool // 在场时它必须是这个值
		wantCounted bool // 界面谓词 `byDesign !== true` 的结果：true = 计入未完成
		wantReason  string
	}{
		{
			name:        "真失败（策略缺失 ⇒ 受管卡算不出来）",
			toolID:      string(family.Claude),
			profileID:   managedCardID,
			wantPresent: false,
			wantValue:   false,
			wantCounted: true,
			wantReason:  "策略",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := wireFailureOf(t, entries, tc.toolID, tc.profileID)
			// reason 仍必须给出：两类条目都要能自称来历（跳过的那条也要有可读的说明）。
			if reason := wireString(t, entry, "reason"); !strings.Contains(reason, tc.wantReason) {
				t.Errorf("%s/%q 的 reason = %q，期望含 %q", tc.toolID, tc.profileID, reason, tc.wantReason)
			}
			value, present := wireBool(t, entry, "byDesign")
			if present != tc.wantPresent {
				t.Fatalf("byDesign 在场 = %v（值 %v），期望在场 = %v：%v", present, value, tc.wantPresent, entry)
			}
			if present && value != tc.wantValue {
				t.Errorf("byDesign = %v，期望 %v", value, tc.wantValue)
			}
			// 界面谓词（§4.4 的语义，不是实现细节）：只有 `byDesign === true` 才不算未完成。
			if counted := !(present && value); counted != tc.wantCounted {
				t.Errorf("按 `byDesign !== true` 过滤：计入未完成 = %v，期望 %v（%v）",
					counted, tc.wantCounted, entry)
			}
		})
	}
}

// ── #27 从未物化过时的 GET ──────────────────────────────────────────────────

// TestMaterializeListBeforeFirstRun 固定「从未物化过」的清单形态：families 照常列出、
// dir 给出预期路径、files 为空数组，且 GET 本身不写盘。
func TestMaterializeListBeforeFirstRun(t *testing.T) {
	srv := newContractServer(t)
	payload := materializeGet(t, srv)

	root := filepath.Join(srv.env.Home, materializeDirName, materializeLeafName)
	if payload.Root != root {
		t.Fatalf("物化根 = %q，期望 %q", payload.Root, root)
	}
	mustNotExist(t, root, "GET 是只读盘点，不该写盘")
	if len(payload.Families) == 0 {
		t.Fatalf("从未物化时也必须列出单元（前端据此显示「尚未物化」）")
	}
	for _, entry := range payload.Families {
		if len(entry.Profiles) == 0 {
			t.Errorf("%s 没有列出任何档位卡", entry.ToolID)
		}
		for _, profile := range entry.Profiles {
			if profile.Dir == "" {
				t.Errorf("%s/%s 没有给出预期目录", entry.ToolID, profile.ProfileID)
			}
			if len(profile.Files) != 0 {
				t.Errorf("%s/%s 尚未物化却报告了文件：%v", entry.ToolID, profile.ProfileID, profile.Files)
			}
		}
	}
	// 启动命令仍然可用（它只依赖目录与计划输入）：claude 的 route 键取自计划字节。
	claude := profileOf(t, familyOf(t, payload, family.Claude), managedCardID)
	if !strings.HasPrefix(claude.LaunchCommand, "CLAUDE_CONFIG_DIR="+claude.Dir) {
		t.Errorf("尚未物化时 claude 的命令没有指向预期目录：%q", claude.LaunchCommand)
	}
}

// ── UC-094 A11：containsCredentials 指示面（真值钉） ────────────────────────

// readMaterializedProfileBytes 把一张卡物化目录里的全部文件字节拼起来（盘上事实）。
func readMaterializedProfileBytes(t *testing.T, p materializeProfile) string {
	t.Helper()
	var b strings.Builder
	for _, name := range p.Files {
		b.Write(mustReadFile(t, filepath.Join(p.Dir, filepath.FromSlash(name))))
	}
	return b.String()
}

// TestMaterializeCredentialIndicatorMatchesReality 是 UC-094 A11 指示面的**真值钉**：
// 清单的 `containsCredentials` 必须与**盘上字节**一致——
//   - 说含凭据的卡，目录里必须真能找到**它自己的**那份凭据（托管卡 = 本机推理令牌
//     = 夹具的 TokenSeed 哨兵；自建卡指向自定义端点 = 卡自有凭据）；
//   - 说不含凭据的卡，目录里不得出现任何真实凭据（哨兵与自有秘密都不得出现）。
//
// 期望表在测试里**独立写出**（不调用被测函数推导）：某天实现「顺手统一」六个族的名
// 单（或多算/少算一族），这张表会指名报出是哪张卡。
func TestMaterializeCredentialIndicatorMatchesReality(t *testing.T) {
	srv := newContractServer(t)
	sentinel := "contract-test-inference-token" // = newContractServer 的 TokenSeed

	// 自建卡两类（pi）：
	//   ① 网关形态（不给端点）⇒ 凭据位 = 本机推理令牌（哨兵）；
	//   ② 自有端点 + 自有凭据 ⇒ 凭据位 = 卡自有秘密（且**不含**本机令牌——替换语义）。
	gatewayCard := createUserCard(t, srv, family.Pi, "指示器-网关卡")
	const ownSecret = "sk-indicator-own-credential-0123456789abcdef"
	credRes := srv.callRaw(t, http.MethodPost, "/api/v1/credentials",
		`{"name":"指示器-自有凭据","provider":"vendor","cred":"`+ownSecret+`"}`, true)
	if !credRes.ok {
		t.Fatalf("录入自有凭据失败: %s", credRes.body)
	}
	var cred credentialView
	if err := json.Unmarshal(credRes.data, &cred); err != nil {
		t.Fatalf("解析凭据响应失败: %v", err)
	}
	ownRes := srv.callRaw(t, http.MethodPost, "/api/v1/tools/pi/profiles",
		`{"name":"指示器-自有端点卡","endpoint":"https://vendor.example/v1","credRef":"`+cred.Ref+`","models":["deepseek-flash"]}`, true)
	if !ownRes.ok {
		t.Fatalf("建自有端点卡失败: %s", ownRes.body)
	}
	var ownView struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(ownRes.data, &ownView); err != nil || ownView.ID == "" {
		t.Fatalf("建卡响应里没有 id: %v（%s）", err, ownRes.body)
	}

	// 受管网关基址 = 装配面的本地回环（回环即受管形态，openclaw 写后自检通过），
	// 无需再覆盖 origin——六个内嵌令牌族都能走到「字节落盘」这一步。
	payload := materializePost(t, srv)

	cases := []struct {
		tool     family.ID
		pid      string
		contains bool
		mustHave string   // contains=true：目录里必须出现的凭据字节
		mustNot  []string // 目录里不得出现的字节（补充禁止项）
	}{
		// 占位符 / 自发密钥 / 不含凭据的三家与各 router/官方档：目录里不得有真实凭据。
		{family.Claude, managedCardID, false, "", nil},
		{family.Claude, OfficialProfileID, false, "", nil},
		{family.Claude, RouterProfileID, false, "", nil},
		{family.ClaudeDesktop, managedCardID, false, "", nil},
		{family.ClaudeDesktop, OfficialProfileID, false, "", nil},
		{family.ClaudeDesktop, RouterProfileID, false, "", nil},
		{family.Codex, managedCardID, false, "", nil},
		{family.Codex, OfficialProfileID, false, "", nil},
		{family.Codex, RouterProfileID, false, "", nil},
		// 六个内嵌令牌族的托管卡：含真实凭据（= 本机推理令牌 = 哨兵）。
		{family.Pi, managedCardID, true, sentinel, nil},
		{family.OpenCode, managedCardID, true, sentinel, nil},
		{family.OpenClaw, managedCardID, true, sentinel, nil},
		{family.Hermes, managedCardID, true, sentinel, nil},
		{family.Gemini, managedCardID, true, sentinel, nil},
		{family.Grokbuild, managedCardID, true, sentinel, nil},
		// 官方档（移除托管条目）/ router 形态（凭据位恒为占位符）⇒ 不含。
		// pi / opencode 是 additive 族（没有官方形态、就没有官方卡）。
		{family.Pi, RouterProfileID, false, "", nil},
		{family.OpenCode, RouterProfileID, false, "", nil},
		{family.Gemini, OfficialProfileID, false, "", nil},
		{family.Grokbuild, OfficialProfileID, false, "", nil},
		// 自建卡：网关形态含本机令牌；自有端点卡含自有凭据、且不含本机令牌。
		{family.Pi, gatewayCard, true, sentinel, nil},
		{family.Pi, ownView.ID, true, ownSecret, []string{sentinel}},
	}
	for _, tc := range cases {
		name := string(tc.tool) + "/" + tc.pid
		// Keep runner identities deterministic; the saved card IDs are random
		// by design and remain the lookup keys inside each subtest.
		switch tc.pid {
		case gatewayCard:
			name = string(tc.tool) + "/user-gateway-card"
		case ownView.ID:
			name = string(tc.tool) + "/user-created-endpoint-card"
		}
		t.Run(name, func(t *testing.T) {
			p := profileOf(t, familyOf(t, payload, tc.tool), tc.pid)
			if p.ContainsCredentials != tc.contains {
				t.Errorf("containsCredentials = %v，期望 %v", p.ContainsCredentials, tc.contains)
			}
			content := readMaterializedProfileBytes(t, p)
			if tc.contains {
				if !strings.Contains(content, tc.mustHave) {
					t.Errorf("清单说含凭据，但目录里找不到该凭据字节（%d 个文件）", len(p.Files))
				}
				for _, forbidden := range tc.mustNot {
					if strings.Contains(content, forbidden) {
						t.Errorf("目录里出现了不该出现的凭据字节: %q", forbidden)
					}
				}
				return
			}
			if strings.Contains(content, sentinel) {
				t.Errorf("清单说不含凭据，但目录里出现了本机令牌（哨兵）")
			}
			if strings.Contains(content, ownSecret) {
				t.Errorf("清单说不含凭据，但目录里出现了自有凭据")
			}
		})
	}
}

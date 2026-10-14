package policy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// policyFixture 是「本地策略文件 + 策略仓库」的固定装置。
//
// 源已切换为本地文件（design §2.3）：seed 把策略载荷写成策略仓库的盘上形态
// （经 Store.persist，与播种/持久化同一写盘纪律），Refresh 即重读该文件——
// 测试全程零网络请求。
type policyFixture struct {
	store *Store
	path  string
}

func newPolicyFixture(t *testing.T, policyPath string) *policyFixture {
	t.Helper()
	now := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	return &policyFixture{
		path:  policyPath,
		store: NewStore(policyPath, Options{Clock: func() time.Time { return now }}),
	}
}

// seed 把策略载荷写进策略文件（磁盘形态 = {schema,origin,fetchedAt,payload}）。
func (f *policyFixture) seed(t *testing.T, payload string) {
	t.Helper()
	if err := f.store.persist([]byte(payload), &Policy{
		Origin: "", FetchedAt: f.store.clock().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("写入策略文件失败: %v", err)
	}
}

// catalogPayload 是带「不规则空白与键序」的目录载荷：逐字节契约就是拿它判定的。
const catalogPayload = `{"models":[{"slug":"gpt-5.6-sol",  "display_name":"gpt-5.6-sol",` + "\n" +
	`   "context_window":1000000},{"slug":"gpt-5.1-codex"}],"data_source":"gateway"}`

// legalPolicy 生成一份合法策略。defaultIsFirst 控制 default 标记落在哪个条目上。
func legalPolicy(version int, defaultIsFirst bool) string {
	deepseek := `{"id":"deepseek-flash","name":"DeepSeek Flash","isDefault":true,` +
		`"maxInputTokens":1000000,"maxOutputTokens":384000,"supportsReasoning":true,"supportsImages":true}`
	glm := `{"id":"bd-glm-5.3-flash","name":"GLM 5.3 Flash","isDefault":false,"supportsImages":false}`
	models := deepseek + `,` + glm
	if !defaultIsFirst {
		models = glm + `,` + deepseek
	}
	return `{"version":` + strconv.Itoa(version) + `,"reason":"例行更新","updatedAt":"2026-09-20T00:00:00Z",` +
		`"models":[` + models + `],"catalogs":{"codex":` + catalogPayload + `,"anthropic":` + catalogPayload + `}}`
}

// TestManagedModelsIsExactlyPolicyDefault：受管模型集合恒为单元素 = 策略 default。
func TestManagedModelsIsExactlyPolicyDefault(t *testing.T) {
	cases := []struct {
		name           string
		version        int
		defaultIsFirst bool
		wantImages     bool
	}{
		{name: "default 在前", version: 3, defaultIsFirst: true, wantImages: true},
		{name: "default 在后", version: 4, defaultIsFirst: false, wantImages: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPolicyFixture(t, filepath.Join(t.TempDir(), "policy.json"))
			fixture.seed(t, legalPolicy(tc.version, tc.defaultIsFirst))
			policy, err := fixture.store.Refresh(context.Background())
			if err != nil {
				t.Fatalf("拉取策略失败: %v", err)
			}

			managed := policy.ManagedModels()
			if len(managed) != 1 {
				t.Fatalf("受管集合大小 = %d，期望 1（%+v）", len(managed), managed)
			}
			if managed[0].ID != policy.DefaultModel || managed[0].ID != "deepseek-flash" {
				t.Fatalf("受管模型 = %q，default = %q", managed[0].ID, policy.DefaultModel)
			}
			if managed[0].SupportsImages != tc.wantImages {
				t.Fatalf("受管模型的 supportsImages 未透传: %+v", managed[0])
			}
			if len(policy.Models) != 2 {
				t.Fatalf("策略模型数 = %d，期望 2（非 default 也在策略里，但不受管）", len(policy.Models))
			}
			spec, ok := policy.Spec("deepseek-flash")
			if !ok || spec.MaxOutputTokens != 384000 || !spec.SupportsReasoning {
				t.Fatalf("条目全文未解析: %+v", spec)
			}
		})
	}
}

// TestManagedModelsFallsBackToFirstModel：策略不带 isDefault 标记时沿用「首个模型」语义。
func TestManagedModelsFallsBackToFirstModel(t *testing.T) {
	fixture := newPolicyFixture(t, filepath.Join(t.TempDir(), "policy.json"))
	fixture.seed(t, `{"version":1,"models":["m-a","m-b"]}`)
	policy, err := fixture.store.Refresh(context.Background())
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	managed := policy.ManagedModels()
	if len(managed) != 1 || managed[0].ID != "m-a" {
		t.Fatalf("无标记时的受管集合 = %+v", managed)
	}
	spec, ok := policy.Spec("m-b")
	if !ok || spec.MaxInputTokens != defaultMaxInputTokens || spec.MaxOutputTokens != defaultMaxOutputTokens {
		t.Fatalf("字符串条目的窗口默认值不对: %+v", spec)
	}
}

// TestCatalogsSurvivePersistenceByteForByte：catalogs 的值是不透明字节，只搬运不重编码——
// 跨落盘与重载也必须逐字节等同（契约 §7 的逐字节契约前提）。
func TestCatalogsSurvivePersistenceByteForByte(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	fixture := newPolicyFixture(t, path)
	fixture.seed(t, legalPolicy(5, true))

	policy, err := fixture.store.Refresh(context.Background())
	if err != nil {
		t.Fatalf("读取策略失败: %v", err)
	}
	raw, ok := policy.Catalog("codex")
	if !ok {
		t.Fatal("codex 目录未命中")
	}
	if string(raw) != catalogPayload {
		t.Fatalf("目录字节被改写:\n拿到 %s\n期望 %s", raw, catalogPayload)
	}

	// 换一个实例只读盘（模拟冷启动）：字节必须一模一样。
	loaded, err := NewStore(path, Options{}).Load()
	if err != nil {
		t.Fatalf("离线加载失败: %v", err)
	}
	reloaded, ok := loaded.Catalog("codex")
	if !ok {
		t.Fatal("离线加载后目录未命中")
	}
	if string(reloaded) != catalogPayload {
		t.Fatalf("重载后目录字节被改写:\n拿到 %s\n期望 %s", reloaded, catalogPayload)
	}
	if loaded.Version != 5 || loaded.DefaultModel != "deepseek-flash" || loaded.FetchedAt == "" {
		t.Fatalf("离线加载的信息不完整: %+v", loaded)
	}
	if len(loaded.Models) != 2 {
		t.Fatalf("离线加载的模型数 = %d", len(loaded.Models))
	}
}

// TestCatalogMissLeavesFallbackPathIntact：未命中（无键，或值为空白/null）必须明确报「没有」，
// 让调用方回落既有行为，而不是拿到一个空对象当成命中。
func TestCatalogMissLeavesFallbackPathIntact(t *testing.T) {
	fixture := newPolicyFixture(t, filepath.Join(t.TempDir(), "policy.json"))
	fixture.seed(t, `{"version":1,"models":["m-a"],"catalogs":{"openai":"   ","pi":null}}`)
	policy, err := fixture.store.Refresh(context.Background())
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	for _, prefix := range []string{"openai", "pi", "codex"} {
		if _, ok := policy.Catalog(prefix); ok {
			t.Fatalf("前缀 %q 不应命中", prefix)
		}
	}
}

// TestPolicyRefreshFallsBackToLastLegalPolicy：文件坏掉时退回上一份可用策略且不打扰用户，
// 磁盘上的合法策略不被覆盖（Refresh 只读不写）。
func TestPolicyRefreshFallsBackToLastLegalPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	fixture := newPolicyFixture(t, path)
	fixture.seed(t, legalPolicy(7, true))

	first, err := fixture.store.Refresh(context.Background())
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}

	// 文件被写坏：仍返回上一份，且不报错。
	if err := os.WriteFile(path, []byte(`<html>down</html>`), 0o600); err != nil {
		t.Fatalf("写坏策略文件失败: %v", err)
	}
	second, err := fixture.store.Refresh(context.Background())
	if err != nil {
		t.Fatalf("有兜底策略时不应报错: %v", err)
	}
	if second.Version != first.Version || len(second.Models) != len(first.Models) {
		t.Fatalf("兜底策略内容不对: %+v", second)
	}
	if _, ok := second.Catalog("codex"); !ok {
		t.Fatal("兜底策略丢了目录")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取策略文件失败: %v", err)
	}
	if !strings.Contains(string(after), "html") {
		t.Fatalf("兜底时磁盘被覆盖: %s", after)
	}
}

// TestPolicyRefreshFallsBackWhenPayloadBecomesInvalid：文件可读但策略非法时同样退回上一份，
// 且**不**改内存（非法内容不得挤出合法内容）。
func TestPolicyRefreshFallsBackWhenPayloadBecomesInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	fixture := newPolicyFixture(t, path)
	fixture.seed(t, legalPolicy(7, true))

	if _, err := fixture.store.Refresh(context.Background()); err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	fixture.seed(t, `{"version":8}`) // 有版本但没模型：非法
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取策略文件失败: %v", err)
	}
	got, err := fixture.store.Refresh(context.Background())
	if err != nil {
		t.Fatalf("有兜底策略时不应报错: %v", err)
	}
	if got.Version != 7 {
		t.Fatalf("非法策略被采纳: %+v", got)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取策略文件失败: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("Refresh 改了盘")
	}
}

// TestPolicyRefreshFailsOnlyWithoutAnyLegalPolicy：从未见过合法策略时才失败。
// 本地文件源下没有「网络不可达」分支（源即文件）：文件缺失与结构非法同码
// gateway_unexpected_response（契约码保留，措辞点明本地文件）。
func TestPolicyRefreshFailsOnlyWithoutAnyLegalPolicy(t *testing.T) {
	missing := newPolicyFixture(t, filepath.Join(t.TempDir(), "policy.json"))
	if _, err := missing.store.Refresh(context.Background()); !errors.Is(err, apierr.New(apierr.CodeGatewayUnexpectedResponse, "")) {
		t.Fatalf("期望 gateway_unexpected_response（文件缺席），实际 %v", err)
	}

	invalid := newPolicyFixture(t, filepath.Join(t.TempDir(), "policy.json"))
	invalid.seed(t, `{"version":1}`)
	if _, err := invalid.store.Refresh(context.Background()); !errors.Is(err, apierr.New(apierr.CodeGatewayUnexpectedResponse, "")) {
		t.Fatalf("期望 gateway_unexpected_response（结构非法），实际 %v", err)
	}
}

// TestPolicyRefreshAdoptsCurrentFileContent：源即文件——Refresh 采纳盘上当前内容，
// 版本回退是编辑者在文件上的显式动作（原「版本单调拦截」保护的是网关侧回滚，
// 网关已随旧中继退役，语义改写有 design §2.3 依据）。
func TestPolicyRefreshAdoptsCurrentFileContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	fixture := newPolicyFixture(t, path)

	fixture.seed(t, legalPolicy(9, true))
	current, err := fixture.store.Refresh(context.Background())
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if current.Version != 9 {
		t.Fatalf("首读版本 = %d，期望 9", current.Version)
	}

	for _, next := range []int{8, 1} {
		fixture.seed(t, legalPolicy(next, true))
		got, err := fixture.store.Refresh(context.Background())
		if err != nil {
			t.Fatalf("版本 %d 的重读不应报错: %v", next, err)
		}
		if got.Version != next {
			t.Fatalf("盘上版本 %d 未被采纳: %d", next, got.Version)
		}
	}
}

// TestPolicyRejectsInvalidPayloads：非法策略整体拒绝，绝不采纳半份策略。
func TestPolicyRejectsInvalidPayloads(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "缺版本", body: `{"models":["m-a"]}`},
		{name: "版本为 0", body: `{"version":0,"models":["m-a"]}`},
		{name: "版本非数字", body: `{"version":"oops","models":["m-a"]}`},
		{name: "模型为空", body: `{"version":2,"models":[]}`},
		{name: "模型全是空条目", body: `{"version":2,"models":[""]}`},
		{name: "不是对象", body: `["not-a-policy"]`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPolicyFixture(t, filepath.Join(t.TempDir(), "policy.json"))
			fixture.seed(t, tc.body)
			_, err := fixture.store.Refresh(context.Background())
			if !errors.Is(err, apierr.New(apierr.CodeGatewayUnexpectedResponse, "")) {
				t.Fatalf("期望 gateway_unexpected_response，实际 %v", err)
			}
			if policy, err := fixture.store.Load(); err != nil || policy != nil {
				t.Fatalf("非法策略不得留下任何状态: %+v %v", policy, err)
			}
		})
	}
}

// TestPolicyRefreshReadsLocalFile：策略源是本地文件——Refresh 重读 path 指向的文件，
// 文件内容变化即被采纳（替代原「匿名拉取」用例：网关客户端已随登录链删除，
// 出网路径在类型层面不存在）。
func TestPolicyRefreshReadsLocalFile(t *testing.T) {
	fixture := newPolicyFixture(t, filepath.Join(t.TempDir(), "policy.json"))
	fixture.seed(t, legalPolicy(1, true))
	first, err := fixture.store.Refresh(context.Background())
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if first.Version != 1 {
		t.Fatalf("首读版本 = %d，期望 1", first.Version)
	}

	fixture.seed(t, legalPolicy(2, true))
	second, err := fixture.store.Refresh(context.Background())
	if err != nil {
		t.Fatalf("重读失败: %v", err)
	}
	if second.Version != 2 {
		t.Fatalf("重读未采纳盘上新内容: 版本 = %d，期望 2", second.Version)
	}
}

// TestPolicyFileIsPrivateMode：策略文件写盘仍按 0600（与凭据同一纪律）。
func TestPolicyFileIsPrivateMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	fixture := newPolicyFixture(t, path)
	fixture.seed(t, legalPolicy(2, true))
	if _, err := fixture.store.Refresh(context.Background()); err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("读取策略文件失败: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("策略文件权限 = %o，期望 600", perm)
	}
}

// TestPolicyJSONShapeMatchesContract：/policy 响应体的字段与契约一致（多一个都不行）。
func TestPolicyJSONShapeMatchesContract(t *testing.T) {
	fixture := newPolicyFixture(t, filepath.Join(t.TempDir(), "policy.json"))
	fixture.seed(t, legalPolicy(3, true))
	policy, err := fixture.store.Refresh(context.Background())
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	want := []string{"version", "defaultModel", "models", "catalogs"}
	if len(decoded) != len(want) {
		t.Fatalf("响应字段集 = %v，期望 %v", keysOf(decoded), want)
	}
	for _, key := range want {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("缺少字段 %q: %v", key, keysOf(decoded))
		}
	}

	var models []map[string]json.RawMessage
	if err := json.Unmarshal(decoded["models"], &models); err != nil {
		t.Fatalf("models 结构非法: %v", err)
	}
	if len(models) == 0 || len(models[0]) != 3 {
		t.Fatalf("models[0] 字段集 = %v，期望 id/name/supportsImages", keysOf(models[0]))
	}
	var catalogs map[string]json.RawMessage
	if err := json.Unmarshal(decoded["catalogs"], &catalogs); err != nil {
		t.Fatalf("catalogs 结构非法: %v", err)
	}
	if _, ok := catalogs["codex"]; !ok {
		t.Fatalf("catalogs 缺 codex: %v", keysOf(catalogs))
	}
}

func keysOf[V any](in map[string]V) []string {
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	return keys
}

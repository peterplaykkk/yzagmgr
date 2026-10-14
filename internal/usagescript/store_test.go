package usagescript

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
)

// 夹具：数据根走 config.Env（与生产同一条路径解析），但仍然完全落在 t.TempDir() 下。
func newDataEnv(t *testing.T) *config.Env {
	t.Helper()
	env := &config.Env{Home: t.TempDir()}
	if err := env.EnsureLayout(); err != nil {
		t.Fatalf("建立数据根骨架失败: %v", err)
	}
	return env
}

func treeDigest(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			out[rel] = "dir"
			return nil
		}
		blob, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(blob)
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		out[rel] = hex.EncodeToString(sum[:]) + ":" + info.Mode().Perm().String()
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 %s 失败: %v", root, err)
	}
	return out
}

func diffDigests(before, after map[string]string) string {
	var sb strings.Builder
	for k, v := range after {
		if got, ok := before[k]; !ok {
			fmt.Fprintf(&sb, "\n  新增: %s (%s)", k, v)
		} else if got != v {
			fmt.Fprintf(&sb, "\n  变化: %s (%s → %s)", k, got, v)
		}
	}
	for k, v := range before {
		if _, ok := after[k]; !ok {
			fmt.Fprintf(&sb, "\n  消失: %s (%s)", k, v)
		}
	}
	return sb.String()
}

// ── UC-016 A14 ──────────────────────────────────────────────────────────────

func TestUsageScriptIsStoredAndPortable(t *testing.T) {
	env := newDataEnv(t)
	st := NewStore(env.UsageScriptsDir(), nil)
	if st.Dir() != env.UsageScriptsDir() {
		t.Fatalf("存储目录=%s，期望 %s", st.Dir(), env.UsageScriptsDir())
	}
	script := `({ request: { method: "GET", url: "https://provider.example/usage" }, extract: { usedPercent: "$.n" } })`

	put, err := st.Put("claude", "p_third_party", script)
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if put.Script != script || put.UpdatedAt == "" {
		t.Fatalf("写入回读形状不符: %+v", put)
	}
	if _, err := time.Parse(time.RFC3339, put.UpdatedAt); err != nil {
		t.Fatalf("updatedAt 不是 RFC3339: %q", put.UpdatedAt)
	}

	// 落盘位置与权限：<data root>/usage-scripts/<tool>__<profile>.json，0600。
	file := filepath.Join(env.UsageScriptsDir(), "claude__p_third_party.json")
	st1, err := os.Stat(file)
	if err != nil {
		t.Fatalf("脚本文件不存在: %v", err)
	}
	if st1.Mode().Perm() != 0o600 {
		t.Fatalf("脚本文件权限=%v，期望 0600", st1.Mode().Perm())
	}
	if dirSt, err := os.Stat(env.UsageScriptsDir()); err != nil || dirSt.Mode().Perm() != 0o700 {
		t.Fatalf("脚本目录权限不符: %v %v", dirSt, err)
	}

	// 回读一致。
	got, ok, err := st.Get("claude", "p_third_party")
	if err != nil || !ok {
		t.Fatalf("回读失败: ok=%v err=%v", ok, err)
	}
	if got.Script != script || got.UpdatedAt != put.UpdatedAt {
		t.Fatalf("回读不一致: %+v vs %+v", got, put)
	}
	// 不存在的档：ok=false 且不报错。
	if _, ok, err := st.Get("claude", "p_none"); ok || err != nil {
		t.Fatalf("缺脚本应 ok=false/err=nil，实际 ok=%v err=%v", ok, err)
	}

	// 导出 → 另一个数据根里导入 → 内容一致（「随该档导出/导入」）。
	blob, err := st.Export("claude", "p_third_party")
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(blob, &raw); err != nil {
		t.Fatalf("导出内容不是 JSON: %v", err)
	}
	for _, key := range []string{"format", "toolId", "profileId", "script", "updatedAt"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("导出内容缺少自证字段 %q: %s", key, blob)
		}
	}

	env2 := newDataEnv(t)
	st2 := NewStore(env2.UsageScriptsDir(), nil)
	ref, err := st2.Import(blob)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if ref.ToolID != "claude" || ref.ProfileID != "p_third_party" {
		t.Fatalf("导入归属不符: %+v", ref)
	}
	got2, ok, err := st2.Get("claude", "p_third_party")
	if err != nil || !ok {
		t.Fatalf("导入后回读失败: ok=%v err=%v", ok, err)
	}
	if got2.Script != script {
		t.Fatalf("导入后脚本不一致: %+v", got2)
	}

	// 删除幂等。
	if deleted, err := st.Delete("claude", "p_third_party"); err != nil || !deleted {
		t.Fatalf("首次删除应成功: %v %v", deleted, err)
	}
	if deleted, err := st.Delete("claude", "p_third_party"); err != nil || deleted {
		t.Fatalf("再次删除应幂等成功且不报错: %v %v", deleted, err)
	}
}

// 存储键进文件名：绝不允许目录穿越。
func TestUsageScriptStoreRejectsUnsafeKeys(t *testing.T) {
	env := newDataEnv(t)
	st := NewStore(env.UsageScriptsDir(), nil)
	for _, bad := range []string{"../evil", "a/b", "..", ".hidden", "", "a b", strings.Repeat("x", 65)} {
		if _, err := st.Put("claude", bad, "s"); err == nil {
			t.Fatalf("档位 id %q 必须被拒", bad)
		} else if ae := apierr.From(err); ae.Code != apierr.CodeInvalidRequest {
			t.Fatalf("档位 id %q 的错误码=%s，期望 invalid_request", bad, ae.Code)
		}
		if _, err := st.Put(bad, "p", "s"); err == nil {
			t.Fatalf("工具 id %q 必须被拒", bad)
		}
	}
	// 导入同样过键校验。
	if _, err := st.Import([]byte(`{"format":"` + storeFormat + `","toolId":"../x","profileId":"p","script":"s"}`)); err == nil {
		t.Fatalf("带非法归属的导入必须被拒")
	}
	// 格式标识不符 → 拒绝。
	if _, err := st.Import([]byte(`{"format":"other/9","toolId":"claude","profileId":"p","script":"s"}`)); err == nil {
		t.Fatalf("未知格式标识必须被拒")
	}
}

// ── UC-016 A12 ──────────────────────────────────────────────────────────────

func TestAbsentScriptShowsNoQuotaNotZero(t *testing.T) {
	env := newDataEnv(t)
	st := NewStore(env.UsageScriptsDir(), nil)
	ff := &fakeFetcher{resp: Response{Status: 200, Body: `{"n":0}`}}
	r := NewRunner(RunnerOptions{Fetcher: ff})

	res, err := r.FetchQuotaFor(st, "claude", "p_without_script", Vars{}, testPolicy())
	if err != nil {
		t.Fatalf("取数返回错误: %v", err)
	}
	if res.Quota != nil {
		t.Fatalf("没有脚本时不得给出额度（哪怕是 0）：%+v", res.Quota)
	}
	if res.Error != nil {
		t.Fatalf("缺脚本不是错误：%+v", res.Error)
	}
	if res.Source != SourceScript {
		t.Fatalf("source=%q，期望 %q", res.Source, SourceScript)
	}
	if len(ff.calls) != 0 {
		t.Fatalf("没有脚本就不该出网，实际代发 %d 次", len(ff.calls))
	}

	// 序列化后必须是 quota:null，而不是一份 usedPercent=0 的额度。
	blob, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if !strings.Contains(string(blob), `"quota":null`) {
		t.Fatalf("响应形状不符（应 quota:null）: %s", blob)
	}
	if strings.Contains(string(blob), "usedPercent") {
		t.Fatalf("不得出现任何额度字段: %s", blob)
	}

	// 取数失败也一样：quota 为 null，不写 0（UC-016 边界条）。
	bad := `({ request: { method: "GET", url: "https://169.254.169.254/x" }, extract: { usedPercent: 1 } })`
	res2, err := r.Usage(bad, Vars{}, testPolicy())
	if err != nil {
		t.Fatalf("取数返回错误: %v", err)
	}
	if res2.Quota != nil || res2.Error == nil {
		t.Fatalf("失败时必须是 quota=null + error：%+v", res2)
	}
}

// ── UC-016 A11 ──────────────────────────────────────────────────────────────

func TestScriptSourcedQuotaIsLabelled(t *testing.T) {
	// 两个来源标签必须不同，界面才能分别标注。
	if SourceScript != "script" || SourceGateway != "gateway" || SourceScript == SourceGateway {
		t.Fatalf("来源标签不符: script=%q gateway=%q", SourceScript, SourceGateway)
	}

	ff := &fakeFetcher{resp: Response{Status: 200, Body: `{"data":{"used":42.5,"reset":1893456000}}`}}
	r := NewRunner(RunnerOptions{Fetcher: ff})
	script := `({
	  request: { method: "GET", url: "https://provider.example/usage" },
	  extract: { usedPercent: "$.data.used", name: "5h", resetsAt: "$.data.reset" }
	})`
	res, err := r.Usage(script, Vars{}, testPolicy())
	if err != nil {
		t.Fatalf("取数返回错误: %v", err)
	}
	if res.Error != nil {
		t.Fatalf("取数失败: %+v", res.Error)
	}
	if res.Source != SourceScript {
		t.Fatalf("取数结果 source=%q，期望 %q", res.Source, SourceScript)
	}
	if res.Quota == nil {
		t.Fatalf("应给出额度")
	}
	if res.Quota.Source != SourceScript {
		t.Fatalf("额度窗口 source=%q，期望 %q（与网关自报区分）", res.Quota.Source, SourceScript)
	}
	if res.Quota.Name != "5h" || res.Quota.UsedPercent != 42.5 {
		t.Fatalf("额度字段不符: %+v", res.Quota)
	}
	if res.Quota.ResetsAt == nil || *res.Quota.ResetsAt != "2030-01-01T00:00:00Z" {
		t.Fatalf("resetsAt 应归一为 RFC3339: %v", res.Quota.ResetsAt)
	}

	// 失败面：不给额度、不写 0。
	bad := `({
	  request: { method: "GET", url: "https://provider.example/usage" },
	  extract: { usedPercent: "$.data.missing" }
	})`
	resBad, err := r.Usage(bad, Vars{}, testPolicy())
	if err != nil {
		t.Fatalf("取数返回错误: %v", err)
	}
	if resBad.Quota != nil {
		t.Fatalf("取不到额度时不得给出额度: %+v", resBad.Quota)
	}
	if resBad.Error == nil || resBad.Error.Code != apierr.CodeInvalidScriptShape {
		t.Fatalf("应给出 invalid_script_shape: %+v", resBad.Error)
	}
	if !strings.Contains(resBad.Error.Message, "$.data.missing") {
		t.Fatalf("错误消息应指名取不到的路径: %q", resBad.Error.Message)
	}
}

// ── UC-016 A8 ───────────────────────────────────────────────────────────────

func TestUsageScriptDryRunHasNoSideEffects(t *testing.T) {
	env := newDataEnv(t)
	st := NewStore(env.UsageScriptsDir(), nil)
	ff := &fakeFetcher{resp: Response{Status: 200, Body: `{"n":7}`}}
	r := NewRunner(RunnerOptions{Fetcher: ff})

	before := treeDigest(t, env.DataRoot())
	if len(before) == 0 {
		t.Fatalf("夹具数据根是空的，快照失去意义")
	}

	// 成功的试运行 + 失败的试运行，各跑一次。
	script := `({
	  request: { method: "GET", url: "https://provider.example/usage" },
	  extract: { usedPercent: "$.n", name: "5h" }
	})`
	res, err := r.Test(script, Vars{APIKey: "sk-dry-run-key-123456"}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	mustNoErr(t, res)

	resFail, err := r.Test(`(function(){ throw new Error("boom"); })()`, Vars{}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	if resFail.Error == nil {
		t.Fatalf("抛错脚本必须给出错误面")
	}

	after := treeDigest(t, env.DataRoot())
	if diff := diffDigests(before, after); diff != "" {
		t.Fatalf("试运行动了盘（零持久化被破坏）：%s", diff)
	}
	// 脚本没有被写进去、用量库也没被创建。
	if _, ok, err := st.Get("claude", "p_third_party"); ok || err != nil {
		t.Fatalf("试运行不该写入脚本: ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(env.UsageDBFile()); !os.IsNotExist(err) {
		t.Fatalf("试运行不该创建用量库")
	}
}

// ── 其余边界（自加） ────────────────────────────────────────────────────────

// 代发失败只保留底层原因：url.Error 的文本含整条 URL，可能带注入的令牌。
func TestUsageScriptFetchFailureDoesNotLeakInjectedValues(t *testing.T) {
	const token = "sk-fetch-SECRET-987654321"
	ue := &url.Error{Op: "Get", URL: "https://provider.example/x?key=" + token,
		Err: errors.New("dial tcp: connection refused")}
	if msg := fetchErrMessage(ue); strings.Contains(msg, token) {
		t.Fatalf("代发失败消息泄漏了 URL 里的令牌: %q", msg)
	} else if !strings.Contains(msg, "connection refused") {
		t.Fatalf("代发失败消息应保留底层原因: %q", msg)
	}

	// 端到端：runner 把代发失败落成 gateway_unreachable，且消息里没有令牌。
	ff := &fakeFetcher{err: ue}
	r := NewRunner(RunnerOptions{Fetcher: ff})
	res, err := r.Test(okScript, Vars{}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	ae := mustScriptCode(t, res.Error, apierr.CodeGatewayUnreachable)
	if ae.Stage != StageRequest {
		t.Fatalf("stage=%s，期望 %s", ae.Stage, StageRequest)
	}
	if strings.Contains(ae.Message, token) {
		t.Fatalf("错误面泄漏了令牌: %q", ae.Message)
	}
}

// 真实 HTTP 代发：正文有界、状态透传、跳转目标再校验（防 302 绕过 SSRF）。
func TestUsageScriptHTTPFetcherBoundsAndGuardsRedirects(t *testing.T) {
	body := strings.Repeat("B", 5000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/big":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
		case "/fail":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("nope"))
		case "/redirect":
			http.Redirect(w, r, "https://169.254.169.254/latest/meta-data/", http.StatusFound)
		case "/slow":
			time.Sleep(400 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	h := &HTTPFetcher{MaxBytes: 1024}
	res, err := h.Do(RequestSpec{Method: "GET", URL: srv.URL + "/big"}, nil)
	if err != nil {
		t.Fatalf("代发失败: %v", err)
	}
	if res.Status != 200 || len(res.Body) != 1024 || !res.Truncated {
		t.Fatalf("正文上限未生效: status=%d len=%d truncated=%v", res.Status, len(res.Body), res.Truncated)
	}

	resFail, err := h.Do(RequestSpec{Method: "GET", URL: srv.URL + "/fail"}, nil)
	if err != nil {
		t.Fatalf("非 2xx 不该是宿主错误（交给脚本处理）: %v", err)
	}
	if resFail.Status != 503 || resFail.Body != "nope" {
		t.Fatalf("状态未透传: %+v", resFail)
	}

	// 跳转目标过黑名单：302 到元数据地址必须被拒。
	_, err = h.Do(RequestSpec{Method: "GET", URL: srv.URL + "/redirect"}, nil)
	if err == nil {
		t.Fatalf("跳到元数据地址必须被拒")
	}
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeBlockedTarget {
		t.Fatalf("跳转拒绝的错误码不符: %v", err)
	}

	// 超时生效。
	slow := &HTTPFetcher{Client: &http.Client{Timeout: 100 * time.Millisecond}}
	if _, err := slow.Do(RequestSpec{Method: "GET", URL: srv.URL + "/slow"}, nil); err == nil {
		t.Fatalf("超时必须报错")
	}
}

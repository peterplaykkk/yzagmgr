package cli

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// TestHumanFormatsCoverDashAndEdgeCases：人类格式化的空值/边界分支。
// 主路径已在 exec 级覆盖；这里补「—」占位、短 sha、空清单这类只在边界出现的分支。
func TestHumanFormatsCoverDashAndEdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		format  func(data []byte) (string, error)
		data    string
		want    []string
		notWant []string
	}{
		{
			// 旧载荷（无 installedState）：人读面回退旧布尔 false → not_installed。
			name:   "tools list 空版本与空 active 显示占位",
			format: func(d []byte) (string, error) { return formatToolsList(d) },
			data:   `[{"id":"pi","name":"Pi","installed":false,"installCopies":0,"version":"","tier":"absent","decideKey":"","paths":[],"activeProfileId":""}]`,
			want:   []string{"pi", "absent", "—", "not_installed"},
		},
		{
			// 旧载荷（无 installedState）：回退旧布尔 true → installed。
			name:   "tools show 空字段与短 sha",
			format: func(d []byte) (string, error) { return formatToolsShow(d) },
			data: `{"tool":{"id":"pi","name":"Pi","installed":true,"installCopies":0,"version":"","tier":"gateway",` +
				`"decideKey":"models.json","paths":[{"path":"/tmp/a","exists":true,"managed":true,"createdByUs":false,` +
				`"sizeBytes":3,"sha256":"ab"}],"activeProfileId":"","companionKeys":[{"name":"k","value":"","masked":true,"order":0}]},` +
				`"profiles":{"items":[{"id":"p1","name":"卡","kind":"user","active":true,"tier":"gateway","modelId":"","editable":true}],` +
				`"total":1,"activeProfileId":""}}`,
			want: []string{"companion", "k=", "*", "p1", "kind=user", "3B", "sha=ab", "installedState\tinstalled"},
		},
		{
			// 旧载荷（无 installedState）：回退旧布尔 false → not_installed。
			name:   "tools show 路径缺席显示 absent",
			format: func(d []byte) (string, error) { return formatToolsShow(d) },
			data: `{"tool":{"id":"pi","name":"Pi","installed":false,"installCopies":0,"version":"1","tier":"absent",` +
				`"decideKey":"k","paths":[{"path":"/tmp/miss","exists":false,"managed":true,"createdByUs":false,"sizeBytes":0,"sha256":""}],` +
				`"activeProfileId":"","companionKeys":[]},"profiles":{"items":[],"total":0,"activeProfileId":""}}`,
			want: []string{"/tmp/miss", "absent", "installedState\tnot_installed"},
		},
		{
			// 新载荷：配置目录在场（旧布尔 true）但无安装证据 ⇒ 人读面报 not_installed，
			// 不再渲染旧布尔 true（报告口径：gemini/grokbuild/openclaw/hermes 上的人读面不一致）。
			name:    "tools list 按 installedState 呈现（配置在场不算安装）",
			format:  func(d []byte) (string, error) { return formatToolsList(d) },
			data:    `[{"id":"gemini","name":"Gemini CLI","installed":true,"installedState":"not_installed","installCopies":0,"version":"","tier":"official","decideKey":"GOOGLE_GEMINI_BASE_URL","paths":[],"activeProfileId":""}]`,
			want:    []string{"gemini", "not_installed"},
			notWant: []string{"true"},
		},
		{
			// unknown 是「不可判」，不得折成未安装，也不得回落旧布尔。
			name:    "tools list installedState=unknown 不折成未安装",
			format:  func(d []byte) (string, error) { return formatToolsList(d) },
			data:    `[{"id":"pi","name":"Pi","installed":false,"installedState":"unknown","installCopies":0,"version":"","tier":"absent","decideKey":"","paths":[],"activeProfileId":""}]`,
			want:    []string{"unknown"},
			notWant: []string{"not_installed", "false"},
		},
		{
			// 新载荷：installedState 优先于旧布尔（installed=true + not_installed ⇒ not_installed）。
			name:    "tools show 按 installedState 呈现（旧布尔 true 被 not_installed 覆盖）",
			format:  func(d []byte) (string, error) { return formatToolsShow(d) },
			data:    `{"tool":{"id":"gemini","name":"Gemini CLI","installed":true,"installedState":"not_installed","installCopies":0,"version":"","tier":"official","decideKey":"k","paths":[],"activeProfileId":"","companionKeys":[]},"profiles":{"items":[],"total":0,"activeProfileId":""}}`,
			want:    []string{"installedState\tnot_installed"},
			notWant: []string{"installed\ttrue"},
		},
		{
			// 新载荷：unknown 原样呈现（不可判不得折成未安装）。
			name:    "tools show installedState=unknown 保持不可判",
			format:  func(d []byte) (string, error) { return formatToolsShow(d) },
			data:    `{"tool":{"id":"pi","name":"Pi","installed":false,"installedState":"unknown","installCopies":0,"version":"","tier":"unknown","decideKey":"k","paths":[],"activeProfileId":"","companionKeys":[]},"profiles":{"items":[],"total":0,"activeProfileId":""}}`,
			want:    []string{"installedState\tunknown"},
			notWant: []string{"not_installed"},
		},
		{
			// 取值不在闭集内（未知扩展/损坏载荷）：与字段缺席同待遇，回退旧布尔。
			name:   "tools show installedState 取值不在闭集回退旧布尔",
			format: func(d []byte) (string, error) { return formatToolsShow(d) },
			data:   `{"tool":{"id":"pi","name":"Pi","installed":true,"installedState":"bogus","installCopies":0,"version":"","tier":"gateway","decideKey":"k","paths":[],"activeProfileId":"","companionKeys":[]},"profiles":{"items":[],"total":0,"activeProfileId":""}}`,
			want:   []string{"installedState\tinstalled"},
		},
		{
			name:   "preview 的 create/delete/warning 分支",
			format: func(d []byte) (string, error) { return formatPreview(d) },
			data: `{"previewId":"pv","expiresAt":"e","operation":"apply","warning":"顶层提示","files":[` +
				`{"path":"/tmp/new","existed":false,"delete":false,"sha256Before":"","bytesBefore":0,"diff":[],"warning":""},` +
				`{"path":"/tmp/old","existed":true,"delete":true,"sha256Before":"","bytesBefore":1,"diff":[],"warning":"接管时创建，恢复后将被移除"}]}`,
			want: []string{"顶层提示", "create", "/tmp/new", "delete", "接管时创建"},
		},
		{
			name:   "restore 带前缀行",
			format: func(d []byte) (string, error) { return formatRestore(d) },
			data: `{"toolId":"pi","backupId":"bk-1-1","apply":{"status":"applied","files":[],` +
				`"backupIds":[],"activation":{"tier":"official","activeProfileId":""}}}`,
			want: []string{"restore", "tool=pi", "backupId=bk-1-1", "status=applied", "official"},
		},
		{
			name:   "backups list 空清单",
			format: func(d []byte) (string, error) { return formatBackupsList(d) },
			data:   `{"items":[],"total":0}`,
			want:   []string{"TOOL", "total", "0"},
		},
		{
			name:   "policy 空目录键",
			format: func(d []byte) (string, error) { return formatPolicy(d) },
			data:   `{"version":1,"defaultModel":"m","models":[{"id":"m","name":"M","supportsImages":false}],"catalogs":{}}`,
			want:   []string{"version", "defaultModel", "models", "m", "catalogs"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.format([]byte(tt.data))
			if err != nil {
				t.Fatalf("格式化失败: %v", err)
			}
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("输出缺 %q\n%s", want, got)
				}
			}
			for _, no := range tt.notWant {
				if strings.Contains(got, no) {
					t.Errorf("输出不应含 %q\n%s", no, got)
				}
			}
		})
	}
}

// TestToolsListInstallStateColumnByRow 逐行钉住人读面的安装状态列：
// 表头为 INSTALL_STATE，行值取自 installedState（不是旧布尔）。
// 口径背景：gemini/grokbuild/openclaw/hermes 在真机只有 v1 遗留配置目录——
// 旧布尔按「配置目录在场即计」会报 true，而 installedState 如实报 not_installed；
// 人读面必须与后者一致（与 Web/API 同口径）。
func TestToolsListInstallStateColumnByRow(t *testing.T) {
	data := `[` +
		`{"id":"claude","name":"Claude Code","installed":true,"installedState":"installed","installCopies":1,"version":"2.1.284","tier":"gateway","decideKey":"","paths":[],"activeProfileId":""},` +
		`{"id":"gemini","name":"Gemini CLI","installed":true,"installedState":"not_installed","installCopies":0,"version":"","tier":"gateway","decideKey":"","paths":[],"activeProfileId":""},` +
		`{"id":"openclaw","name":"OpenClaw","installed":true,"installedState":"unknown","installCopies":0,"version":"","tier":"unknown","decideKey":"","paths":[],"activeProfileId":""}]`
	out, err := formatToolsList([]byte(data))
	if err != nil {
		t.Fatalf("格式化失败: %v", err)
	}
	// 列分隔：tabwriter 的 padchar 是空格、padding=2 ⇒ 列间恒有 ≥2 个空格，
	// 而列内文本（如 "Claude Code"）只有单个空格（CI 语义：按 2+ 空格切列）。
	sep := regexp.MustCompile(`\s{2,}`)
	want := map[string]string{
		"claude":   "installed",
		"gemini":   "not_installed",
		"openclaw": "unknown",
	}
	seen := map[string]bool{}
	for i, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if i == 0 {
			if !strings.Contains(line, "INSTALL_STATE") {
				t.Errorf("表头缺 INSTALL_STATE 列: %q", line)
			}
			continue
		}
		cols := sep.Split(line, -1)
		if len(cols) != 6 {
			t.Errorf("行 %q 切出 %d 列，期望 6（ID NAME TIER INSTALL_STATE VERSION ACTIVE）", line, len(cols))
			continue
		}
		state, ok := want[cols[0]]
		if !ok {
			t.Errorf("意外行: %q", line)
			continue
		}
		seen[cols[0]] = true
		if cols[3] != state {
			t.Errorf("%s 的 INSTALL_STATE 列 = %q，期望 %q（应按 installedState 呈现）", cols[0], cols[3], state)
		}
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("输出缺 %s 行\n%s", id, out)
		}
	}
}

// TestToolsShowProfilesBadPayload：tool 正常而 profiles 坏的分支（组合视图的第二段）。
func TestToolsShowProfilesBadPayload(t *testing.T) {
	data := `{"tool":{"id":"pi","name":"Pi","installed":true,"installCopies":0,"version":"1","tier":"gateway",` +
		`"decideKey":"k","paths":[],"activeProfileId":"","companionKeys":[]},"profiles":"not-an-object"}`
	if out, err := formatToolsShow([]byte(data)); err == nil {
		t.Fatalf("坏 profiles 被格式化成 %q, 期望报错", out)
	}
}

// TestHumanFormatsRejectBadPayload：坏载荷一律报错，不输出假内容。
func TestHumanFormatsRejectBadPayload(t *testing.T) {
	for _, f := range []func(json.RawMessage) (string, error){
		formatHealth, formatToolsList, formatToolsShow, formatPreview,
		formatApply, formatRestore, formatBackupsList, formatPolicy,
	} {
		if out, err := f([]byte(`"x"`)); err == nil {
			t.Errorf("坏载荷被格式化成 %q, 期望报错", out)
		}
	}
}

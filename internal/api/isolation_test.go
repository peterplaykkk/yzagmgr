// 隔离与「不污染用户配置」的两条结构判据（UC-013 A6 无钥匙串、UC-013 A10 无公网源回退）。
//
// 这两条**不是**行为断言，而是「将来有人加回一条通路就会被挡住」的结构判据：
//   - A6：本产品全流程不读写系统钥匙串、不触发系统授权弹窗。判据是**源码里不存在**
//     Security.framework 的调用点与 `security`/`osascript` 这类系统工具调用；
//     等价性说明见用例注释。
//   - A10：依赖获取走公开默认 registry、运行期出网只走受控基址。判据是 registry 指向、Go 模块代理
//     绑定与运行期出网基址三处。
package api

import (
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/proxy"
)

// repoRoot 是本服务的仓库目录（`internal/api` 往上两级）。
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("定位仓库根失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("仓库根不像本服务目录（缺 go.mod）: %v", err)
	}
	return root
}

// runtimeSource 是一份参与扫描的**运行期**源码。
type runtimeSource struct {
	path string
	text string
}

// runtimeSources 收齐本产品运行期会被执行的源码：Go（去掉 `_test.go`）、Swift helper 与
// 仓库内的 shell 脚本。
//
// 排除 `_test.go`：测试代码里的字符串字面量（包括本文件里的这些模式）不是运行期通路，
// 把它们算进来只会让判据自伤。排除 `web/`（前端由它自己的 suite 守，UC-006 A1）
// 与构建产物目录（node_modules/bin/tools/reports/dist）。
func runtimeSources(t *testing.T, root string) []runtimeSource {
	t.Helper()
	skipDirs := map[string]bool{
		"node_modules": true, "bin": true, "tools": true, "reports": true,
		"dist": true, ".git": true, ".e2e-bin": true, "test-results": true,
	}
	out := []runtimeSource{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if skipDirs[entry.Name()] || entry.Name() == "web" {
				return fs.SkipDir
			}
			return nil
		}
		name := entry.Name()
		switch {
		case strings.HasSuffix(name, "_test.go"):
			return nil
		case strings.HasSuffix(name, ".go"), strings.HasSuffix(name, ".swift"),
			strings.HasSuffix(name, ".sh"), name == "Makefile":
		default:
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, path)
		out = append(out, runtimeSource{path: rel, text: string(raw)})
		return nil
	})
	if err != nil {
		t.Fatalf("扫描运行期源码失败: %v", err)
	}
	if len(out) < 20 {
		t.Fatalf("只扫到 %d 份源码，扫描面不成立（路径算错？）", len(out))
	}
	return out
}

// TestNoKeychainAccessAndNoAuthPrompt 守住 UC-013 A6：全流程不读写系统钥匙串、
// 不触发任何系统授权弹窗。
//
// **为什么静态判据等价**：macOS 上「读钥匙串」与「弹授权框」只有三条入口——
// Security.framework 的 API（`SecItem*`/`SecKeychain*`/`kSec*`/`LAContext`）、
// `security` 命令行、以及要求授权的 `osascript`/AppleScript。本产品是纯 Go + 一个
// 不做凭据判断的 Swift 渲染器（UC-011 A3：helper 只渲染与回传），
// 凭据一律落在数据根下的 0600 文件（那条已有 `TestSensitiveFilesAreMode0600` 等断言）。
// 因此「源码里没有这三条入口」等价于「不碰钥匙串、不会弹授权框」——比在测试里真去调一次
// 钥匙串更安全（真调会污染开发机的钥匙串，也可能被弹窗卡住）。
func TestNoKeychainAccessAndNoAuthPrompt(t *testing.T) {
	// 三条入口各自的识别串（大小写敏感：Swift/ObjC 的 API 名有固定写法）。
	forbidden := []string{
		"SecItem",               // Security.framework 的条目读写
		"SecKeychain",           // 旧版钥匙串 API
		"SecAccess",             // 访问控制
		"SecTrustedApplication", // 信任应用列表
		"kSecClass",             // 条目类别常量
		"LAContext",             // LocalAuthentication（Touch ID 授权框）
		"/usr/bin/security",     // security 命令行
		"find-generic-password",
		"find-internet-password",
		"osascript", // 需要「辅助功能/自动化」授权
	}

	sources := runtimeSources(t, repoRoot(t))
	scanned := make([]string, 0, len(sources))
	for _, src := range sources {
		scanned = append(scanned, src.path)
		for _, needle := range forbidden {
			if idx := strings.Index(src.text, needle); idx >= 0 {
				line := 1 + strings.Count(src.text[:idx], "\n")
				t.Errorf("%s:%d 出现钥匙串/授权入口 %q —— UC-013 A6 要求全流程不碰钥匙串",
					src.path, line, needle)
			}
		}
	}
	// 扫描面自检：这几个文件必须在扫描面里，否则「零命中」可能只是因为没扫到。
	//（tray/main.swift 随 tray/ 删除——design §2.6；扫描面收口到 Go/脚本面。）
	for _, must := range []string{"cmd/yzagmgr/main.go"} {
		found := false
		for _, path := range scanned {
			if path == must {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("扫描面缺 %s（判据不成立）", must)
		}
	}

	// 出网与凭据之外的第三处：进程执行的外部命令里不许有关键串工具。
	for _, src := range sources {
		for _, line := range strings.Split(src.text, "\n") {
			if !strings.Contains(line, "exec.Command") && !strings.Contains(line, "Process(") {
				continue
			}
			for _, needle := range forbidden {
				if strings.Contains(line, needle) {
					t.Errorf("%s 执行了外部命令含 %q: %s", src.path, needle, strings.TrimSpace(line))
				}
			}
		}
	}
}

// TestNoUpstreamRegistryFallback 守依赖获取不指向非公开来源：npm 侧依赖链必须落在公开
// 默认 registry 上，运行期出网仍只走受控基址。
//
// 三层（各挡一种回退方式）：
//   - npm 侧：`web/.npmrc` 不得出现 registry 重定向，package-lock.json 的 resolved
//     一律指向 https://registry.npmjs.org/；
//   - Go 侧：仓内任何 GOPROXY/GOSUMDB/GONOSUMDB 绑定都不得指向公网代理（或 off），
//     不得出现 proxy.golang.org / sum.golang.org；
//   - 运行期：出网基址只有两个（分发面与网关），且默认 HTTP 客户端**不跟随** $HTTP_PROXY。
func TestNoUpstreamRegistryFallback(t *testing.T) {
	root := repoRoot(t)

	t.Run("npm 依赖指向公开 registry", func(t *testing.T) {
		if raw, err := os.ReadFile(filepath.Join(root, "web", ".npmrc")); err == nil {
			if strings.Contains(string(raw), "registry=") {
				t.Errorf("web/.npmrc 含 registry 重定向（依赖必须走公开默认 registry）: %s", raw)
			}
		}
		raw, err := os.ReadFile(filepath.Join(root, "web", "package-lock.json"))
		if err != nil {
			t.Fatalf("读 web/package-lock.json 失败: %v", err)
		}
		total := strings.Count(string(raw), `"resolved":`)
		public := strings.Count(string(raw), `"resolved": "https://registry.npmjs.org/`)
		if total == 0 || total != public {
			t.Errorf("package-lock.json 的 resolved 未全部指向 https://registry.npmjs.org/（total=%d public=%d）", total, public)
		}
	})

	t.Run("Go 模块代理不指向公网", func(t *testing.T) {
		var hits []string
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if name := entry.Name(); name == "web" || name == "node_modules" || name == "bin" || name == ".git" {
					return fs.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(entry.Name(), "_test.go") {
				return nil
			}
			switch entry.Name() {
			case "Makefile", "go.mod", "go.sum", ".envrc", ".gitlab-ci.yml":
			default:
				if !strings.HasSuffix(entry.Name(), ".sh") && !strings.HasSuffix(entry.Name(), ".env") {
					return nil
				}
			}
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			for _, line := range strings.Split(string(raw), "\n") {
				if strings.Contains(line, "GOPROXY") || strings.Contains(line, "GOSUMDB") ||
					strings.Contains(line, "GONOSUMDB") || strings.Contains(line, "GOFLAGS") {
					rel, _ := filepath.Rel(root, path)
					hits = append(hits, rel+": "+strings.TrimSpace(line))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("扫描模块代理绑定失败: %v", err)
		}
		for _, hit := range hits {
			for _, upstream := range []string{"proxy.golang.org", "sum.golang.org", "goproxy.io", "goproxy.cn"} {
				if strings.Contains(hit, upstream) {
					t.Errorf("模块代理指向公网源 %q: %s", upstream, hit)
				}
			}
		}
		// 当前状态的登记：仓内没有 Go 侧代理绑定。若将来加了内网代理镜像，
		// 上面那条「不得指向公网」会放行，并把新绑定登记在这里（t.Log 只是信息，不是断言）。
		t.Logf("仓内 GOPROXY/GOSUMDB 绑定 %d 处: %v", len(hits), hits)
	})

	t.Run("运行期出网只走受控基址", func(t *testing.T) {
		// 分发面与旧上游基址常量已随清理删除（design §1.1/§2.4）：运行期
		// 唯一注册的上游是臂官方端点表（§1.3 三家直连矩阵）——逐项核 https 与官方域。
		wantHosts := map[string]bool{
			"open.bigmodel.cn": true, "api.openai.com": true, "api.anthropic.com": true,
		}
		for target, base := range proxy.DefaultArmBases() {
			u, err := url.Parse(base)
			if err != nil || u.Scheme != "https" {
				t.Errorf("臂基址 %v = %q，期望合法 https URL", target, base)
				continue
			}
			if !wantHosts[u.Hostname()] {
				t.Errorf("臂基址 %v 指向非官方域 %q（允许集 = open.bigmodel.cn/api.openai.com/api.anthropic.com）", target, u.Hostname())
			}
		}
		// 默认客户端不跟随 $HTTP_PROXY/$HTTPS_PROXY：跟随环境代理等于把「出网只走受控地址」
		// 交给运行环境决定，而测试机/开发机上这些变量常常指向一个本地抓包代理。
		client := defaultAPIClient()
		transport, ok := client.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("默认客户端的 Transport 形态变了: %T", client.Transport)
		}
		if transport.Proxy != nil {
			t.Errorf("默认客户端跟随了环境代理（Proxy != nil）")
		}
		t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
		if again, ok := defaultAPIClient().Transport.(*http.Transport); !ok || again.Proxy != nil {
			t.Errorf("设置了 HTTPS_PROXY 之后默认客户端跟随了环境代理")
		}
	})
}

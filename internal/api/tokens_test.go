// 管理令牌的跨重启稳定性（UC-006 A9 改写 / design §1.6-F2，E5 拍板）。
//
// 原用例 `TestAdminTokenRotatesPerProcessStart` 断言「每次进程启动轮换、重启即失效」；
// F2 裁定改为**存在即复用**（agserv 对 admin.token 读一次永久缓存，轮换即面板恒 401），
// 冻结断言按 code-test 纪律带理由改写：依据 = inv-8 §1.2/§3-8 + design §1.6-F2 + plan E5。
package api

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
)

// ── UC-006 A9（F2 三分支） ─────────────────────────────────────────────────

// TestAdminTokenStableAcrossRestartsWhenPresent 守住 F2 三分支与「跨重启稳定」：
//
//  1. 文件存在且 0600 + 64hex → 复用同一枚（两次 Ensure 结果逐字相同）；
//  2. 权限过宽（0644）→ chmod 0600 后仍复用同一枚（不换令牌）；
//  3. 内容空/形状错 → 重生成并回报 replaced=true（调用方据此记 Warn）；
//  4. 装配级：同一数据根的两个「进程」取到同一令牌，旧令牌在新进程上继续有效
//     ——这正是「轮换即失效」半句的反面，也是 agserv 永久缓存的前提。
func TestAdminTokenStableAcrossRestartsWhenPresent(t *testing.T) {
	root := t.TempDir()
	t.Setenv(config.EnvHomeVar, root)
	env, err := config.FromProcess()
	if err != nil {
		t.Fatalf("构造隔离环境失败: %v", err)
	}
	if err := env.EnsureLayout(); err != nil {
		t.Fatalf("建立数据根失败: %v", err)
	}

	// 分支 4 前置：首启（文件缺席）生成一枚。
	first, replaced, err := env.EnsureAdminToken()
	if err != nil {
		t.Fatalf("首启生成令牌失败: %v", err)
	}
	if replaced {
		t.Errorf("文件缺席的首启生成不应标记 replaced")
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(first) {
		t.Errorf("令牌形态 = %q，期望 64 位小写 hex", first)
	}
	st, err := os.Stat(env.AdminTokenFile())
	if err != nil {
		t.Fatalf("令牌文件不存在: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("令牌文件权限 = %o，期望 600", st.Mode().Perm())
	}
	if !strings.HasPrefix(env.AdminTokenFile(), env.DataRoot()+string(filepath.Separator)) {
		t.Errorf("令牌文件 %q 不在数据根 %q 下", env.AdminTokenFile(), env.DataRoot())
	}

	// 分支 1：0600 + 合法内容 → 复用同一枚。
	again, replaced, err := env.EnsureAdminToken()
	if err != nil {
		t.Fatalf("复用读取失败: %v", err)
	}
	if replaced || again != first {
		t.Fatalf("分支1（0600 合法内容）必须原样复用: got=%q replaced=%v want=%q", again, replaced, first)
	}

	// 分支 2：权限过宽 → chmod 0600 后仍复用同一枚。
	if err := os.Chmod(env.AdminTokenFile(), 0o644); err != nil {
		t.Fatalf("放宽权限失败: %v", err)
	}
	widened, replaced, err := env.EnsureAdminToken()
	if err != nil {
		t.Fatalf("chmod 分支读取失败: %v", err)
	}
	if replaced || widened != first {
		t.Fatalf("分支2（权限过宽）必须 chmod 后复用同一枚: got=%q replaced=%v", widened, replaced)
	}
	st, err = os.Stat(env.AdminTokenFile())
	if err != nil {
		t.Fatalf("令牌文件不存在: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("分支2 收回后权限 = %o，期望 600", st.Mode().Perm())
	}

	// 分支 3：内容坏 → 重生成 + replaced=true（Warn 由 cmd 侧记录）。
	for _, bad := range []string{"", "not-a-token", strings.Repeat("z", 64)} {
		if err := os.WriteFile(env.AdminTokenFile(), []byte(bad), 0o600); err != nil {
			t.Fatalf("写坏令牌失败: %v", err)
		}
		fixed, replaced, err := env.EnsureAdminToken()
		if err != nil {
			t.Fatalf("坏内容分支失败: %v", err)
		}
		if !replaced {
			t.Fatalf("分支3（内容=%q）必须标记 replaced=true", bad)
		}
		if fixed == first || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(fixed) {
			t.Fatalf("分支3 必须产出新的合法令牌: %q", fixed)
		}
	}

	// 装配级：回到稳定令牌，同一数据根的两个进程取到同一枚。
	if err := env.WriteAdminToken(first); err != nil {
		t.Fatalf("恢复令牌失败: %v", err)
	}
	stable, replaced, err := env.EnsureAdminToken()
	if err != nil || replaced || stable != first {
		t.Fatalf("恢复后的复用失败: token=%q replaced=%v err=%v", stable, replaced, err)
	}

	base := newContractServer(t)
	depsA := base.Deps
	depsA.Token = stable
	procA, err := New(depsA)
	if err != nil {
		t.Fatalf("装配进程 A 失败: %v", err)
	}
	depsB := base.Deps
	depsB.Token = stable
	procB, err := New(depsB)
	if err != nil {
		t.Fatalf("装配进程 B 失败: %v", err)
	}
	if got, _ := env.ReadAdminToken(); got != stable {
		t.Errorf("重启后令牌文件 = %q，期望仍为 %q（存在即复用）", got, stable)
	}

	srvA := &contractServer{Server: procA, env: base.env, token: stable, upstream: base.upstream}
	srvB := &contractServer{Server: procB, env: base.env, token: stable, upstream: base.upstream}

	hdr := map[string]string{"X-Agmgr-Token": stable}
	if res := callWithHeaders(t, srvA, http.MethodGet, "/api/v1/tools", "", hdr); !res.ok {
		t.Errorf("进程 A 用复用令牌必须放行: %s", res.body)
	}
	// F2 的核心：旧令牌（=同一枚令牌）在新进程上**继续有效**——agserv 永久缓存的前提。
	if res := callWithHeaders(t, srvB, http.MethodGet, "/api/v1/tools", "", hdr); !res.ok {
		t.Errorf("跨重启后同一枚令牌必须继续放行: %s", res.body)
	}
	// 形状错误的令牌仍被拒（复用不放松鉴权）。
	badHdr := map[string]string{"X-Agmgr-Token": "bad-token"}
	if stale := callWithHeaders(t, srvB, http.MethodGet, "/api/v1/tools", "", badHdr); stale.ok {
		t.Errorf("形状错误的令牌必须被拒")
	} else if stale.status != http.StatusUnauthorized || stale.code != apierr.CodeUnauthorized {
		t.Errorf("坏令牌被拒的形态 = %d/%q，期望 401/unauthorized", stale.status, stale.code)
	}
	// 免鉴权端点不受令牌形态影响（健康检查与版本是启动路径的探针）。
	if res := srvB.callRaw(t, http.MethodGet, "/api/v1/health", "", false); !res.ok {
		t.Errorf("免鉴权端点须可用: %s", res.body)
	}
}

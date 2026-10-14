package diagnose

import "testing"

// TestUsernameFor 守住用户名的推导口径（推不出就空串，不拿目录名冒充）。
func TestUsernameFor(t *testing.T) {
	cases := []struct {
		home string
		want string
	}{
		{"/Users/q", "q"},
		{"/Users/example-user/", "example-user"},
		{"/home/alice", "alice"},
		{"/tmp/xyz/Users/synthuser123", "synthuser123"}, // 夹具形态：取最后一段 /Users/<名>
		{"/var/folders/xx/T/tmp123", ""},                // 推不出就是不推
		{"", ""},
	}
	for _, tc := range cases {
		if got := UsernameFor(tc.home); got != tc.want {
			t.Errorf("UsernameFor(%q) = %q，期望 %q", tc.home, got, tc.want)
		}
	}
}

// TestMaskPersonal 守住复制面的脱敏：家目录 → ~、个人目录段与裸用户名 → <已脱敏>。
func TestMaskPersonal(t *testing.T) {
	const home = "/Users/synthuser123"
	const user = "synthuser123"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"家目录前缀", "数据根 " + home + "/.yzagmgr 就绪", "数据根 ~/.yzagmgr 就绪"},
		{"家目录即全部", home, "~"},
		{"他人个人目录段", "文件 /Users/otheruser/.zshrc 被覆盖", "文件 /Users/<已脱敏>/.zshrc 被覆盖"},
		{"linux 个人目录段", "/home/someone/config.toml", "/home/<已脱敏>/config.toml"},
		{"裸用户名（URL 主机）", "https://synthuser123.example/api", "https://<已脱敏>.example/api"},
		{"裸用户名（邮箱）", "mail synthuser123@example.com", "mail <已脱敏>@example.com"},
		{"更长记号里的子串不掩", "用户 anothersynthuser123x 不是本人", "用户 anothersynthuser123x 不是本人"},
		{"非个人路径照常保留", "/usr/local/bin/codex 与 /Applications/Claude.app", "/usr/local/bin/codex 与 /Applications/Claude.app"},
		{"幂等", "~/.yzagmgr 与 /Users/<已脱敏>/x", "~/.yzagmgr 与 /Users/<已脱敏>/x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MaskPersonal(tc.in, home, user); got != tc.want {
				t.Errorf("MaskPersonal(%q) = %q，期望 %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestMaskPersonalShortUsernameBoundary 守住短用户名的边界：裸记号替换只对
// ≥ 3 字符用户名生效（否则会把正常文本切碎），但家目录与目录段规则仍然覆盖。
func TestMaskPersonalShortUsernameBoundary(t *testing.T) {
	const home = "/Users/q"
	if got := MaskPersonal("q 是用户名，磁盘上还有 qatar 与 quote", home, "q"); got != "q 是用户名，磁盘上还有 qatar 与 quote" {
		t.Errorf("短用户名不得参与裸记号替换（会切碎正常文本）: %q", got)
	}
	if got := MaskPersonal("/Users/q/.zshrc 未生效", home, "q"); got != "~/.zshrc 未生效" {
		t.Errorf("短用户名仍须由家目录规则覆盖: %q", got)
	}
	if got := MaskPersonal("受影响文件 /Users/q/other", home, "q"); got != "受影响文件 ~/other" {
		t.Errorf("家目录（含后续段）应整体替换为 ~: %q", got)
	}
}

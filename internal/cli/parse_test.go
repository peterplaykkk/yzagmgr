package cli

import (
	"errors"
	"strings"
	"testing"
)

// TestParseSubcommands 是子命令解析的表驱动测试：正常、边界、错误路径全覆盖。
// 断言解析**结果**（Kind/Tool/Profile/Backup/JSON），不是只断言「报没报错」——
// 合法形态解析错了比报错更危险（会去打错的 REST 端点）。
func TestParseSubcommands(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    Cmd
		wantErr string // 空 = 合法；非空 = 错误消息须含它
	}{
		// ── 守护进程形态（绝不能破坏） ─────────────────────────────────────
		{name: "无参数 = 守护进程", args: nil, want: Cmd{Kind: KindDaemon}},
		{name: "空切片 = 守护进程", args: []string{}, want: Cmd{Kind: KindDaemon}},
		{name: "serve = 守护进程", args: []string{"serve"}, want: Cmd{Kind: KindDaemon}},
		{
			name: "LaunchServices -psn 残留 = 守护进程",
			args: []string{"-psn_0_1234567"},
			want: Cmd{Kind: KindDaemon},
		},
		{
			name: "多个 -psn 残留 = 守护进程",
			args: []string{"-psn_0_1", "-psn_0_2"},
			want: Cmd{Kind: KindDaemon},
		},
		{name: "serve 带参数被拒", args: []string{"serve", "extra"}, wantErr: "serve 不接受"},
		{name: "serve 带旗标被拒", args: []string{"serve", "--json"}, wantErr: "serve 不接受"},
		{name: "孤立旗标无子命令被拒", args: []string{"--json"}, wantErr: "旗标必须与子命令"},

		// ── help ──────────────────────────────────────────────────────────
		{name: "help", args: []string{"help"}, want: Cmd{Kind: KindHelp}},
		{name: "--help", args: []string{"--help"}, want: Cmd{Kind: KindHelp}},
		{name: "-h", args: []string{"-h"}, want: Cmd{Kind: KindHelp}},
		{name: "help 带参数被拒", args: []string{"help", "x"}, wantErr: "help 不接受"},

		// ── health ────────────────────────────────────────────────────────
		{name: "health", args: []string{"health"}, want: Cmd{Kind: KindHealth}},
		{name: "health --json", args: []string{"health", "--json"}, want: Cmd{Kind: KindHealth, JSON: true}},
		{name: "--json 可在前", args: []string{"--json", "health"}, want: Cmd{Kind: KindHealth, JSON: true}},
		{name: "health 多余参数被拒", args: []string{"health", "x"}, wantErr: "health 需要 0 个参数"},
		{name: "health 未知旗标被拒", args: []string{"health", "--bogus"}, wantErr: "未知旗标"},

		// ── tools ─────────────────────────────────────────────────────────
		{name: "tools list", args: []string{"tools", "list"}, want: Cmd{Kind: KindToolsList}},
		{
			name: "tools list --json",
			args: []string{"tools", "list", "--json"},
			want: Cmd{Kind: KindToolsList, JSON: true},
		},
		{
			name: "tools show pi",
			args: []string{"tools", "show", "pi"},
			want: Cmd{Kind: KindToolsShow, Tool: "pi"},
		},
		{
			name: "tools show 带 --json",
			args: []string{"tools", "show", "opencode", "--json"},
			want: Cmd{Kind: KindToolsShow, Tool: "opencode", JSON: true},
		},
		{name: "tools 无子命令被拒", args: []string{"tools"}, wantErr: "tools 需要子命令"},
		{name: "tools show 缺 id 被拒", args: []string{"tools", "show"}, wantErr: "tools show 需要 1 个参数"},
		{
			name:    "tools show 多余 id 被拒",
			args:    []string{"tools", "show", "pi", "claude"},
			wantErr: "tools show 需要 1 个参数",
		},
		{name: "tools 未知子命令被拒", args: []string{"tools", "bogus"}, wantErr: "未知子命令"},
		{name: "tools list 多余参数被拒", args: []string{"tools", "list", "x"}, wantErr: "tools list 需要 0 个参数"},

		// ── preview / apply ───────────────────────────────────────────────
		{
			name: "preview 带 profile",
			args: []string{"preview", "pi", "--profile", "yzagmgr-local"},
			want: Cmd{Kind: KindPreview, Tool: "pi", Profile: "yzagmgr-local"},
		},
		{
			name: "preview 带 --profile= 等号形态",
			args: []string{"preview", "pi", "--profile=yzagmgr-local"},
			want: Cmd{Kind: KindPreview, Tool: "pi", Profile: "yzagmgr-local"},
		},
		{
			name: "preview 旗标可在位置参数前",
			args: []string{"preview", "--profile", "official", "claude", "--json"},
			want: Cmd{Kind: KindPreview, Tool: "claude", Profile: "official", JSON: true},
		},
		{
			name: "重复 --profile 取最后一个",
			args: []string{"preview", "pi", "--profile", "a", "--profile", "b"},
			want: Cmd{Kind: KindPreview, Tool: "pi", Profile: "b"},
		},
		{
			name: "apply 带 profile",
			args: []string{"apply", "claude", "--profile", "official"},
			want: Cmd{Kind: KindApply, Tool: "claude", Profile: "official"},
		},
		{
			name: "apply 带 --json",
			args: []string{"apply", "opencode", "--profile", "yzagmgr-local", "--json"},
			want: Cmd{Kind: KindApply, Tool: "opencode", Profile: "yzagmgr-local", JSON: true},
		},
		{
			// UC-043：router 形态下它是**选择器名**（取值校验在守护进程侧）。
			name: "preview 带 --model-id（选择器名）",
			args: []string{"preview", "pi", "--profile", "yzagmgr-router", "--model-id", "mz-alias"},
			want: Cmd{Kind: KindPreview, Tool: "pi", Profile: "yzagmgr-router", ModelID: "mz-alias"},
		},
		{
			name: "apply 带 --model-id= 等号形态",
			args: []string{"apply", "pi", "--profile", "yzagmgr-router", "--model-id=mz-alias"},
			want: Cmd{Kind: KindApply, Tool: "pi", Profile: "yzagmgr-router", ModelID: "mz-alias"},
		},
		{
			name: "preview --model-id 空串 = 没给（不在请求体写键）",
			args: []string{"preview", "pi", "--profile", "yzagmgr-router", "--model-id="},
			want: Cmd{Kind: KindPreview, Tool: "pi", Profile: "yzagmgr-router"},
		},
		{
			name:    "preview 不收 --endpoint",
			args:    []string{"preview", "pi", "--profile", "p", "--endpoint", "https://e.example"},
			wantErr: "只接受 --profile 与 --model-id",
		},
		{
			name:    "restore 不收 --model-id",
			args:    []string{"restore", "--model-id", "x"},
			wantErr: "不接受",
		},
		{
			name:    "health 不收 --model-id",
			args:    []string{"health", "--model-id", "x"},
			wantErr: "不接受",
		},
		{name: "preview 缺 --profile 被拒", args: []string{"preview", "pi"}, wantErr: "preview 需要 --profile"},
		{name: "apply 缺 --profile 被拒", args: []string{"apply", "pi"}, wantErr: "apply 需要 --profile"},
		{name: "preview 缺工具被拒", args: []string{"preview", "--profile", "x"}, wantErr: "preview 需要 1 个参数"},
		{name: "preview --profile 空串被拒", args: []string{"preview", "pi", "--profile="}, wantErr: "preview 需要 --profile"},
		{name: "preview 工具为空被拒", args: []string{"preview", "", "--profile", "x"}, wantErr: "工具 id 不得为空"},
		{name: "preview --profile 缺取值被拒", args: []string{"preview", "pi", "--profile"}, wantErr: "--profile 缺少取值"},
		{name: "preview 不收 --backup", args: []string{"preview", "pi", "--profile", "x", "--backup", "y"}, wantErr: "--backup 只用于 restore"},
		{name: "preview 多余参数被拒", args: []string{"preview", "pi", "x", "--profile", "p"}, wantErr: "preview 需要 1 个参数"},

		// ── restore ───────────────────────────────────────────────────────
		{name: "restore 无参", args: []string{"restore"}, want: Cmd{Kind: KindRestore}},
		{
			name: "restore --backup",
			args: []string{"restore", "--backup", "bk-1-1"},
			want: Cmd{Kind: KindRestore, Backup: "bk-1-1"},
		},
		{
			name: "restore --backup= 等号形态 + --json",
			args: []string{"restore", "--backup=bk-1-1", "--json"},
			want: Cmd{Kind: KindRestore, Backup: "bk-1-1", JSON: true},
		},
		{name: "restore 位置参数被拒", args: []string{"restore", "pi"}, wantErr: "restore 需要 0 个参数"},
		{name: "restore --backup 缺取值被拒", args: []string{"restore", "--backup"}, wantErr: "--backup 缺少取值"},
		{name: "restore 不收 --profile", args: []string{"restore", "--profile", "x"}, wantErr: "--profile 只用于 preview"},

		// ── backups ───────────────────────────────────────────────────────
		{name: "backups list", args: []string{"backups", "list"}, want: Cmd{Kind: KindBackupsList}},
		{
			name: "backups list --json",
			args: []string{"backups", "list", "--json"},
			want: Cmd{Kind: KindBackupsList, JSON: true},
		},
		{name: "backups 无子命令被拒", args: []string{"backups"}, wantErr: "backups 需要子命令"},
		{name: "backups 未知子命令被拒", args: []string{"backups", "ls"}, wantErr: "未知子命令"},
		{name: "backups list 多余参数被拒", args: []string{"backups", "list", "x"}, wantErr: "backups list 需要 0 个参数"},
		{
			name:    "backups list 不收 --backup",
			args:    []string{"backups", "list", "--backup", "x"},
			wantErr: "backups list 不接受",
		},

		// ── policy ────────────────────────────────────────────────────────
		{name: "policy show", args: []string{"policy", "show"}, want: Cmd{Kind: KindPolicyShow}},
		{name: "policy refresh", args: []string{"policy", "refresh"}, want: Cmd{Kind: KindPolicyRefresh}},
		{
			name: "policy refresh --json",
			args: []string{"policy", "refresh", "--json"},
			want: Cmd{Kind: KindPolicyRefresh, JSON: true},
		},
		{name: "policy 无子命令被拒", args: []string{"policy"}, wantErr: "policy 需要子命令"},
		{name: "policy 未知子命令被拒", args: []string{"policy", "reload"}, wantErr: "未知子命令"},
		{name: "policy show 多余参数被拒", args: []string{"policy", "show", "x"}, wantErr: "policy show 需要 0 个参数"},
		{name: "policy 不收 --profile", args: []string{"policy", "show", "--profile", "x"}, wantErr: "policy 不接受"},

		// ── 未知子命令 ─────────────────────────────────────────────────────
		{name: "未知子命令被拒", args: []string{"frobnicate"}, wantErr: `未知子命令 "frobnicate"`},
		{name: "version 也是未知子命令（本次最小集不含它）", args: []string{"version"}, wantErr: "未知子命令"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.args)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Parse(%q) = %+v, 期望错误含 %q", tt.args, got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Parse(%q) 错误 = %q, 期望含 %q", tt.args, err.Error(), tt.wantErr)
				}
				var ue *usageError
				if !errors.As(err, &ue) {
					t.Fatalf("Parse(%q) 错误类型 = %T, 期望 *usageError（退出码 2 的前提）", tt.args, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) 报错: %v", tt.args, err)
			}
			if got != tt.want {
				t.Fatalf("Parse(%q) = %+v, 期望 %+v", tt.args, got, tt.want)
			}
		})
	}
}

// TestUsageTextMentionsEverySubcommand 守住「help 与解析器不漂移」：
// Parse 接受的每个子命令都必须出现在帮助文本里，用户才找得到。
func TestUsageTextMentionsEverySubcommand(t *testing.T) {
	text := UsageText()
	for _, want := range []string{
		"yzagmgr serve",
		"yzagmgr help",
		"yzagmgr health",
		"yzagmgr tools list",
		"yzagmgr tools show",
		"yzagmgr preview",
		"yzagmgr apply",
		"yzagmgr restore",
		"yzagmgr backups list",
		"yzagmgr policy show",
		"yzagmgr policy refresh",
		"YZAGMGR_HOME",
		"YZAGMGR_API_PORT",
		"admin.token",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("UsageText() 缺 %q", want)
		}
	}
}

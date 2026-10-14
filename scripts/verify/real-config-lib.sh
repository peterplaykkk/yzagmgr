#!/usr/bin/env bash
# scripts/verify/real-config-lib.sh —— 真实配置保护面（被 source，不独立执行）。
#
# 只有 scripts/verify/{snapshot,backup,restore}-real-config.sh 使用本文件；
# 它们是全仓库**仅有的**触碰真实 $HOME 的验收脚本（doc/acceptance-steps.md §2.2/§8.0）。
#
# 保护面（按 acceptance-recipe §A 的十家族落盘路径 + 桌面应用 4 文件 + v1 配置面）：
#   - 十家族 live 文件：claude(含旧名 claude.json) / claude-desktop(4 文件) / codex(2) /
#     gemini(2) / grokbuild / opencode / openclaw / hermes / pi(2) / workbuddy(2)；
#   - （v1 配置面清理段已删——design §2.6：归档无旧 v1 存量升级义务。）
#
# 默认**不**纳入（理由写在这里，避免把「有意的边界」误读成「漏了」）：
#   - （v1 数据根清理段同删——design §2.6。）
#     ~/Library/Application Support/Claude-3p/**（11 GiB）：属**运行态大文件**，
#     且 v1/Claude Desktop 运行期间持续写入 ⇒ 纳入快照会让「前后 diff 必须为空」
#     的判据恒假（噪声），也不能保护任何「配置」。需要全量归档时显式传 --full。
#   - ~/.claude.json：不是任何家族的受管路径（acceptance-recipe §K-3 明确不纳入
#     family_paths），且本机 Claude Code 会话在持续改写它 ⇒ 纳入只会制造假差异。
#
# 快照行格式（TAB 分隔，按路径排序 ⇒ 可直接 diff）：
#   <sha256>\t<size>\t<abs-path>
#   absent\t-\t<abs-path>

REAL_CONFIG_FULL="${REAL_CONFIG_FULL:-0}"

# 十家族 live 文件（相对 ${HOME}；claude 的旧名与当前名都列，不存在会记 absent）
_real_config_family_paths() {
  cat <<'EOF'
.claude/settings.json
.claude/claude.json
.codex/config.toml
.codex/auth.json
.gemini/.env
.gemini/settings.json
.grok/config.toml
.config/opencode/opencode.json
.openclaw/openclaw.json
.hermes/config.yaml
.pi/agent/models.json
.pi/agent/settings.json
.workbuddy/models.json
.workbuddy/settings.json
Library/Application Support/Claude/claude_desktop_config.json
Library/Application Support/Claude-3p/claude_desktop_config.json
Library/Application Support/Claude-3p/configLibrary/00000000-0000-4000-8000-000000157210.json
Library/Application Support/Claude-3p/configLibrary/_meta.json
EOF
}

# v1 配置面（相对 ${HOME}，目录递归）——条目已清空：v1 清理段删除（design §2.6）。
_real_config_v1_config_paths() {
  cat <<'EOF'
EOF
}

# 全量面（--full 才用）：v1 数据根与桌面应用目录整树
_real_config_full_paths() {
  cat <<'EOF'
Library/Application Support/Claude
EOF
}

# 打印保护面绝对路径清单（去重 + 稳定排序）。
real_config_paths() {
  local home="$1"
  local -a rels=()
  while IFS= read -r rel; do [[ -n "$rel" ]] && rels+=("$rel"); done < <(_real_config_family_paths)

  if [[ "$REAL_CONFIG_FULL" == "1" ]]; then
    while IFS= read -r rel; do [[ -n "$rel" ]] && rels+=("$rel"); done < <(_real_config_full_paths)
  else
    while IFS= read -r rel; do [[ -n "$rel" ]] && rels+=("$rel"); done < <(_real_config_v1_config_paths)
  fi

  local rel abs
  for rel in "${rels[@]}"; do
    abs="$home/$rel"
    if [[ -d "$abs" ]]; then
      # 目录：递归展开为文件（排序保证确定性）
      find "$abs" -type f -print 2>/dev/null | LC_ALL=C sort
    else
      printf '%s\n' "$abs"
    fi
  done | LC_ALL=C sort -u
}

real_config_scope_label() {
  if [[ "$REAL_CONFIG_FULL" == "1" ]]; then
    printf 'full（十家族 + v1 数据根整树 + Claude 应用目录整树）'
  else
    printf 'tool-config（十家族 live 文件 + v1 配置面 settings/profiles/secrets）'
  fi
}

# 快照：打印全部保护面路径的 sha256。
real_config_snapshot() {
  local home="$1"
  printf '# real-config snapshot scope=%s home=%s\n' "$(real_config_scope_label)" "$home"
  local path
  while IFS= read -r path; do
    if [[ -f "$path" ]]; then
      printf '%s\t%s\t%s\n' "$(sha256_file "$path")" "$(stat -f '%z' "$path")" "$path"
    else
      printf 'absent\t-\t%s\n' "$path"
    fi
  done < <(real_config_paths "$home")
}

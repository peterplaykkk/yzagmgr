package mcp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
)

// intent 是一次物化的**解析后意图**（preview 时定格进租约，apply 按它重算，
// 注册表在 preview 之后的改动不静默生效——要重新 preview，UC-111 主流程）。
type intent struct {
	Tool    string            `json:"tool"`
	Op      string            `json:"op"` // apply | remove
	Names   []string          `json:"names"`
	Entries map[string]Server `json:"entries,omitempty"`
}

// Op 是一次 preview/apply 的请求意图。
type Op struct {
	Tool  string
	Mode  string // apply | remove
	Names []string
}

// render 把意图叠加到 raw（可能是缺席文件的 nil）上，返回目标字节。
//
// 实现纪律：**逐名 parse→edit→render 循环**，每轮都在最新渲染结果上重新解析。
// docmodel 的节点区间指向源字节，同一 Doc 里对同一父对象的多次 insert 会落在
// 同一坐标（区间冲突/次序未定义）；逐名成环从结构上消除了这个坑，代价是 O(n)
// 次解析——条目数个位数、文件 KB 级，可接受。非操作集字节在每轮原样透传。
func render(path string, raw []byte, it intent) ([]byte, error) {
	if it.Op == "apply" {
		return renderApply(path, raw, it)
	}
	return renderRemove(path, raw, it)
}

func renderApply(path string, raw []byte, it intent) ([]byte, error) {
	if len(it.Names) == 0 {
		return validateParsable(path, raw)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		// 缺席/空白文件：从 `{}` 起以**紧凑值**一次成型建档（键序由 encoding/json
		// 排序）。紧凑是刻意的——后续 apply 的原地替换也产紧凑值，冷启动与重
		// apply 的字节才逐字节一致（用多行规范形态建档会让第二次 apply 改字节、
		// 永远到不了 unchanged）。尾随换行保留给文本文件习惯，remove 后随空壳
		// 一起存活（A6 边界：{}\n）。
		bucket := map[string]any{}
		for _, name := range it.Names {
			bucket[name] = it.Entries[name].value()
		}
		doc, err := docmodel.ParseJSON([]byte("{}"))
		if err != nil {
			return nil, configInvalid(path, err)
		}
		if err := doc.SetJSON(bucket, "mcpServers"); err != nil {
			return nil, configInvalid(path, err)
		}
		out := append([]byte{}, doc.Render()...)
		return append(out, '\n'), nil
	}
	cur := raw
	for _, name := range it.Names {
		doc, err := docmodel.ParseJSON(cur)
		if err != nil {
			return nil, configInvalid(path, err)
		}
		ms := doc.Root().Get("mcpServers")
		enc, err := encodeValue(it.Entries[name].value())
		if err != nil {
			return nil, configInvalid(path, err)
		}
		switch {
		case ms == nil:
			// 键缺席：SetJSON 在根上插入紧凑单行容器。排版一致性是后续 Delete 的
			// 前提——docmodel 的行启发式在「单行容器+成员间换行」的混血布局下会把
			// 容器开口一起吃掉；单行容器的后续成员插入走下面的 default 分支（不带
			// 换行），容器始终保持单行一致。
			if err := doc.SetJSON(map[string]any{name: it.Entries[name].value()}, "mcpServers"); err != nil {
				return nil, configInvalid(path, err)
			}
		case ms.Kind != docmodel.KObject:
			// 值形状是数组/标量/null：既不能插成员也不该被本写链静默改形——显式拒。
			return nil, apierr.New(apierr.CodeConfigInvalid,
				"mcpServers 不是对象（%s），拒写: %s", ms.Kind, path)
		case containerMultiline(ms, cur):
			// 多行容器：SetJSON 对已存在成员是原地替换（幂等 upsert），缺席才插入。
			if err := doc.SetJSON(it.Entries[name].value(), "mcpServers", name); err != nil {
				return nil, configInvalid(path, err)
			}
		default:
			// 单行对象容器（含空对象）：
			//   已有同名成员 → 原地替换值区间（否则重复键：单行容器正是本写链自己
			//   造的形态，缺这一步会让任何第二次 apply 无界追加同名旧值）；
			//   空对象 {}   → 走 SetJSON 的空容器插入分支；
			//   否则        → 末成员后追加 `, "k": v`（不带换行，保持单行一致——
			//   docmodel 的 insertMember 恒插 `\n`，会造出混血布局）。
			if m := ms.MemberOf(name); m != nil {
				if err := doc.ReplaceByteRange(m.Val.Start, m.Val.End, enc); err != nil {
					return nil, configInvalid(path, err)
				}
			} else if len(ms.Members) == 0 {
				if err := doc.SetJSON(it.Entries[name].value(), "mcpServers", name); err != nil {
					return nil, configInvalid(path, err)
				}
			} else {
				last := ms.Members[len(ms.Members)-1]
				keyBytes, err := encodeKey(name)
				if err != nil {
					return nil, configInvalid(path, err)
				}
				repl := append([]byte{',', ' '}, keyBytes...)
				repl = append(repl, ':', ' ')
				repl = append(repl, enc...)
				if err := doc.ReplaceByteRange(last.End, last.End, repl); err != nil {
					return nil, configInvalid(path, err)
				}
			}
		}
		cur = doc.Render()
	}
	return cur, nil
}

// containerMultiline 判定容器是「成员各占一行」的多行排版（首成员前有换行）。
func containerMultiline(ms *docmodel.Node, src []byte) bool {
	if len(ms.Members) == 0 {
		return false
	}
	first := ms.Members[0]
	return bytes.IndexByte(src[ms.Start+1:first.Start], '\n') >= 0
}

func renderRemove(path string, raw []byte, it intent) ([]byte, error) {
	if len(it.Names) == 0 {
		return validateParsable(path, raw)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return raw, nil
	}
	cur := raw
	removedAny := false
	for _, name := range it.Names {
		doc, err := docmodel.ParseJSON(cur)
		if err != nil {
			return nil, configInvalid(path, err)
		}
		if doc.Root().Get("mcpServers", name) == nil {
			continue
		}
		if err := doc.Delete("mcpServers", name); err != nil {
			return nil, configInvalid(path, err)
		}
		cur = doc.Render()
		removedAny = true
	}
	if !removedAny {
		return raw, nil
	}
	// 本次真删过才做空容器收尾：他手写的空 mcpServers 键在零删除时原样保留。
	// 空容器整体摘键（而非留 `{}`），使「起始无键」的字节往返成立（UC-111 A6）。
	doc, err := docmodel.ParseJSON(cur)
	if err != nil {
		return nil, configInvalid(path, err)
	}
	if ms := doc.Root().Get("mcpServers"); ms != nil && ms.Len() == 0 {
		if err := doc.Delete("mcpServers"); err != nil {
			return nil, configInvalid(path, err)
		}
		cur = doc.Render()
	}
	return cur, nil
}

// subtreeHash 把「我方目标条目」在 raw 中的现状钉成指纹（预览时记录、应用时复核）。// 他键与非目标条目的漂移不进指纹——那是 apply 期意图重算要吸收的（UC-111 A5）。
func subtreeHash(raw []byte, names []string) string {
	h := sha256.New()
	if len(bytes.TrimSpace(raw)) == 0 {
		h.Write([]byte("<absent>"))
	} else {
		doc, err := docmodel.ParseJSON(raw)
		if err != nil {
			// 解析失败的现状也必须是一个确定指纹：让 apply 复核时同样报错
			// （读侧 config_invalid），而不是误判成「我方条目没变」。
			h.Write([]byte("<invalid-json>"))
		} else {
			for _, name := range names {
				h.Write([]byte(name))
				h.Write([]byte{0})
				if n := doc.Root().Get("mcpServers", name); n != nil {
					h.Write([]byte{1})
					// 规范化后哈希：键序/空白的纯格式重排**不算**我方条目被改
					// （值等价即吸收，UC-111 A5 分支一），只有值变了才是冲突。
					var v any
					if err := json.Unmarshal(n.AsRaw(), &v); err != nil {
						h.Write(n.AsRaw())
					} else if norm, merr := json.Marshal(v); merr == nil {
						h.Write(norm)
					} else {
						h.Write(n.AsRaw())
					}
				} else {
					h.Write([]byte{0})
				}
				h.Write([]byte{0})
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// resolveNames 把请求里的 names 归一成字典序去重集合。
func resolveNames(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, n := range in {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// parseObject 解析顶层对象（状态读面用）；非对象/非法 JSON 返回错误。
func parseObject(raw []byte) (*docmodel.Doc, error) {
	return docmodel.ParseJSON(raw)
}

// encodeValue 把条目渲染值编码为紧凑 JSON（键排序由 encoding/json 保证）。
func encodeValue(v map[string]any) ([]byte, error) {
	blob, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return blob, nil
}

// encodeKey 编码成员键（带引号）。
func encodeKey(k string) ([]byte, error) {
	blob, err := json.Marshal(k)
	if err != nil {
		return nil, err
	}
	return blob, nil
}

// validateParsable 在 names 为空的收口路径仍校验在场目标是合法 JSON，保持与
// GET /mcp/tools 读面的同一口径（非法 JSON 一律 config_invalid，不出现「preview
// 改口成功、status 报错」的双面）。
func validateParsable(path string, raw []byte) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) > 0 {
		if _, err := docmodel.ParseJSON(raw); err != nil {
			return nil, configInvalid(path, err)
		}
	}
	return raw, nil
}

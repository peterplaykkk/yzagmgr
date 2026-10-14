// 失败信封的令牌形态脱敏（UC-014 A11 的响应面收口，2026-09-27）。
//
// 背景（A3 判红复现）：家族与 docmodel 会把「解析失败处的记号原文」回显进
// config_invalid 的 message（docmodel 的 `记号 %q 不是合法 TOML 值` 一类），而记号
// 可以就是用户 live 配置里的令牌明文——codex/grokbuild 的 TOML 值位即实测复现点
// （`.codex/config.toml` 写 `model = sk-ant-…` ⇒ preview 的 error.message 带明文）。
//
// 修法取「信封装配的统一咽喉点」而不是逐个回显点：回显分支散布在各家族的解析/写入链
// （且会随家族新增），逐点掩必然漏；而**所有失败响应**都经 apierr.Fail(...).WriteJSON
// 出去，在 api 层收口能一次罩住「任何族、任何错误分支」的 message 与 details。
//
// 为什么不在 apierr 包内做：依赖方向是 envcheck → apierr（envcheck 的修复面用 apierr
// 报错），apierr 反向 import envcheck 会成环；掩码实现只此一处来源
// （envcheck.MaskTokenShapes，与 §9 整行脱敏/差异面同一条前缀表与边界判据）。
//
// 为什么「掩区间」而不是「整段抹除」：错误信息要保住诊断价值（路径、偏移、原因），
// 掩码只把令牌形态的区间换成 `<已脱敏>`，其余文本逐字保留。
package api

import (
	"reflect"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/envcheck"
)

// failEnvelope 构造失败信封：先对错误做令牌形态脱敏，再进 apierr.Fail。
//
// **api 包内所有失败响应都必须从这里出去**（统一咽喉点）：既有的
// apierr.Fail(apierr.From(err)) 直连写法没有掩码这一步，A11 就是从这里漏的。
func failEnvelope(err error) apierr.Response {
	return apierr.Fail(maskedEnvelopeError(apierr.From(err)))
}

// maskedEnvelopeError 返回脱敏后的错误**副本**（nil 透传）。
//
// 刻意不原地改：同一个 *apierr.Error 还会被操作日志、进程日志与错误分支判断使用，
// 掩码是响应呈现层的事，不该反向污染这些面。message 与 details 里每个可达字符串
// 都过 envcheck.MaskTokenShapes。
func maskedEnvelopeError(ae *apierr.Error) *apierr.Error {
	if ae == nil {
		return nil
	}
	out := &apierr.Error{Code: ae.Code, Message: envcheck.MaskTokenShapes(ae.Message)}
	if ae.Details != nil {
		out.Details = maskDetailValue(ae.Details).(map[string]any)
	}
	return out
}

// maskedErrorText 返回错误的文本形态（`err.Error()`，**含**错误码前缀）过一遍令牌形态掩码。
//
// 用于「错误文本进**成功**信封」的面（/delivery/switch 的逐工具结果、
// /materialize 的 partialFailures[].reason）：它们承载的是同一条错误消息
// （可达路径与 preview 相同——配置解析失败的记号回显），A11 的「不含令牌明文」
// 一体适用；掩码口径与 maskedEnvelopeError 同一处来源。刻意走 err.Error() 整串
// （不走 From().Message）：这些面的既有文案含 `invalid_request:` 一类码前缀，
// 是调用方与测试依赖的形态，不得因掩码而丢。
func maskedErrorText(err error) string {
	if err == nil {
		return ""
	}
	return envcheck.MaskTokenShapes(err.Error())
}

// maskDetailValue 深拷贝一个 details 值，并把其中所有可达的字符串脱敏。
//
// 覆盖 string / map（键与值）/ slice / array / struct（可导出字段）/ pointer /
// interface；其余类型（数字、布尔、时间等）原样返回——它们不可能承载令牌文本。
// 未导出的结构字段读写都够不着，按复制值保留（details 的形状是仓库自己的结构体，
// 令牌文本只会出现在可导出字段与 map/slice 里）。
func maskDetailValue(v any) any {
	if v == nil {
		return nil
	}
	return maskReflectValue(reflect.ValueOf(v)).Interface()
}

// maskReflectValue 递归下钻一个反射值：字符串换掩码，容器重建副本。
func maskReflectValue(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.String:
		s := v.String()
		masked := envcheck.MaskTokenShapes(s)
		if masked == s {
			return v
		}
		return reflect.ValueOf(masked).Convert(v.Type())
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			out.SetMapIndex(maskReflectValue(iter.Key()), maskReflectValue(iter.Value()))
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(maskReflectValue(v.Index(i)))
		}
		return out
	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(maskReflectValue(v.Index(i)))
		}
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v) // 先整份复制，再逐字段覆盖（未导出字段只能停留在复制值上）
		for i := 0; i < v.NumField(); i++ {
			if !out.Field(i).CanSet() {
				continue
			}
			out.Field(i).Set(maskReflectValue(v.Field(i)))
		}
		return out
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(maskReflectValue(v.Elem()))
		return out
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(maskReflectValue(v.Elem()))
		return out
	default:
		return v
	}
}

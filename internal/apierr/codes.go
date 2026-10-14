// Package apierr 定义 yzagmgr 本地 API 的封闭错误码集合与统一信封。
//
// 契约来源：doc/api-v1.md §1.2 / §1.3。
// 新增码必须同时改该文档与 doc/api-v1.md，并由 TestErrorCodesAreAClosedSet 守住。
package apierr

// Code 是错误码。取值只能是本文件声明的常量之一。
type Code string

const (
	// 请求层
	CodeInvalidRequest  Code = "invalid_request"
	CodeUnauthorized    Code = "unauthorized"
	CodeForbiddenOrigin Code = "forbidden_origin"
	CodeUnauthenticated Code = "unauthenticated"
	CodeNotFound        Code = "not_found"

	// 配置封层
	CodeConfigChanged    Code = "config_changed"
	CodeConfigInvalid    Code = "config_invalid"
	CodePathConflict     Code = "path_conflict"
	CodePermissionDenied Code = "permission_denied"
	CodePreviewExpired   Code = "preview_expired"
	CodePreviewInUse     Code = "preview_in_use"
	CodeBackupUnreadable Code = "backup_unreadable"

	// 存储
	CodeStorage Code = "storage"

	// 下载与更新
	CodeIntegrityFailed Code = "integrity_failed"

	// 网关与登录
	CodeGatewayUnreachable        Code = "gateway_unreachable"
	CodeGatewayUnexpectedResponse Code = "gateway_unexpected_response"
	CodeLoginExpired              Code = "login_expired"
	CodeCancelled                 Code = "cancelled"

	// WorkBuddy
	CodeChannelNotConfigured Code = "channel_not_configured"

	// 配置测试（UC-039）
	//
	// 「同一工具的配置测试已在跑」是一条**独立**的失败语义：它不是参数错（调用方给的
	// tool/卡都对）、不是资源不存在、也不是「过期」（上一次测试还在跑时它并没有过期）。
	// 与脚本类错误同理独占一码，免得消费方按 `invalid_request` 的文案去猜。
	CodeTestInProgress Code = "test_in_progress"

	// 个人配置的乐观并发（UC-094 A5）
	//
	// 「保存所基于的版本已不是当前版本」是一条**独立**的失败语义：请求本身合法、目标卡
	// 也存在，是「另一个写者在你读取之后改过它」这一**状态冲突**（与 test_in_progress 同
	// 一族）。消费方据此走「看两版并重新提交」，而不是把 `invalid_request` 的文案当提示。
	CodeVersionConflict Code = "version_conflict"

	// 能力闸门（UC-094 A7）
	//
	// 「保存的能力声明与所选模型的能力事实不兼容且未获显式确认」是一条**独立**的失败
	// 语义：不是参数形状错（字段都合法），而是「存下去会得到一条伪能力」这一**兼容性
	// 冲突**。details 里带机器可读的 checks 清单（供界面标出并让用户确认或修正）。
	CodeCapabilityConflict Code = "capability_conflict"

	// 限额（D32 / UC-041）：数据面按用户限额**前置拒绝**（未发起任何上游请求）。
	CodeQuotaExceeded Code = "quota_exceeded"

	// 吞吐速率（design.md §1.4④ / design-b §5.4）：数据面按 RPM/TPM **前置拒绝**
	// （未发起任何上游请求）。与 quota_exceeded 语义刻意分开：quota 要等重置/agserv
	// 调整，rate_limited 下个自然分钟自愈（分钟窗限速器，见 proxy/rategate.go）。
	// 三处同步：codes.go（本处）/ doc/api-v1.md §1.3（T7）/ web errors.ts（T5）——IP4。
	CodeRateLimited Code = "rate_limited"

	// 用量脚本
	CodeScriptTimeout       Code = "script_timeout"
	CodeScriptLimitExceeded Code = "script_limit_exceeded"
	CodeInvalidScriptShape  Code = "invalid_script_shape"
	CodeInvalidURL          Code = "invalid_url"
	CodeBlockedTarget       Code = "blocked_target"

	// 兜底
	CodeNotSupported Code = "not_supported"
	CodeInternal     Code = "internal"
)

// 记账专用取值（UC-100 A1，2026-09-28 additive）——**不是**信封错误码：
// 它们只出现在用量记账的 `errorCode` 列（§8 RequestLog）与诊断面的输入里，
// **不在** All 封闭集合中（信封错误码集合不受影响，Valid() 对它们恒为 false）。
//
// 存在的理由（C-0 审计缺口）：上游响应的失败此前全部归入 gateway_unexpected_response，
// 「上游鉴权」（401/403）与「上游限流」（429）从记账里分不出来，界面与诊断也就分不出来。
// 本地失败（未到达上游，routerRecordCode 的 reached=false 分支）仍用错误码本身，
// 与这两个取值不混。
const (
	// AccountingUpstreamAuth 是上游 401/403 的记账取值（上游鉴权）。
	AccountingUpstreamAuth Code = "upstream_auth"
	// AccountingUpstreamRateLimit 是上游 429 的记账取值（上游限流）。
	AccountingUpstreamRateLimit Code = "upstream_rate_limit"
)

// All 是封闭集合，顺序与 doc/api-v1.md §1.3 的表一致。
// 契约测试用它做集合相等断言，因此这里多一个或少一个都会红。
var All = []Code{
	CodeInvalidRequest,
	CodeUnauthorized,
	CodeForbiddenOrigin,
	CodeUnauthenticated,
	CodeNotFound,

	CodeConfigChanged,
	CodeConfigInvalid,
	CodePathConflict,
	CodePermissionDenied,
	CodePreviewExpired,
	CodePreviewInUse,
	CodeBackupUnreadable,

	CodeStorage,

	CodeIntegrityFailed,

	CodeGatewayUnreachable,
	CodeGatewayUnexpectedResponse,
	CodeLoginExpired,
	CodeCancelled,

	CodeChannelNotConfigured,

	CodeTestInProgress,
	CodeQuotaExceeded,
	CodeRateLimited,

	CodeVersionConflict,
	CodeCapabilityConflict,

	CodeScriptTimeout,
	CodeScriptLimitExceeded,
	CodeInvalidScriptShape,
	CodeInvalidURL,
	CodeBlockedTarget,

	CodeNotSupported,
	CodeInternal,
}

// Valid 报告 c 是否属于封闭集合。
func (c Code) Valid() bool {
	for _, k := range All {
		if k == c {
			return true
		}
	}
	return false
}

// HTTPStatus 给出该错误码对应的 HTTP 状态。
// 语义映射刻意保守：只有确实是「没找到」「没权限」「冲突」的才是 4xx，其余一律 4xx/5xx 分明。
func (c Code) HTTPStatus() int {
	switch c {
	case CodeUnauthorized, CodeUnauthenticated:
		return 401
	case CodeForbiddenOrigin, CodePermissionDenied:
		return 403
	case CodeNotFound:
		return 404
	case CodeQuotaExceeded:
		// 限额已满：用 429 表达「现在不行，额度恢复后可以」，与 403 的「没权限」区分开。
		return 429
	case CodeRateLimited:
		// 速率超限：同 429 语义（下个自然分钟自愈），码与 quota_exceeded 分开。
		return 429
	case CodePreviewExpired, CodePreviewInUse, CodeCancelled:
		return 410
	case CodeTestInProgress:
		// 409：请求本身没问题，是「同一工具上另一次测试还在跑」这一**状态冲突**。
		// 刻意不复用 410（那是「这次引用的东西已经没了」），也不降级成 400（调用方
		// 无从区分「我传错了」与「稍后再试」）。
		return 409
	case CodeVersionConflict, CodeCapabilityConflict:
		// 409：两者都是「请求合法但被服务端的当前状态挡住」——
		//   - version_conflict：基于的版本已不是当前版本（看两版并重新提交后即可成功）；
		//   - capability_conflict：能力声明与所选模型事实不兼容且未确认（确认或修正后即可）。
		// 不用 400（不是参数错）、不用 410（引用的东西都还在）。
		return 409
	case CodeInvalidRequest, CodeConfigInvalid, CodePathConflict,
		CodeConfigChanged, CodeBackupUnreadable, CodeChannelNotConfigured,
		CodeScriptTimeout, CodeScriptLimitExceeded, CodeInvalidScriptShape,
		CodeInvalidURL, CodeBlockedTarget, CodeNotSupported, CodeIntegrityFailed:
		return 400
	case CodeLoginExpired:
		return 410
	case CodeGatewayUnreachable:
		return 502
	case CodeGatewayUnexpectedResponse:
		return 502
	case CodeStorage:
		return 507
	case CodeInternal:
		return 500
	default:
		return 500
	}
}

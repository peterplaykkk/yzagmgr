// 本文件是路由表：doc/api-v1.md 里每一条端点在这里注册一次。
//
// 纪律：
//   - 路径与契约**逐字一致**；`TestDocumentedEndpointsAllExist` 会把契约清单与注册表比对。
//   - 变更类端点一律走 mutate（= 执行 + 操作日志 + 信封），只读端点走 get。
//   - 这里不做业务判断（档位、路径解析、事务都在服务包），只做「翻译 + 包信封」。
package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/operations"
)

// routeEntry 是一条已注册的端点（方法与路径模板）。
//
// 保留这份清单是为了让契约测试能**机械地**回答「文档里的每条端点都注册了吗」——
// `http.ServeMux` 不提供列举模式的口子，而「靠人眼核对 80 行」不算契约镜像。
type routeEntry struct {
	Method  string
	Pattern string
}

// Routes 返回已注册的端点清单（顺序即注册顺序）。
func (s *Server) Routes() []routeEntry {
	out := make([]routeEntry, len(s.registered))
	copy(out, s.registered)
	return out
}

// routes 记录注册过的端点。
func (s *Server) record(method, pattern string) {
	s.registered = append(s.registered, routeEntry{Method: method, Pattern: pattern})
}

// ── 注册原语 ────────────────────────────────────────────────────────────────

// get 注册一个只读端点：调用 handler，把结果包成成功信封。
func (s *Server) get(pattern string, h Handler) {
	method, path := splitPattern(pattern)
	s.record(method, path)
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		Respond(w, r, h)
	})
}

// splitPattern 把 `GET /api/v1/tools/{id}` 切成方法与路径。
func splitPattern(pattern string) (string, string) {
	if i := strings.IndexByte(pattern, ' '); i > 0 {
		return pattern[:i], pattern[i+1:]
	}
	return "", pattern
}

// opSpec 描述一次变更在操作日志里的身份（UC-006 A7）。
type opSpec struct {
	// Action 是操作名（如 tool.apply）。小写字母起头，点分（operations 包有字符集闸门）。
	Action string
	// Target 从请求推导操作目标（工具 id）；非工具操作返回空串。
	Target func(r *http.Request) string
	// TargetFromResult extracts a safe resource identity from the handler result
	// for body-addressed operations whose request cannot be decoded twice.
	TargetFromResult func(data any) string
	// AsyncWorker routes start a background task and cannot accept a one-shot
	// client reservation. Synchronous dispatch still takes a short shared lease.
	AsyncWorker bool
	// OwnsWriterAdmission marks an installer whose handler/worker acquires its
	// shared/exclusive coordinator leases at the actual state/replacement edges.
	OwnsWriterAdmission bool
	// OperationLogAdmission takes a short shared lease around operation-log
	// persistence when a route performs network preparation before its own write.
	OperationLogAdmission bool
}

// mutationHandler 是一次变更的执行体：返回响应载荷与关联的备份编号。
type mutationHandler func(r *http.Request) (data any, backupID string, err error)

// loggedFailure 表示「响应按成功信封返回，但操作日志记 failed」。
//
// 存在的理由是一类端点的语义：执行**完成**了（HTTP 上是成功的业务响应），但结果本身是
// 「没成功」——脚本试运行返回了错误、按档取数没能取到额度。契约把 `error?` 放在成功
// 信封的载荷里，因此响应必须成功；而 UC-014 A13 要求失败在操作日志里记为失败，
// 两者只能用这个类型同时满足。
type loggedFailure struct {
	data any
	code apierr.Code
}

func (e *loggedFailure) Error() string { return string(e.code) }

// mutate 注册一个变更端点。
//
// 顺序刻意如此：**先执行、后记账**。先记 ok 再执行等于给失败盖上成功的章
// （operations 包注释里的第 1 条纪律）。
func (s *Server) mutate(pattern string, spec opSpec, h mutationHandler) {
	method, path := splitPattern(pattern)
	s.record(method, path)
	if !spec.AsyncWorker {
		s.reservableMutationPatterns[method+" "+path] = struct{}{}
	}
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		rid := w.Header().Get("X-Request-Id")
		trackedID, trackedInstance, trackedPath, tracked, err := s.validateOperationRequest(r)
		if err != nil {
			failEnvelope(err).WriteJSON(w, rid)
			return
		}
		var release func()
		if !spec.OwnsWriterAdmission {
			if ctxErr := r.Context().Err(); ctxErr != nil {
				err = apierr.New(apierr.CodeCancelled, "请求在写入 admission 前已取消，未执行操作")
			} else {
				release, err = s.writeCoordinator.AcquireShared(r.Context(), s.operationInstanceID)
				if err != nil {
					err = coordinatorAPIError(err)
				}
			}
		}
		releaseWriter := func() {
			if release != nil {
				finish := release
				release = nil
				finish()
			}
		}
		defer releaseWriter()
		if err != nil {
			if tracked {
				// A reserved call that did not win writer admission remains reserved;
				// the caller can close it or retry while this same instance is valid.
				failEnvelope(err).WriteJSON(w, rid)
				return
			}
			s.recordOperationWithAdmission(spec, r, rid, "", errorCodeOf(err), nil)
			releaseWriter()
			failEnvelope(err).WriteJSON(w, rid)
			return
		}
		if tracked {
			if ctxErr := r.Context().Err(); ctxErr != nil {
				releaseWriter()
				failEnvelope(apierr.New(apierr.CodeCancelled, "请求在写入 admission 前已取消，未执行操作")).WriteJSON(w, rid)
				return
			}
			status, consumeErr := s.operationRequests.Consume(r.Context(), trackedID, trackedInstance, r.Method, trackedPath)
			if consumeErr != nil {
				releaseWriter()
				failEnvelope(consumeErr).WriteJSON(w, rid)
				return
			}
			rid = status.RequestID
			w.Header().Set("X-Request-Id", rid)
		}
		operationLogged := false
		defer func() {
			if recovered := recover(); recovered != nil {
				if tracked && !operationLogged {
					operationID := s.recordOperationAdmitted(spec, r, rid, "", apierr.CodeInternal, nil)
					s.operationRequests.Complete(rid, http.StatusInternalServerError, string(apierr.CodeInternal), operationID)
				}
				panic(recovered)
			}
		}()

		var data any
		var backupID string
		data, backupID, err = h(r)
		var logged *loggedFailure
		if errors.As(err, &logged) {
			operationID := s.recordOperationAdmitted(spec, r, rid, backupID, logged.code, logged.data)
			operationLogged = true
			if tracked {
				s.operationRequests.Complete(rid, http.StatusOK, "", operationID)
			}
			releaseWriter()
			apierr.OK(logged.data).WriteJSON(w, rid)
			return
		}
		operationID := s.recordOperationAdmitted(spec, r, rid, backupID, errorCodeOf(err), data)
		operationLogged = true
		if tracked {
			errorCode := ""
			if err != nil {
				errorCode = string(apierr.From(err).Code)
			}
			s.operationRequests.Complete(rid, operationRequestHTTPStatus(err), errorCode, operationID)
		}
		releaseWriter()
		if err != nil {
			// 失败出口与 get 通道共用 failEnvelope（UC-014 A11 的掩码收口点）。
			failEnvelope(err).WriteJSON(w, rid)
			return
		}
		apierr.OK(data).WriteJSON(w, rid)
	})
}

func (s *Server) recordOperationAdmitted(spec opSpec, r *http.Request, requestID, backupID string, failure apierr.Code, data any) string {
	if !spec.OperationLogAdmission && !spec.OwnsWriterAdmission {
		return s.recordOperation(spec, r, requestID, backupID, failure, data)
	}
	return s.recordOperationWithAdmission(spec, r, requestID, backupID, failure, data)
}

func (s *Server) recordOperationWithAdmission(spec opSpec, r *http.Request, requestID, backupID string, failure apierr.Code, data any) string {
	var operationID string
	if s.writeCoordinator.withHeldExclusive(func() {
		// A restart-owning handler may return while intentionally retaining EX
		// through old-process exit; keep its lease live for the whole append.
		operationID = s.recordOperation(spec, r, requestID, backupID, failure, data)
	}) {
		return operationID
	}
	release, err := s.acquireOperationLogAdmission()
	if err != nil {
		s.log.Error("操作日志写入缺少当前代际 admission", "action", spec.Action, "requestId", requestID, "err", err)
		return ""
	}
	defer release()
	return s.recordOperation(spec, r, requestID, backupID, failure, data)
}

// taskAdmissionRetryInterval keeps workers behind a short exclusive install
// freeze retrying instead of failing the reservation（原定义在 tasks.go，
// 下载/任务面删除后随唯一消费点迁至本文件——design §2.2 §11 十路由删除）。
const taskAdmissionRetryInterval = 25 * time.Millisecond

func (s *Server) acquireOperationLogAdmission() (func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), operationStartupFenceTimeout)
	defer cancel()
	return s.acquireOperationLogAdmissionWithContext(ctx)
}

func (s *Server) acquireOperationLogAdmissionWithContext(ctx context.Context) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		release, err := s.writeCoordinator.AcquireShared(ctx, s.operationInstanceID)
		if !errors.Is(err, errWriteFrozen) {
			return release, err
		}
		timer := time.NewTimer(taskAdmissionRetryInterval)
		select {
		case <-ctx.Done():
			stopAndDrainOperationTimer(timer.Stop, timer.C)
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// recordOperation 写一条操作日志。result 如实取自执行结果（成功 ok / 失败 failed + 错误码）。
//
// 记账失败时**不**改写已经发生的执行结果：把一次成功的变更报成失败会造成
// 「盘上已改、界面说没改」，比少一条审计更危险。记账失败因此只大声记进程日志。
func (s *Server) recordOperation(spec opSpec, r *http.Request, requestID, backupID string, failure apierr.Code, data any) string {
	target := ""
	if spec.Target != nil {
		target = spec.Target(r)
	} else if spec.TargetFromResult != nil {
		target = spec.TargetFromResult(data)
	}
	op := operations.Operation{
		Action:    spec.Action,
		Target:    target,
		Result:    operations.ResultOK,
		BackupID:  backupID,
		RequestID: requestID,
	}
	if failure != "" {
		op.Result = operations.ResultFailed
		op.ErrorCode = string(failure)
	}
	stored, aerr := s.Ops.Append(op)
	if aerr != nil {
		s.log.Error("操作日志写入失败（执行结果已生效，不改为失败）",
			"action", spec.Action, "target", target, "requestId", requestID, "err", aerr)
		return ""
	}
	return stored.ID
}

// control registers a lifecycle-control endpoint. It uses the normal security
// middleware and response envelope but deliberately does not append an ordinary
// business operation record.
func (s *Server) control(pattern string, h Handler) {
	method, path := splitPattern(pattern)
	s.record(method, path)
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		Respond(w, r, h)
	})
}

// errorCodeOf 把执行错误归一成错误码；nil 返回空串（成功）。
func errorCodeOf(err error) apierr.Code {
	if err == nil {
		return ""
	}
	return apierr.From(err).Code
}

// ── 路由表 ──────────────────────────────────────────────────────────────────

// registerRoutes 注册 doc/api-v1.md 的全部端点。
//
// 端点计数（design §1.1 清理后）：133 − 40 删 = **93**（§1.4 操作日志 5 + §2 健康与版本 2 +
// §3 策略 2 + §4.1 工具 3 + §4.2 档位卡 6 + §4.3 预览与应用 8 + §4.4 配置物化 2 +
// §4.6 配置内容与测试/草稿 6 + §4.7 托管凭据 6 + §4.8 运行时证据 3 + §5 备份 2 +
// §7 本地代理 4（status/config/start/stop，E2）+ §7.1 本地 router 多臂 13 + §8 用量 11 +
// §9 环境变量冲突 5 + §10 用量脚本 5 + §16 本机诊断 4 + §17 MCP 配置 6）。
// 删除面 = §3 登录/账户/令牌 12 + §4.5 下发 2 + §6 WorkBuddy 10 + §11 下载/任务/自更新 10 +
// §12 状态栏 3 + proxy routes 2 + ensure-gateway-profiles 1（最终以 contract_test 机械核对为准）。
//
// （原「UC-034 包装」三端点已恢复直连原 handler：delivery.go 删除后包装即死，
// 见 design §2.2 router.go 行——审查 #11。）
func (s *Server) registerRoutes() {
	const v1 = "/api/v1"

	// ── §1.4 操作日志 ──
	s.get("GET "+v1+"/operations", s.handleOperationsList)
	s.get("GET "+v1+"/operations/{id}", s.handleOperationGet)
	// UC-098 request lifecycle control APIs deliberately do not create ordinary
	// operation-log rows. Only the consumed business request gets one terminal row.
	s.control("POST "+v1+"/operations/requests", s.handleOperationRequestReserve)
	s.get("GET "+v1+"/operations/requests/{requestId}", s.handleOperationRequestStatus)
	s.control("POST "+v1+"/operations/requests/{requestId}/close", s.handleOperationRequestClose)

	// ── §2 健康与版本（免鉴权，见 Guard 白名单） ──
	s.get("GET "+v1+"/health", s.handleHealth)
	s.get("GET "+v1+"/version", s.handleVersion)

	// ── §3 策略（登录/账户/令牌 12 路由已删，design §1.1） ──
	s.get("GET "+v1+"/policy", s.handlePolicy)
	s.mutate("POST "+v1+"/policy/refresh", opSpec{Action: "policy.refresh"}, s.handlePolicyRefresh)

	// ── §4.1 工具 ──
	s.get("GET "+v1+"/tools", s.handleTools)
	s.get("GET "+v1+"/tools/{id}", s.handleToolGet)
	s.get("GET "+v1+"/tools/{id}/install-copies", s.handleInstallCopies)

	// ── §4.2 档位卡 ──
	s.get("GET "+v1+"/tools/{id}/profiles", s.handleProfilesList)
	s.mutate("POST "+v1+"/tools/{id}/profiles", opSpec{Action: "profile.create", Target: toolIDOf}, s.handleProfileCreate)
	s.mutate("PUT "+v1+"/tools/{id}/profiles/order", opSpec{Action: "profile.reorder", Target: toolIDOf}, s.handleProfileOrder)
	s.mutate("PUT "+v1+"/tools/{id}/profiles/{pid}", opSpec{Action: "profile.update", Target: toolIDOf}, s.handleProfileUpdate)
	s.mutate("DELETE "+v1+"/tools/{id}/profiles/{pid}", opSpec{Action: "profile.delete", Target: toolIDOf}, s.handleProfileDelete)
	s.mutate("POST "+v1+"/tools/{id}/profiles/{pid}/duplicate",
		opSpec{Action: "profile.duplicate", Target: toolIDOf}, s.handleProfileDuplicate)

	// ── §4.3 预览与应用 ──
	s.mutate("POST "+v1+"/tools/{id}/preview", opSpec{Action: "tool.preview", Target: toolIDOf}, s.handleToolPreview)
	s.mutate("POST "+v1+"/tools/{id}/apply", opSpec{Action: "tool.apply", Target: toolIDOf}, s.handleToolApply)
	s.mutate("POST "+v1+"/tools/{id}/restore/preview",
		opSpec{Action: "tool.restore.preview", Target: toolIDOf}, s.handleToolRestorePreview)
	s.mutate("POST "+v1+"/tools/{id}/restore/apply",
		opSpec{Action: "tool.restore.apply", Target: toolIDOf}, s.handleToolRestoreApply)
	// UC-003 修订 additive（2026-09-27）：带快照的重置（非法配置恢复面）。预览走
	// mutate 与 preview/restore 同口径（建租约属于「变更类」的执行前段），应用走同一个
	// applyLease。运行时永远只作用于路径里的这一个工具（禁全局重置）。
	s.mutate("POST "+v1+"/tools/{id}/reset/preview",
		opSpec{Action: "tool.reset.preview", Target: toolIDOf}, s.handleToolResetPreview)
	s.mutate("POST "+v1+"/tools/{id}/reset/apply",
		opSpec{Action: "tool.reset.apply", Target: toolIDOf}, s.handleToolResetApply)
	s.get("GET "+v1+"/tools/{id}/backups", s.handleToolBackups)
	s.mutate("POST "+v1+"/tools/{id}/probe", opSpec{Action: "tool.probe", Target: toolIDOf}, s.handleToolProbe)

	// ── §4.4 配置物化（UC-019）──
	// 物化是变更类端点（它会写 `<Home>/.yzagmgr/materialize/**`），因此必须走 mutate 落操作日志；
	// GET 是只读盘点（不写任何字节），走 get。
	s.get("GET "+v1+"/materialize", s.handleMaterializeList)
	s.mutate("POST "+v1+"/materialize", opSpec{Action: "materialize.run"}, s.handleMaterializeRun)

	// ── §4.6 配置内容与测试（UC-039/097）及草稿 assessment（UC-112，additive）──
	// content 是**纯读**（不触盘、不建租约）⇒ get；test 真的跑一次 CLI ⇒ mutate
	// （操作日志要能回答「谁在什么时候测过哪张卡」，与 §10 的试运行同口径）。
	// UC-097 A4 additive（2026-09-27）：test/cancel 是停止入口——它终止一次在飞的真实
	// 运行（进程组回收、终态如实标 cancelled），属变更类 ⇒ mutate 落操作日志。
	s.get("GET "+v1+"/tools/{id}/profiles/{pid}/content", s.handleProfileContent)
	s.mutate("POST "+v1+"/tools/{id}/profiles/{pid}/test",
		opSpec{Action: "profile.test", Target: toolIDOf}, s.handleProfileTest)
	s.mutate("POST "+v1+"/tools/{id}/profiles/{pid}/test/cancel",
		opSpec{Action: "profile.test.cancel", Target: toolIDOf}, s.handleProfileTestCancel)
	// 草稿预览是有请求体的纯计算（同 POST /routes/explain）且不落操作日志；
	// 测试/取消只记录通用操作元数据，不记录 profileId、draftFingerprint 或内容。
	s.get("POST "+v1+"/tools/{id}/profiles/draft/preview", s.handleProfileDraftPreview)
	s.mutate("POST "+v1+"/tools/{id}/profiles/draft/test",
		opSpec{Action: "profile.draft.test", Target: toolIDOf}, s.handleProfileDraftTest)
	s.mutate("POST "+v1+"/tools/{id}/profiles/draft/test/cancel",
		opSpec{Action: "profile.draft.test.cancel", Target: toolIDOf}, s.handleProfileDraftTestCancel)

	// ── §4.7 托管凭据（UC-094 A9，additive）──
	// 列出/读取是只读面（get）；创建收一次性明文入托管凭据库 ⇒ mutate（操作日志要能
	// 回答「谁在什么时候录入了哪枚凭据」——**日志与响应都不含明文**，只有引用与名称）。
	s.get("GET "+v1+"/credentials", s.handleCredentialsList)
	s.mutate("POST "+v1+"/credentials", opSpec{Action: "credential.create"}, s.handleCredentialCreate)
	s.get("GET "+v1+"/credentials/{uid}", s.handleCredentialGet)
	// UC-094 A10 additive（2026-09-27）：轮换/撤销是变更类（操作日志要能回答
	// 「谁在什么时候轮换/撤销了哪枚凭据」——日志只记引用与结果，不含明文）；
	// references 是只读影响面（「查看受影响配置后确认」的查看面，零副作用）。
	s.mutate("POST "+v1+"/credentials/{uid}/rotate",
		opSpec{Action: "credential.rotate"}, s.handleCredentialRotate)
	s.mutate("POST "+v1+"/credentials/{uid}/revoke",
		opSpec{Action: "credential.revoke"}, s.handleCredentialRevoke)
	s.get("GET "+v1+"/credentials/{uid}/references", s.handleCredentialReferences)

	// ── §4.8 运行时证据与受控验证（UC-092 A9/A10/A11，additive）──
	// 读取是只读面（状态在服务端读取时刻判定）；受控验证是两步的变更类端点
	// （preview 建一次性租约、apply 会真的跑一次 CLI 并发起一次模型调用 ⇒ 都走
	// mutate 落操作日志：「谁在什么时候对哪个工具发起过受控验证」必须可审计）。
	s.get("GET "+v1+"/tools/{id}/runtime-evidence", s.handleRuntimeEvidence)
	s.mutate("POST "+v1+"/tools/{id}/runtime-evidence/verify/preview",
		opSpec{Action: "runtime-evidence.verify.preview", Target: toolIDOf}, s.handleRuntimeVerifyPreview)
	s.mutate("POST "+v1+"/tools/{id}/runtime-evidence/verify/apply",
		opSpec{Action: "runtime-evidence.verify.apply", Target: toolIDOf}, s.handleRuntimeVerifyApply)

	// ── §5 备份保留 ──
	s.get("GET "+v1+"/backups/settings", s.handleBackupSettings)
	s.mutate("PUT "+v1+"/backups/settings", opSpec{Action: "backup.settings.save"}, s.handleBackupSettingsSave)

	// ── §17 MCP 配置（UC-111，独立写链） ──
	s.get("GET "+v1+"/mcp/servers", s.handleMcpServersList)
	s.mutate("PUT "+v1+"/mcp/servers/{name}",
		opSpec{Action: "mcp.servers.save", Target: mcpServerNameTarget}, s.handleMcpServerSave)
	s.mutate("DELETE "+v1+"/mcp/servers/{name}",
		opSpec{Action: "mcp.servers.remove", Target: mcpServerNameTarget}, s.handleMcpServerDelete)
	s.get("GET "+v1+"/mcp/tools", s.handleMcpToolsStatus)
	// preview/apply 的工具身份在请求体里（Target 在执行后调用、body 不可重读），
	// 与 routes.add 同款处理：target 留空串。
	s.mutate("POST "+v1+"/mcp/preview", opSpec{Action: "mcp.preview"}, s.handleMcpPreview)
	s.mutate("POST "+v1+"/mcp/apply", opSpec{Action: "mcp.apply"}, s.handleMcpApply)

	// ── §7 本地代理（管理面） ──
	s.get("GET "+v1+"/proxy/status", s.handleProxyStatus)
	s.mutate("PUT "+v1+"/proxy/config", opSpec{Action: "proxy.config.save"}, s.handleProxyConfigSave)
	s.mutate("POST "+v1+"/proxy/start", opSpec{Action: "proxy.start"}, s.handleProxyStart)
	s.mutate("POST "+v1+"/proxy/stop", opSpec{Action: "proxy.stop"}, s.handleProxyStop)

	// ── §7.1 本地 router 多臂（UC-023/028/029） ──
	s.get("GET "+v1+"/routes", s.handleRoutesList)
	s.get("GET "+v1+"/routes/{name}", s.handleRoutesGet)
	// UC-043 additive：选择器枚举（只读）。字面量路径比 `/routes/{name}` 更具体，
	// Go 1.22+ 的 ServeMux 按**特异性**而不是注册顺序选路 ⇒ 本行不会被通配行抢走
	// （代价：真叫 `names` 的请求映射名在 GET /routes/{name} 上会被本行遮蔽）。
	s.get("GET "+v1+"/routes/names", s.handleRoutesNames)
	// UC-096 additive：解释预览（纯计算：不联网/不读 live/不触盘/不建租约）。
	// 只读端点走 get（不落操作日志——预览可被反复试输入，记账面不该被它淹掉）；
	// POST 只因输入是带结构体的请求体（草稿规则集可能很大）。
	s.get("POST "+v1+"/routes/explain", s.handleRoutesExplain)
	s.mutate("POST "+v1+"/routes", opSpec{Action: "routes.add"}, s.handleRoutesAdd)
	s.mutate("PUT "+v1+"/routes/{name}", opSpec{Action: "routes.set", Target: routerRouteNameTarget}, s.handleRoutesSet)
	s.mutate("DELETE "+v1+"/routes/{name}", opSpec{Action: "routes.remove", Target: routerRouteNameTarget}, s.handleRoutesRemove)
	s.get("GET "+v1+"/remap/model/{arm}", s.handleRemapModelList)
	s.mutate("PUT "+v1+"/remap/model/{arm}",
		opSpec{Action: "remap.model.set", Target: routerArmTarget}, s.handleRemapModelSet)
	s.mutate("DELETE "+v1+"/remap/model/{arm}/{alias}",
		opSpec{Action: "remap.model.delete", Target: routerArmTarget}, s.handleRemapModelDelete)
	s.get("GET "+v1+"/remap/effort/{arm}", s.handleRemapEffortList)
	s.mutate("PUT "+v1+"/remap/effort/{arm}",
		opSpec{Action: "remap.effort.set", Target: routerArmTarget}, s.handleRemapEffortSet)
	s.mutate("PUT "+v1+"/remap/effort/{arm}/force",
		opSpec{Action: "remap.effort.force", Target: routerArmTarget}, s.handleRemapEffortForce)

	// ── §8 用量 ──
	s.get("GET "+v1+"/usage/summary", s.handleUsageSummary)
	s.get("GET "+v1+"/usage/trends", s.handleUsageTrends)
	s.get("GET "+v1+"/usage/breakdown", s.handleUsageBreakdown)
	s.get("GET "+v1+"/usage/requests", s.handleUsageRequests)
	s.get("GET "+v1+"/usage/requests/{id}", s.handleUsageRequestGet)
	s.get("GET "+v1+"/usage/pricing", s.handleUsagePricing)
	s.mutate("PUT "+v1+"/usage/pricing", opSpec{Action: "usage.pricing.save"}, s.handleUsagePricingSave)
	// UC-048 additive：本地监控数据上限与维护。PUT 改了设置、POST 会删旧行，
	// 都是变更类端点 ⇒ 走 mutate 落操作日志；GET 是纯读。
	s.get("GET "+v1+"/usage/limits", s.handleUsageLimits)
	s.mutate("PUT "+v1+"/usage/limits", opSpec{Action: "usage.limits.save"}, s.handleUsageLimitsSave)
	s.mutate("POST "+v1+"/usage/maintenance", opSpec{Action: "usage.maintenance"}, s.handleUsageMaintenance)

	// ── §8 四能力扩展面（design-b §6.1，IP1 接线合入）──
	// 每用户汇总 / 计费流水与账单 / 计价规则整表 / 速率限制 / 每臂配额 / 上游注册表。
	// 读面走 get（不落操作日志），写面走 mutate（action 词表：usage.pricing.rules.save、
	// rate_limit.save、arm_quota.save、providers.save）。
	s.get("GET "+v1+"/usage/users", s.handleUsageUsers)
	s.get("GET "+v1+"/usage/ledger", s.handleUsageLedger)
	s.get("GET "+v1+"/usage/statement/{slug}", s.handleUsageStatement)
	s.get("GET "+v1+"/usage/pricing/rules", s.handleUsagePricingRulesGet)
	s.mutate("PUT "+v1+"/usage/pricing/rules",
		opSpec{Action: "usage.pricing.rules.save"}, s.handleUsagePricingRulesSave)
	s.get("GET "+v1+"/usage/rate-limits", s.handleUsageRateLimitsList)
	s.mutate("PUT "+v1+"/usage/rate-limits/{slug}",
		opSpec{Action: "rate_limit.save", Target: routerRouteNameTarget}, s.handleUsageRateLimitSave)
	s.get("GET "+v1+"/usage/arm-quota", s.handleUsageArmQuotaGet)
	s.mutate("PUT "+v1+"/usage/arm-quota", opSpec{Action: "arm_quota.save"}, s.handleUsageArmQuotaSave)
	s.get("GET "+v1+"/providers", s.handleProvidersGet)
	s.mutate("PUT "+v1+"/providers", opSpec{Action: "providers.save"}, s.handleProvidersSave)

	// UC-049 additive：系统信息页（数据占用 / 表行数 / 查询延时参数）。只读：不触发维护。
	s.get("GET "+v1+"/system/info", s.handleSystemInfo)

	// ── §9 环境变量冲突 ──
	s.get("GET "+v1+"/env/conflicts", s.handleEnvConflicts)
	s.mutate("POST "+v1+"/env/scan", opSpec{Action: "env.scan"}, s.handleEnvScan)
	s.mutate("POST "+v1+"/env/repair/preview", opSpec{Action: "env.repair.preview"}, s.handleEnvRepairPreview)
	s.mutate("POST "+v1+"/env/repair/apply", opSpec{Action: "env.repair.apply"}, s.handleEnvRepairApply)
	s.mutate("POST "+v1+"/env/repair/restore", opSpec{Action: "env.repair.restore"}, s.handleEnvRepairRestore)

	// ── §10 用量脚本 ──
	s.get("GET "+v1+"/tools/{id}/profiles/{pid}/usage-script", s.handleUsageScriptGet)
	s.mutate("PUT "+v1+"/tools/{id}/profiles/{pid}/usage-script",
		opSpec{Action: "usage-script.save", Target: toolIDOf}, s.handleUsageScriptSave)
	s.mutate("DELETE "+v1+"/tools/{id}/profiles/{pid}/usage-script",
		opSpec{Action: "usage-script.delete", Target: toolIDOf}, s.handleUsageScriptDelete)
	s.mutate("POST "+v1+"/tools/{id}/profiles/{pid}/usage-script/test",
		opSpec{Action: "usage-script.test", Target: toolIDOf}, s.handleUsageScriptTest)
	s.mutate("POST "+v1+"/tools/{id}/profiles/{pid}/usage",
		opSpec{Action: "usage-script.fetch", Target: toolIDOf}, s.handleProfileUsage)

	// ── §16 本机诊断（UC-100 additive，2026-09-28）──
	// 全部只读：探测（probe）不写配置/凭据、不装依赖、不起子进程（A2），因此走 get
	// （不落操作日志——检测可被反复发起，记账面不该被它淹掉）；bundle 是脱敏复制素材；
	// fix/preview 只出计划（执行发生在既有 preview/apply 事务端点，禁自动修复）。
	s.get("GET "+v1+"/diagnostics", s.handleDiagnostics)
	s.get("POST "+v1+"/diagnostics/probe", s.handleDiagnosticsProbe)
	s.get("GET "+v1+"/diagnostics/bundle", s.handleDiagnosticsBundle)
	s.get("POST "+v1+"/diagnostics/fix/preview", s.handleDiagnosticsFixPreview)
}

// toolIDOf 取路径里的工具 id（操作日志的 target）。
func toolIDOf(r *http.Request) string { return r.PathValue("id") }

// routerArmTarget 取路径里的臂名（操作日志的 target）。
func routerArmTarget(r *http.Request) string { return r.PathValue("arm") }

// routerRouteNameTarget 取路径里的臂名（POST /routes 的臂名在请求体里、已被 handler
// 读走，故 routes.add 不设 Target——mutate 的 Target 在执行后调用，body 不可重读）。
func routerRouteNameTarget(r *http.Request) string { return r.PathValue("name") }

// mcpServerNameTarget 取路径里的注册名（操作日志的 target）。注册名限定小写
// slug 形态（mcp.ValidateName），与 operations.safeTarget 字符集一致。
func mcpServerNameTarget(r *http.Request) string { return r.PathValue("name") }

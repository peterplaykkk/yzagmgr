package api

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/operations"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

const (
	operationRequestReservationTTL = 60 * time.Second
	operationRequestTerminalTTL    = 10 * time.Minute
	operationRequestMaxEntries     = 256
	operationRequestPrepareTimeout = 10 * time.Second
	operationIDHeader              = "X-Agmgr-Operation-Id"
	operationInstanceHeader        = "X-Agmgr-Operation-Instance"
)

const (
	operationRequestReserved      = "reserved"
	operationRequestRunning       = "running"
	operationRequestCompleted     = "completed"
	operationRequestClosed        = "closed"
	operationRequestFencedUnknown = "fenced_unknown"
	operationRequestUnknown       = "unknown"
)

type operationRequestReserveBody struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

type operationRequestCloseBody struct {
	InstanceID string `json:"instanceId"`
}

// operationRequestStatus is deliberately metadata-only. Request/response
// bodies, config bytes, credentials, fingerprints and test proof never enter
// the in-memory lifecycle ledger or runtime generation files.
type operationRequestStatus struct {
	RequestID   string `json:"requestId"`
	InstanceID  string `json:"instanceId"`
	State       string `json:"state"`
	Method      string `json:"method,omitempty"`
	Path        string `json:"path,omitempty"`
	ExpiresAt   string `json:"expiresAt,omitempty"`
	StartedAt   string `json:"startedAt,omitempty"`
	FinishedAt  string `json:"finishedAt,omitempty"`
	HTTPStatus  int    `json:"httpStatus,omitempty"`
	ErrorCode   string `json:"errorCode,omitempty"`
	OperationID string `json:"operationId,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

type operationRequestEntry struct {
	status     operationRequestStatus
	expiresAt  time.Time
	finishedAt time.Time
}

type operationRequestLedger struct {
	mu         sync.Mutex
	instanceID string
	now        func() time.Time
	seq        uint64
	entries    map[string]*operationRequestEntry
}

func newOperationRequestLedger(instanceID string, now func() time.Time) *operationRequestLedger {
	if now == nil {
		now = time.Now
	}
	return &operationRequestLedger{
		instanceID: instanceID,
		now:        now,
		entries:    make(map[string]*operationRequestEntry),
	}
}

func (l *operationRequestLedger) Reserve(method, requestPath string) (operationRequestStatus, error) {
	if err := validateOperationRequestPath(method, requestPath); err != nil {
		return operationRequestStatus{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now().UTC()
	l.pruneLocked(now)
	if len(l.entries) >= operationRequestMaxEntries {
		return operationRequestStatus{}, apierr.New(apierr.CodeInvalidRequest,
			"在途操作预留已满；等待现有请求终态或过期后重试")
	}
	l.seq++
	// The random per-daemon instance segment, generation, and monotonic sequence
	// make IDs non-reusable within this process even after terminal entries prune.
	instancePart := strings.TrimPrefix(l.instanceID, "inst_")
	requestID := fmt.Sprintf("opreq_%s_%d", instancePart, l.seq)
	expires := now.Add(operationRequestReservationTTL)
	status := operationRequestStatus{
		RequestID:  requestID,
		InstanceID: l.instanceID,
		State:      operationRequestReserved,
		Method:     method,
		Path:       requestPath,
		ExpiresAt:  operationTime(expires),
	}
	l.entries[requestID] = &operationRequestEntry{status: status, expiresAt: expires}
	return status, nil
}

func (l *operationRequestLedger) Consume(ctx context.Context, requestID, instanceID, method, requestPath string) (operationRequestStatus, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now().UTC()
	l.pruneLocked(now)
	if err := ctx.Err(); err != nil {
		return operationRequestStatus{}, apierr.New(apierr.CodeCancelled, "请求在写入 admission 前已取消，未执行操作")
	}
	if instanceID != l.instanceID {
		return operationRequestStatus{}, apierr.New(apierr.CodeInvalidRequest,
			"操作预留不属于当前服务实例")
	}
	entry, ok := l.entries[requestID]
	if !ok {
		return operationRequestStatus{}, apierr.New(apierr.CodeInvalidRequest,
			"操作预留不存在、已关闭或已过期")
	}
	if entry.status.State != operationRequestReserved {
		return operationRequestStatus{}, apierr.New(apierr.CodeInvalidRequest,
			"操作预留已被消费或封闭")
	}
	if entry.status.Method != method || entry.status.Path != requestPath {
		return operationRequestStatus{}, apierr.New(apierr.CodeInvalidRequest,
			"实际请求与操作预留的 method/path 不匹配")
	}
	entry.status.State = operationRequestRunning
	entry.status.StartedAt = operationTime(now)
	return entry.status, nil
}

func (l *operationRequestLedger) Status(
	requestID, instanceID string,
	terminalLookup func(string) (operations.Operation, bool),
	fencedOldInstance bool,
) operationRequestStatus {
	if !operationRequestIDBelongsToInstance(requestID, instanceID) {
		return operationRequestStatus{RequestID: requestID, InstanceID: instanceID, State: operationRequestUnknown}
	}
	l.mu.Lock()
	now := l.now().UTC()
	l.pruneLocked(now)
	if instanceID == l.instanceID {
		if entry, ok := l.entries[requestID]; ok {
			status := entry.status
			l.mu.Unlock()
			if fencedOldInstance && status.State == operationRequestReserved {
				return operationRequestStatus{
					RequestID:  requestID,
					InstanceID: instanceID,
					State:      operationRequestFencedUnknown,
					Reason:     "instance_replaced",
				}
			}
			return status
		}
	}
	l.mu.Unlock()

	if terminalLookup != nil {
		if operation, ok := terminalLookup(requestID); ok {
			return operationRequestCompletedStatus(requestID, instanceID, operation)
		}
	}
	if fencedOldInstance {
		return operationRequestStatus{
			RequestID:  requestID,
			InstanceID: instanceID,
			State:      operationRequestFencedUnknown,
			Reason:     "instance_replaced",
		}
	}
	return operationRequestStatus{
		RequestID:  requestID,
		InstanceID: instanceID,
		State:      operationRequestUnknown,
	}
}

func (l *operationRequestLedger) Close(
	requestID, instanceID string,
	terminalLookup func(string) (operations.Operation, bool),
	fencedOldInstance bool,
) operationRequestStatus {
	if !operationRequestIDBelongsToInstance(requestID, instanceID) {
		return operationRequestStatus{RequestID: requestID, InstanceID: instanceID, State: operationRequestUnknown}
	}
	l.mu.Lock()
	now := l.now().UTC()
	l.pruneLocked(now)
	if instanceID == l.instanceID {
		if entry, ok := l.entries[requestID]; ok {
			switch entry.status.State {
			case operationRequestReserved:
				entry.status.State = operationRequestClosed
				entry.status.Reason = "not_started"
				entry.status.FinishedAt = operationTime(now)
				entry.finishedAt = now
			case operationRequestRunning, operationRequestCompleted, operationRequestClosed:
				// Close is a fence for unstarted work, not cancellation for running work.
			}
			status := entry.status
			l.mu.Unlock()
			return status
		}
	}
	l.mu.Unlock()

	if terminalLookup != nil {
		if operation, ok := terminalLookup(requestID); ok {
			return operationRequestCompletedStatus(requestID, instanceID, operation)
		}
	}
	if fencedOldInstance {
		return operationRequestStatus{
			RequestID:  requestID,
			InstanceID: instanceID,
			State:      operationRequestFencedUnknown,
			Reason:     "instance_replaced",
		}
	}
	if instanceID == l.instanceID {
		return operationRequestStatus{
			RequestID:  requestID,
			InstanceID: instanceID,
			State:      operationRequestFencedUnknown,
			Reason:     "not_claimable",
		}
	}
	return operationRequestStatus{
		RequestID:  requestID,
		InstanceID: instanceID,
		State:      operationRequestUnknown,
	}
}

func (l *operationRequestLedger) Complete(requestID string, httpStatus int, errorCode, operationID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.entries[requestID]
	if !ok || entry.status.State != operationRequestRunning {
		return
	}
	now := l.now().UTC()
	entry.status.State = operationRequestCompleted
	entry.status.FinishedAt = operationTime(now)
	entry.status.HTTPStatus = httpStatus
	entry.status.ErrorCode = errorCode
	entry.status.OperationID = operationID
	entry.finishedAt = now
}

func (l *operationRequestLedger) Outstanding() []operationRequestStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(l.now().UTC())
	out := make([]operationRequestStatus, 0)
	for _, entry := range l.entries {
		if entry.status.State == operationRequestReserved || entry.status.State == operationRequestRunning {
			out = append(out, entry.status)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ExpiresAt != out[j].ExpiresAt {
			return out[i].ExpiresAt < out[j].ExpiresAt
		}
		return out[i].RequestID < out[j].RequestID
	})
	return out
}

func (l *operationRequestLedger) HasOutstanding() bool {
	return len(l.Outstanding()) > 0
}

func (l *operationRequestLedger) pruneLocked(now time.Time) {
	for requestID, entry := range l.entries {
		switch entry.status.State {
		case operationRequestReserved:
			if now.After(entry.expiresAt) || now.Equal(entry.expiresAt) {
				entry.status.State = operationRequestClosed
				entry.status.Reason = "expired_unclaimed"
				entry.status.FinishedAt = operationTime(now)
				entry.finishedAt = now
			}
		case operationRequestCompleted, operationRequestClosed, operationRequestFencedUnknown:
			if !entry.finishedAt.IsZero() && now.Sub(entry.finishedAt) >= operationRequestTerminalTTL {
				delete(l.entries, requestID)
			}
		}
	}
}

func operationRequestCompletedStatus(requestID, instanceID string, operation operations.Operation) operationRequestStatus {
	status := operationRequestStatus{
		RequestID:   requestID,
		InstanceID:  instanceID,
		State:       operationRequestCompleted,
		FinishedAt:  operationTime(operation.At),
		ErrorCode:   operation.ErrorCode,
		OperationID: operation.ID,
	}
	return status
}

func operationTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func validateOperationRequestPath(method, requestPath string) error {
	if method != http.MethodPost && method != http.MethodPut && method != http.MethodDelete {
		return apierr.New(apierr.CodeInvalidRequest, "操作预留只接受 POST/PUT/DELETE")
	}
	if requestPath == "" || !strings.HasPrefix(requestPath, "/") ||
		strings.HasPrefix(requestPath, "//") || strings.ContainsAny(requestPath, "?#\\\x00") {
		return apierr.New(apierr.CodeInvalidRequest, "path 必须是规范的站内绝对路径")
	}
	parsed, err := url.ParseRequestURI(requestPath)
	if err != nil || parsed.IsAbs() || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != requestPath {
		return apierr.New(apierr.CodeInvalidRequest, "path 不得包含 URL、query、hash 或转义路径段")
	}
	if path.Clean(requestPath) != requestPath || strings.Contains(requestPath, "/./") || strings.Contains(requestPath, "/../") {
		return apierr.New(apierr.CodeInvalidRequest, "path 不得包含路径穿越或非规范分隔符")
	}
	return nil
}

func validOperationRequestID(value string) bool {
	if !strings.HasPrefix(value, "opreq_") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(value, "opreq_"), "_")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return false
	}
	if _, err := hex.DecodeString(parts[0]); err != nil {
		return false
	}
	for _, segment := range parts[1:] {
		for _, char := range segment {
			if char < '0' || char > '9' {
				return false
			}
		}
	}
	return true
}

func validOperationInstanceID(value string) bool {
	if !strings.HasPrefix(value, "inst_") {
		return false
	}
	encoded := strings.TrimPrefix(value, "inst_")
	separator := strings.LastIndexByte(encoded, '_')
	if separator <= 0 || separator == len(encoded)-1 {
		return false
	}
	if _, err := hex.DecodeString(encoded[:separator]); err != nil {
		return false
	}
	for _, char := range encoded[separator+1:] {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func operationRequestIDBelongsToInstance(requestID, instanceID string) bool {
	if !validOperationRequestID(requestID) || !validOperationInstanceID(instanceID) {
		return false
	}
	requestParts := strings.Split(strings.TrimPrefix(requestID, "opreq_"), "_")
	instanceParts := strings.Split(strings.TrimPrefix(instanceID, "inst_"), "_")
	return len(requestParts) == 3 && len(instanceParts) == 2 &&
		requestParts[0] == instanceParts[0] && requestParts[1] == instanceParts[1]
}

func (s *Server) handleOperationRequestReserve(r *http.Request) (any, error) {
	ctx, cancel := context.WithTimeout(r.Context(), operationRequestPrepareTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, apierr.New(apierr.CodeCancelled, "操作预留准备已取消，未派发实际请求")
	}
	var body operationRequestReserveBody
	if err := DecodeBody(r, &body); err != nil {
		return nil, err
	}
	if err := validateOperationRequestPath(body.Method, body.Path); err != nil {
		return nil, err
	}
	if !s.reservableOperationRoute(body.Method, body.Path) {
		return nil, apierr.New(apierr.CodeNotSupported,
			"该 method/path 不是可预留的同步管理操作")
	}
	if err := ctx.Err(); err != nil {
		return nil, apierr.New(apierr.CodeCancelled, "操作预留准备已取消，未派发实际请求")
	}
	release, err := s.writeCoordinator.AcquireShared(ctx, s.operationInstanceID)
	if err != nil {
		return nil, coordinatorAPIError(err)
	}
	defer release()
	status, err := s.operationRequests.Reserve(body.Method, body.Path)
	if err != nil {
		return nil, err
	}
	if s.operationRequestAfterReserve != nil {
		s.operationRequestAfterReserve()
	}
	if err := ctx.Err(); err != nil {
		// No response reference escaped and no original mutation has been sent.
		// Close the reservation locally so it cannot hold the upgrade gate until TTL.
		_ = s.operationRequests.Close(status.RequestID, status.InstanceID, nil, false)
		return nil, apierr.New(apierr.CodeCancelled, "操作预留准备已取消，未派发实际请求")
	}
	return status, nil
}

func (s *Server) handleOperationRequestStatus(r *http.Request) (any, error) {
	requestID := r.PathValue("requestId")
	query := r.URL.Query()
	instances := query["instanceId"]
	if !validOperationRequestID(requestID) || len(instances) != 1 || !validOperationInstanceID(instances[0]) || len(query) != 1 {
		return nil, apierr.New(apierr.CodeInvalidRequest, "requestId 与唯一 instanceId 必须合法")
	}
	instanceID := instances[0]
	// A locally unknown/evicted reference is fenced only after a fresh locked
	// generation read proves the requested instance can no longer admit a writer.
	fenced, _ := s.writeCoordinator.CurrentInstanceFenced(r.Context(), instanceID)
	return s.operationRequests.Status(requestID, instanceID, s.operationForRequestID, fenced), nil
}

func (s *Server) handleOperationRequestClose(r *http.Request) (any, error) {
	requestID := r.PathValue("requestId")
	if !validOperationRequestID(requestID) {
		return nil, apierr.New(apierr.CodeInvalidRequest, "requestId 不合法")
	}
	var body operationRequestCloseBody
	if err := DecodeBody(r, &body); err != nil {
		return nil, err
	}
	if !validOperationInstanceID(body.InstanceID) {
		return nil, apierr.New(apierr.CodeInvalidRequest, "instanceId 不合法")
	}
	fenced, _ := s.writeCoordinator.CurrentInstanceFenced(r.Context(), body.InstanceID)
	return s.operationRequests.Close(requestID, body.InstanceID, s.operationForRequestID, fenced), nil
}

func (s *Server) operationForRequestID(requestID string) (operations.Operation, bool) {
	page, err := s.operationsByRequestID(operations.Filter{Limit: operations.MaxLimit}, requestID)
	if err != nil || len(page.Items) == 0 {
		return operations.Operation{}, false
	}
	return page.Items[0], true
}

// checkExclusiveFreezeReady is called only after an installer owns the shared
// coordinator's exclusive freeze. It rechecks persistent preview leases and
// one-shot request reservations that do not themselves hold shared file locks.
func (s *Server) checkExclusiveFreezeReady() error {
	if len(s.Leases.InFlight()) > 0 || tx.ApplyInFlight() > 0 ||
		s.writeCoordinator.ActiveShared() > 0 || s.operationRequests.HasOutstanding() {
		return apierr.New(apierr.CodeInvalidRequest,
			"仍有未消费预览或操作预留，安装替换已拒绝（当前安装未替换）")
	}
	return nil
}

func (s *Server) reservableOperationRoute(method, requestPath string) bool {
	if s.mux == nil || s.reservableMutationPatterns == nil {
		return false
	}
	request := &http.Request{Method: method, URL: &url.URL{Path: "/api/v1" + requestPath}}
	_, pattern := s.mux.Handler(request)
	_, ok := s.reservableMutationPatterns[pattern]
	return ok
}

func (s *Server) validateOperationRequest(r *http.Request) (requestID, instanceID, requestPath string, tracked bool, err error) {
	id := r.Header.Get(operationIDHeader)
	trackedInstanceID := r.Header.Get(operationInstanceHeader)
	if id == "" && trackedInstanceID == "" {
		return "", "", "", false, nil // legacy callers retain the server-generated X-Request-Id path.
	}
	if !validOperationRequestID(id) || !validOperationInstanceID(trackedInstanceID) {
		return "", "", "", true, apierr.New(apierr.CodeInvalidRequest,
			"跟踪请求必须同时携带有效的操作预留编号与服务实例编号")
	}
	method := r.Method
	requestPath = strings.TrimPrefix(r.URL.Path, "/api/v1")
	if !s.reservableOperationRoute(method, requestPath) {
		return "", "", "", true, apierr.New(apierr.CodeNotSupported, "该 method/path 不能消费操作预留")
	}
	return id, trackedInstanceID, requestPath, true, nil
}

func coordinatorAPIError(err error) error {
	switch {
	case err == nil:
		return nil
	case err == errWriteFrozen:
		return apierr.New(apierr.CodeInvalidRequest, "服务正在冻结写入入口，请等待升级阶段结束后重试")
	case err == errOperationInstanceStale:
		return apierr.New(apierr.CodeInvalidRequest, "服务实例已更新；请重新读取状态后再操作")
	default:
		return apierr.Wrap(apierr.CodeStorage, err, "写入协调器不可用，未执行操作")
	}
}

func operationRequestHTTPStatus(err error) int {
	if err == nil {
		return http.StatusOK
	}
	return apierr.From(err).Code.HTTPStatus()
}

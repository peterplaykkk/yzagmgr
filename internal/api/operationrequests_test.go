package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/operations"
)

type operationRequestTestState struct {
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

type cancelOnReadBody struct {
	reader io.Reader
	cancel context.CancelFunc
	done   bool
}

type cancelOnNthErrContext struct {
	context.Context
	mu    sync.Mutex
	calls int
	nth   int
}

func (c *cancelOnNthErrContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls >= c.nth {
		return context.Canceled
	}
	return nil
}

func (b *cancelOnReadBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if !b.done {
		b.done = true
		b.cancel()
	}
	return n, err
}

func operationRequestCall(
	t *testing.T, srv *contractServer, method, path, body string, token bool, headers map[string]string,
) envelope {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if token {
		req.Header.Set("X-Agmgr-Token", srv.token)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	res := rec.Result()
	out := envelope{status: res.StatusCode, header: res.Header, body: rec.Body.String()}
	var parsed struct {
		OK        bool            `json:"ok"`
		Data      json.RawMessage `json:"data"`
		Error     *apierr.Error   `json:"error"`
		RequestID string          `json:"requestId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("%s %s did not return envelope JSON: %v (%s)", method, path, err, rec.Body.String())
	}
	out.ok, out.data, out.hasData, out.reqID = parsed.OK, parsed.Data, parsed.Data != nil, parsed.RequestID
	if parsed.Error != nil {
		out.code, out.message = parsed.Error.Code, parsed.Error.Message
	}
	return out
}

func operationRequestReserve(t *testing.T, srv *contractServer, method, path string) operationRequestTestState {
	t.Helper()
	res := srv.callRaw(t, http.MethodPost, "/api/v1/operations/requests",
		encodeTestJSON(t, map[string]string{"method": method, "path": path}), true)
	if !res.ok {
		t.Fatalf("reserve %s %s failed: %s", method, path, res.body)
	}
	var state operationRequestTestState
	if err := json.Unmarshal(res.data, &state); err != nil {
		t.Fatalf("reserve response is not request state: %v (%s)", err, res.body)
	}
	return state
}

func operationRequestState(t *testing.T, srv *contractServer, requestID, instanceID string) (envelope, operationRequestTestState) {
	t.Helper()
	res := srv.callRaw(t, http.MethodGet,
		"/api/v1/operations/requests/"+requestID+"?instanceId="+instanceID, "", true)
	var state operationRequestTestState
	if res.hasData {
		if err := json.Unmarshal(res.data, &state); err != nil {
			t.Fatalf("status response invalid: %v (%s)", err, res.body)
		}
	}
	return res, state
}

func operationRequestClose(t *testing.T, srv *contractServer, requestID, instanceID string) (envelope, operationRequestTestState) {
	t.Helper()
	res := srv.callRaw(t, http.MethodPost, "/api/v1/operations/requests/"+requestID+"/close",
		encodeTestJSON(t, map[string]string{"instanceId": instanceID}), true)
	var state operationRequestTestState
	if res.hasData {
		if err := json.Unmarshal(res.data, &state); err != nil {
			t.Fatalf("close response invalid: %v (%s)", err, res.body)
		}
	}
	return res, state
}

func operationRequestStateValue(t *testing.T, value any) operationRequestTestState {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode ledger state: %v", err)
	}
	var state operationRequestTestState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("decode ledger state: %v (%s)", err, raw)
	}
	return state
}

func TestOperationRequestAPIsReserveCloseAndBindInstance(t *testing.T) {
	srv := newContractServer(t)
	unauth := srv.callRaw(t, http.MethodPost, "/api/v1/operations/requests",
		`{"method":"POST","path":"/workbuddy/set-default"}`, false)
	if unauth.ok || unauth.status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated reserve = %s", unauth.body)
	}
	badJSON := srv.callRaw(t, http.MethodPost, "/api/v1/operations/requests", `{`, true)
	if badJSON.ok || badJSON.code != apierr.CodeInvalidRequest {
		t.Fatalf("malformed reserve = %s", badJSON.body)
	}

	for _, tc := range []struct {
		name, method, path string
		wantCode           apierr.Code
	}{
		{"GET is read-only", http.MethodGet, "/workbuddy", apierr.CodeInvalidRequest},
		{"absolute URL", http.MethodPost, "https://example.invalid/workbuddy/set-default", apierr.CodeInvalidRequest},
		{"prefixed path", http.MethodPost, "/api/v1/workbuddy/set-default", apierr.CodeNotSupported},
		{"query string", http.MethodPost, "/workbuddy/set-default?index=0", apierr.CodeInvalidRequest},
		{"fragment", http.MethodPost, "/workbuddy/set-default#x", apierr.CodeInvalidRequest},
		{"path traversal", http.MethodPost, "/workbuddy/../update/app/install", apierr.CodeInvalidRequest},
		{"async install excluded", http.MethodPost, "/update/app/install", apierr.CodeNotSupported},
		{"unknown route", http.MethodPost, "/not-registered", apierr.CodeNotSupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := srv.callRaw(t, http.MethodPost, "/api/v1/operations/requests",
				encodeTestJSON(t, map[string]string{"method": tc.method, "path": tc.path}), true)
			if res.ok || res.code != tc.wantCode {
				t.Fatalf("invalid reserve target accepted: %s", res.body)
			}
		})
	}

	before, err := srv.Ops.Query(operations.Filter{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	reserved := operationRequestReserve(t, srv, http.MethodPost, "/routes")
	if reserved.RequestID == "" || reserved.InstanceID == "" || reserved.State != "reserved" ||
		reserved.Method != http.MethodPost || reserved.Path != "/routes" {
		t.Fatalf("reservation metadata mismatch: %+v", reserved)
	}
	if _, err := time.Parse(time.RFC3339, reserved.ExpiresAt); err != nil {
		t.Fatalf("expiresAt is not RFC3339 UTC: %q", reserved.ExpiresAt)
	}
	withoutInstance := srv.callRaw(t, http.MethodGet,
		"/api/v1/operations/requests/"+reserved.RequestID, "", true)
	if withoutInstance.ok || withoutInstance.code != apierr.CodeInvalidRequest {
		t.Fatalf("status without instanceId = %s", withoutInstance.body)
	}
	for _, tc := range []struct {
		name, target string
	}{
		{"invalid request ID", "/api/v1/operations/requests/not-an-operation-id?instanceId=" + reserved.InstanceID},
		{"duplicate instance parameter", "/api/v1/operations/requests/" + reserved.RequestID + "?instanceId=" + reserved.InstanceID + "&instanceId=" + reserved.InstanceID},
		{"extra query parameter", "/api/v1/operations/requests/" + reserved.RequestID + "?instanceId=" + reserved.InstanceID + "&extra=1"},
		{"invalid instance ID", "/api/v1/operations/requests/" + reserved.RequestID + "?instanceId=foreign-instance"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := srv.callRaw(t, http.MethodGet, tc.target, "", true)
			if res.ok || res.code != apierr.CodeInvalidRequest {
				t.Fatalf("invalid status binding accepted: %s", res.body)
			}
		})
	}
	for _, tc := range []struct{ name, path, body string }{
		{"invalid close request ID", "/api/v1/operations/requests/not-an-operation-id/close", `{"instanceId":"` + reserved.InstanceID + `"}`},
		{"malformed close body", "/api/v1/operations/requests/" + reserved.RequestID + "/close", `{`},
		{"invalid close instance ID", "/api/v1/operations/requests/" + reserved.RequestID + "/close", `{"instanceId":"bad"}`},
	} {
		res := srv.callRaw(t, http.MethodPost, tc.path, tc.body, true)
		if res.ok || res.code != apierr.CodeInvalidRequest {
			t.Errorf("%s accepted malformed close binding: %s", tc.name, res.body)
		}
	}
	wrongInstanceID := "inst_0123456789abcdef0123456789abcdef_1"
	wrongInstance, wrongState := operationRequestState(t, srv, reserved.RequestID, wrongInstanceID)
	if !wrongInstance.ok || wrongState.State != "unknown" || wrongState.OperationID != "" {
		t.Fatalf("request ID from another instance must not disclose terminal data: %s / %+v", wrongInstance.body, wrongState)
	}
	wrongClose, wrongCloseState := operationRequestClose(t, srv, reserved.RequestID, wrongInstanceID)
	if !wrongClose.ok || wrongCloseState.State != "unknown" {
		t.Fatalf("wrong-instance close returned an unrelated state: %+v", wrongCloseState)
	}
	fencedOldRequestID := "opreq_ffffffffffffffffffffffffffffffff_1_999"
	fencedOldInstanceID := "inst_ffffffffffffffffffffffffffffffff_1"
	_, fencedOld := operationRequestState(t, srv, fencedOldRequestID, fencedOldInstanceID)
	if fencedOld.State != "fenced_unknown" || fencedOld.Reason != "instance_replaced" {
		t.Fatalf("valid old-generation reference did not retain its outcome-unknown fence: %+v", fencedOld)
	}
	stillReserved, status := operationRequestState(t, srv, reserved.RequestID, reserved.InstanceID)
	if !stillReserved.ok || status.State != "reserved" {
		t.Fatalf("wrong-instance close changed reservation: %s / %+v", stillReserved.body, status)
	}
	closedEnvelope, closed := operationRequestClose(t, srv, reserved.RequestID, reserved.InstanceID)
	if !closedEnvelope.ok || closed.State != "closed" || closed.Reason != "not_started" {
		t.Fatalf("reserved close must fence late admission as not_started: %s / %+v", closedEnvelope.body, closed)
	}
	_, repeated := operationRequestClose(t, srv, reserved.RequestID, reserved.InstanceID)
	if repeated.State != "closed" {
		t.Fatalf("repeat close is not idempotent: %+v", repeated)
	}
	_, final := operationRequestState(t, srv, reserved.RequestID, reserved.InstanceID)
	if final.State != "closed" {
		t.Fatalf("closed state did not survive status read: %+v", final)
	}
	after, err := srv.Ops.Query(operations.Filter{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if after.Total != before.Total {
		t.Fatalf("reserve/status/close wrote normal operation logs: %d -> %d", before.Total, after.Total)
	}
	t.Run("reserve preparation cancellation and freeze failures create no usable reference", func(t *testing.T) {
		server, _ := testableServer(t)
		body := `{"method":"POST","path":"/routes"}`
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/operations/requests", strings.NewReader(body)).WithContext(ctx)
		if _, err := server.handleOperationRequestReserve(request); err == nil || apierr.From(err).Code != apierr.CodeCancelled {
			t.Fatalf("pre-cancelled reserve did not stop before allocation: %v", err)
		}
		if server.operationRequests.HasOutstanding() {
			t.Fatal("pre-cancelled reserve left a live operation reference")
		}

		freeze, err := server.writeCoordinator.AcquireExclusive(context.Background(), server.operationInstanceID)
		if err != nil {
			t.Fatal(err)
		}
		blocked := server.callRaw(t, http.MethodPost, "/api/v1/operations/requests", body, true)
		freeze.Release()
		if blocked.ok || blocked.code != apierr.CodeInvalidRequest || server.operationRequests.HasOutstanding() {
			t.Fatalf("reserve crossed active replacement freeze or left a reference: %s", blocked.body)
		}

		entered := make(chan struct{})
		continueReserve := make(chan struct{})
		var enteredOnce sync.Once
		server.operationRequests.now = func() time.Time {
			enteredOnce.Do(func() {
				close(entered)
				<-continueReserve
			})
			return time.Now().UTC()
		}
		ctx, cancel = context.WithCancel(context.Background())
		request = httptest.NewRequest(http.MethodPost, "/api/v1/operations/requests", strings.NewReader(body)).WithContext(ctx)
		done := make(chan error, 1)
		go func() {
			_, err := server.handleOperationRequestReserve(request)
			done <- err
		}()
		select {
		case <-entered:
		case <-time.After(time.Second):
			cancel()
			close(continueReserve)
			t.Fatal("reserve did not reach the post-admission clock barrier")
		}
		cancel()
		close(continueReserve)
		if err := <-done; err == nil || apierr.From(err).Code != apierr.CodeCancelled {
			t.Fatalf("cancelled post-admission prepare returned %v", err)
		}
		if server.operationRequests.HasOutstanding() {
			t.Fatal("cancelled reserve response left an undisclosed active reference")
		}
	})

	createReservation := operationRequestReserve(t, srv, http.MethodPost, "/tools/pi/profiles")
	_, createClosed := operationRequestClose(t, srv, createReservation.RequestID, createReservation.InstanceID)
	if createClosed.State != "closed" {
		t.Fatalf("create reservation wasn't closed before late request: %+v", createClosed)
	}
	profilesBefore := profilesOf(t, srv, "pi")
	late := operationRequestCall(t, srv, http.MethodPost, "/api/v1/tools/pi/profiles",
		`{"name":"must-not-exist","endpoint":"https://provider.example/v1"}`, true,
		map[string]string{"X-Agmgr-Operation-Id": createReservation.RequestID,
			"X-Agmgr-Operation-Instance": createReservation.InstanceID})
	if late.ok {
		t.Fatalf("late request crossed a closed reservation: %s", late.body)
	}
	profilesAfter := profilesOf(t, srv, "pi")
	if len(profilesAfter.Items) != len(profilesBefore.Items) {
		t.Fatalf("close failed to fence late profile create: before=%d after=%d", len(profilesBefore.Items), len(profilesAfter.Items))
	}
	//（「global writer view」子测随旧写态视图删除。）
}

func TestOperationRequestReserveCancellationClosesUnreturnedReservation(t *testing.T) {
	srv := newContractServer(t)
	body := `{"method":"POST","path":"/routes"}`
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/operations/requests", strings.NewReader(body)).WithContext(ctx)
	if _, err := srv.handleOperationRequestReserve(request); apierr.From(err).Code != apierr.CodeCancelled {
		t.Fatalf("pre-canceled reservation returned %v", err)
	}
	if srv.operationRequests.HasOutstanding() {
		t.Fatal("pre-canceled reservation left a blocker in the ledger")
	}

	ctx, cancel = context.WithCancel(context.Background())
	srv.operationRequestAfterReserve = cancel
	request = httptest.NewRequest(http.MethodPost, "/api/v1/operations/requests", strings.NewReader(body)).WithContext(ctx)
	if _, err := srv.handleOperationRequestReserve(request); apierr.From(err).Code != apierr.CodeCancelled {
		t.Fatalf("cancellation after reserve returned %v", err)
	}
	if srv.operationRequests.HasOutstanding() {
		t.Fatal("locally closed response-less reservation still blocks replacement")
	}

	srv.operationRequestAfterReserve = nil
	request = httptest.NewRequest(http.MethodPost, "/api/v1/operations/requests", strings.NewReader("{"))
	if _, err := srv.handleOperationRequestReserve(request); err == nil {
		t.Fatal("malformed reserve body unexpectedly produced an operation reference")
	}

	ctx, cancel = context.WithCancel(context.Background())
	request = httptest.NewRequest(http.MethodPost, "/api/v1/operations/requests", &cancelOnReadBody{
		reader: strings.NewReader(body), cancel: cancel,
	}).WithContext(ctx)
	if _, err := srv.handleOperationRequestReserve(request); apierr.From(err).Code != apierr.CodeCancelled {
		t.Fatalf("cancellation during body decode did not stop reservation preparation: %v", err)
	}
	if srv.operationRequests.HasOutstanding() {
		t.Fatal("body-decode cancellation left an outstanding reservation")
	}

	capacityServer := newContractServer(t)
	for i := 0; i < operationRequestMaxEntries; i++ {
		if _, err := capacityServer.operationRequests.Reserve(http.MethodPost, "/routes"); err != nil {
			t.Fatal(err)
		}
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/operations/requests", strings.NewReader(body))
	if _, err := capacityServer.handleOperationRequestReserve(request); apierr.From(err).Code != apierr.CodeInvalidRequest {
		t.Fatalf("full reservation ledger returned %v", err)
	}
}

func TestOperationRequestStaleWriteIsNotLoggedAndFrozenFailureLogsAfterRelease(t *testing.T) {
	srv := newContractServer(t)
	ref, err := srv.routerTokens.Put("sk-operation-request-stale-write-0001")
	if err != nil {
		t.Fatal(err)
	}
	profile := createUserProfileForDraftTest(t, srv, "pi", encodeTestJSON(t, map[string]any{
		"name": "lifecycle stale log fixture", "endpoint": "https://provider.example/v1",
		"credRef": ref, "models": []string{"deepseek-flash"},
	}))
	path := "/tools/pi/profiles/" + profile.ID
	currentInstance := srv.operationInstanceID
	srv.operationInstanceID = "inst_ffffffffffffffff_1"
	stale := operationRequestCall(t, srv, http.MethodPut, "/api/v1"+path,
		`{"name":"stale write must not persist"}`, true, nil)
	if stale.ok || stale.code != apierr.CodeInvalidRequest {
		t.Fatalf("stale-generation synchronous mutation was not refused: %s", stale.body)
	}
	page, err := srv.Ops.Query(operations.Filter{Action: "profile.update", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 0 {
		t.Fatalf("stale server wrote an operation row after losing generation admission: %+v", page.Items)
	}
	if got := profilesOf(t, srv, "pi"); findProfileName(got.Items, profile.ID) != "lifecycle stale log fixture" {
		t.Fatal("stale server mutation changed the saved profile")
	}
	srv.operationInstanceID = currentInstance

	canceledCtx := &cancelOnNthErrContext{Context: context.Background(), nth: 1}
	canceledRequest := httptest.NewRequest(http.MethodPut, "/api/v1"+path,
		strings.NewReader(`{"name":"canceled before admission"}`)).WithContext(canceledCtx)
	canceledRequest.Header.Set("Content-Type", "application/json")
	canceledRequest.Header.Set("X-Agmgr-Token", srv.token)
	canceledRecorder := httptest.NewRecorder()
	srv.ServeHTTP(canceledRecorder, canceledRequest)
	var canceledEnvelope struct {
		OK    bool `json:"ok"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(canceledRecorder.Body.Bytes(), &canceledEnvelope); err != nil || canceledEnvelope.OK || canceledEnvelope.Error.Code != string(apierr.CodeCancelled) {
		t.Fatalf("pre-admission canceled write result = %v (%s)", err, canceledRecorder.Body.String())
	}
	page, err = srv.Ops.Query(operations.Filter{Action: "profile.update", Limit: 20})
	if err != nil || page.Total != 1 || page.Items[0].ErrorCode != string(apierr.CodeCancelled) {
		t.Fatalf("current-generation canceled mutation did not record its bounded failure: %+v err=%v", page.Items, err)
	}

	freeze, err := srv.writeCoordinator.AcquireExclusive(context.Background(), currentInstance)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, "/api/v1"+path,
		strings.NewReader(`{"name":"same-generation frozen write"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Agmgr-Token", srv.token)
	rec := httptest.NewRecorder()
	responseDone := make(chan struct{})
	go func() {
		srv.ServeHTTP(rec, request)
		close(responseDone)
	}()
	select {
	case <-responseDone:
		freeze.Release()
		t.Fatal("operation returned before the temporary exclusive freeze could be drained")
	case <-time.After(25 * time.Millisecond):
	}
	freeze.Release()
	select {
	case <-responseDone:
	case <-time.After(2 * time.Second):
		t.Fatal("frozen failure audit did not resume after shared operation-log admission returned")
	}
	page, err = srv.Ops.Query(operations.Filter{Action: "profile.update", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || page.Items[0].Result != operations.ResultFailed || page.Items[0].ErrorCode != string(apierr.CodeInvalidRequest) ||
		page.Items[1].ErrorCode != string(apierr.CodeCancelled) {
		t.Fatalf("same-generation temporary-freeze failure was not logged after release: %+v", page.Items)
	}
}

func TestOperationRequestTrackedAdmissionFailurePreservesReservation(t *testing.T) {
	srv := newContractServer(t)
	ref, err := srv.routerTokens.Put("sk-operation-request-admission-error-0001")
	if err != nil {
		t.Fatal(err)
	}
	profile := createUserProfileForDraftTest(t, srv, "pi", encodeTestJSON(t, map[string]any{
		"name": "tracked admission fixture", "endpoint": "https://provider.example/v1",
		"credRef": ref, "models": []string{"deepseek-flash"},
	}))
	path := "/tools/pi/profiles/" + profile.ID
	reserved := operationRequestReserve(t, srv, http.MethodPut, path)
	headers := map[string]string{
		operationIDHeader: reserved.RequestID, operationInstanceHeader: reserved.InstanceID,
	}

	t.Run("temporary freeze keeps reservation unconsumed and unlogged", func(t *testing.T) {
		lease, err := srv.writeCoordinator.AcquireExclusive(context.Background(), srv.operationInstanceID)
		if err != nil {
			t.Fatal(err)
		}
		response := operationRequestCall(t, srv, http.MethodPut, "/api/v1"+path,
			`{"name":"must remain reserved"}`, true, headers)
		if response.ok || response.code != apierr.CodeInvalidRequest {
			lease.Release()
			t.Fatalf("tracked write crossed a temporary exclusive freeze: %s", response.body)
		}
		lease.Release()
		_, state := operationRequestState(t, srv, reserved.RequestID, reserved.InstanceID)
		if state.State != "reserved" {
			t.Fatalf("failed admission consumed its reservation: %+v", state)
		}
		page, err := srv.Ops.Query(operations.Filter{Action: "profile.update", Limit: 10})
		if err != nil || page.Total != 0 {
			t.Fatalf("tracked pre-dispatch failure wrote an operation row: %+v err=%v", page.Items, err)
		}
	})

	t.Run("context canceled after successful SH stays unconsumed", func(t *testing.T) {
		ctx := &cancelOnNthErrContext{Context: context.Background(), nth: 4}
		request := httptest.NewRequest(http.MethodPut, "/api/v1"+path,
			strings.NewReader(`{"name":"post-admission canceled"}`)).WithContext(ctx)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Agmgr-Token", srv.token)
		request.Header.Set(operationIDHeader, reserved.RequestID)
		request.Header.Set(operationInstanceHeader, reserved.InstanceID)
		recorder := httptest.NewRecorder()
		srv.ServeHTTP(recorder, request)
		var envelope struct {
			OK    bool `json:"ok"`
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil || envelope.OK || envelope.Error.Code != string(apierr.CodeCancelled) {
			t.Fatalf("post-admission cancellation result = %v (%s), ErrCalls=%d", err, recorder.Body.String(), ctx.calls)
		}
		_, state := operationRequestState(t, srv, reserved.RequestID, reserved.InstanceID)
		if state.State != "reserved" {
			t.Fatalf("cancellation between SH and consume transitioned the reservation: %+v", state)
		}
	})
}

func TestOperationLogAdmissionRetriesFreezeAndHonorsDeadline(t *testing.T) {
	srv := newContractServer(t)
	lease, err := srv.writeCoordinator.AcquireExclusive(context.Background(), srv.operationInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	if release, err := srv.acquireOperationLogAdmissionWithContext(ctx); release != nil || !errors.Is(err, context.DeadlineExceeded) {
		cancel()
		lease.Release()
		t.Fatalf("operation-log admission did not bound its temporary-freeze retries: release=%v err=%v", release != nil, err)
	}
	cancel()
	lease.Release()
	release, err := srv.acquireOperationLogAdmissionWithContext(nil)
	if err != nil {
		t.Fatalf("operation-log admission did not recover after freeze release: %v", err)
	}
	release()
}

func TestOperationLogHeldExclusiveAndAppendFailurePaths(t *testing.T) {
	t.Run("held exclusive lease covers the operation append", func(t *testing.T) {
		srv := newContractServer(t)
		lease, err := srv.writeCoordinator.AcquireExclusive(context.Background(), srv.operationInstanceID)
		if err != nil {
			t.Fatal(err)
		}
		srv.writeCoordinator.Hold(lease)
		request := httptest.NewRequest(http.MethodPost, "/api/v1/self-update/install", strings.NewReader(`{}`))
		operationID := srv.recordOperationWithAdmission(opSpec{Action: "test.operation.held"}, request, "req_held_operation_0001", "", "", nil)
		if operationID == "" {
			lease.Release()
			t.Fatal("held-exclusive operation append did not return its audit identity")
		}
		logged, err := srv.Ops.Get(operationID)
		if err != nil || logged.Action != "test.operation.held" {
			lease.Release()
			t.Fatalf("held-exclusive operation was not appended: %+v, %v", logged, err)
		}
		lease.Release()
	})

	t.Run("audit storage failure is observable as an empty operation identity", func(t *testing.T) {
		srv := newContractServer(t)
		if err := os.Mkdir(srv.Env.OperationsFile(), 0o700); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/v1/self-update/install", strings.NewReader(`{}`))
		if operationID := srv.recordOperationWithAdmission(opSpec{Action: "test.operation.append-error"},
			request, "req_operation_append_error_1", "", "", nil); operationID != "" {
			t.Fatalf("failed audit append returned an operation identity: %q", operationID)
		}
	})
}

func TestOperationRequestTrackedLoggedFailureAndPanicAreTerminallyRecorded(t *testing.T) {
	srv := newContractServer(t)
	srv.mutate("POST /api/v1/test/operation-logged-failure", opSpec{Action: "test.operation.logged-failure"},
		func(*http.Request) (any, string, error) {
			return map[string]string{"status": "probe-error"}, "", &loggedFailure{
				data: map[string]string{"status": "probe-error"}, code: apierr.CodeGatewayUnreachable,
			}
		})
	loggedReservation := operationRequestReserve(t, srv, http.MethodPost, "/test/operation-logged-failure")
	logged := operationRequestCall(t, srv, http.MethodPost, "/api/v1/test/operation-logged-failure", `{}`,
		true, map[string]string{operationIDHeader: loggedReservation.RequestID, operationInstanceHeader: loggedReservation.InstanceID})
	if !logged.ok || logged.reqID != loggedReservation.RequestID {
		t.Fatalf("tracked loggedFailure route did not retain its successful response envelope: %s", logged.body)
	}
	_, loggedState := operationRequestState(t, srv, loggedReservation.RequestID, loggedReservation.InstanceID)
	if loggedState.State != "completed" || loggedState.HTTPStatus != http.StatusOK || loggedState.OperationID == "" {
		t.Fatalf("tracked loggedFailure route was not completed with its actual operation ID: %+v", loggedState)
	}
	loggedOp, err := srv.Ops.Get(loggedState.OperationID)
	if err != nil || loggedOp.Result != operations.ResultFailed || loggedOp.ErrorCode != string(apierr.CodeGatewayUnreachable) {
		t.Fatalf("loggedFailure operation record = %+v, %v", loggedOp, err)
	}

	srv.mutate("POST /api/v1/test/operation-panic", opSpec{Action: "test.operation.panic"},
		func(*http.Request) (any, string, error) { panic("fixture secret panic payload") })
	panicReservation := operationRequestReserve(t, srv, http.MethodPost, "/test/operation-panic")
	request := httptest.NewRequest(http.MethodPost, "/api/v1/test/operation-panic", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Agmgr-Token", srv.token)
	request.Header.Set(operationIDHeader, panicReservation.RequestID)
	request.Header.Set(operationInstanceHeader, panicReservation.InstanceID)
	response := httptest.NewRecorder()
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		srv.ServeHTTP(response, request)
	}()
	if recovered == nil {
		t.Fatal("fixture panic did not propagate through the panic-safe audit boundary")
	}
	if strings.Contains(response.Body.String(), "fixture secret panic payload") {
		t.Fatal("panic details escaped to the HTTP response")
	}
	_, panicState := operationRequestState(t, srv, panicReservation.RequestID, panicReservation.InstanceID)
	if panicState.State != "completed" || panicState.HTTPStatus != http.StatusInternalServerError ||
		panicState.ErrorCode != string(apierr.CodeInternal) || panicState.OperationID == "" {
		t.Fatalf("tracked panic did not persist a generic internal terminal result: %+v", panicState)
	}
	panicOp, err := srv.Ops.Get(panicState.OperationID)
	if err != nil || panicOp.Result != operations.ResultFailed || panicOp.ErrorCode != string(apierr.CodeInternal) || panicOp.RequestID != panicReservation.RequestID {
		t.Fatalf("panic audit record is missing generic safe failure metadata: %+v, %v", panicOp, err)
	}
}

func TestOperationRequestAdmissionConsumesOnlyMatchingWriteOnce(t *testing.T) {
	srv := newContractServer(t)
	ref, err := srv.routerTokens.Put("sk-operation-request-profile-0001")
	if err != nil {
		t.Fatal(err)
	}
	profile := createUserProfileForDraftTest(t, srv, "pi", encodeTestJSON(t, map[string]any{
		"name": "operation request fixture", "endpoint": "https://provider.example/v1",
		"credRef": ref, "models": []string{"deepseek-flash"},
	}))
	path := "/tools/pi/profiles/" + profile.ID
	reserved := operationRequestReserve(t, srv, http.MethodPut, path)
	trackedHeaders := map[string]string{
		"X-Agmgr-Operation-Id":       reserved.RequestID,
		"X-Agmgr-Operation-Instance": reserved.InstanceID,
		"X-Request-Id":               "caller-request-id-must-not-win",
	}
	missingHeader := operationRequestCall(t, srv, http.MethodPut, "/api/v1"+path,
		`{"name":"missing operation instance"}`, true, map[string]string{"X-Agmgr-Operation-Id": reserved.RequestID})
	if missingHeader.ok || missingHeader.code != apierr.CodeInvalidRequest {
		t.Fatalf("tracked write with only one correlation header was not rejected: %s", missingHeader.body)
	}
	missingID := operationRequestCall(t, srv, http.MethodPut, "/api/v1"+path,
		`{"name":"missing operation ID"}`, true, map[string]string{"X-Agmgr-Operation-Instance": reserved.InstanceID})
	if missingID.ok || missingID.code != apierr.CodeInvalidRequest {
		t.Fatalf("tracked write with only an instance header was not rejected: %s", missingID.body)
	}
	//（「unreservable async worker」子断言随 §11 异步任务端点删除而注销——
	// 下载/任务/自更新面已删，现存 mutate 端点全部可预留；design §2.2。）

	wrongPath := operationRequestCall(t, srv, http.MethodPut, "/api/v1/tools/pi/profiles/another-card",
		`{"name":"must-not-write"}`, true, trackedHeaders)
	if wrongPath.ok {
		t.Fatalf("wrong path consumed reserved write: %s", wrongPath.body)
	}
	_, stillReserved := operationRequestState(t, srv, reserved.RequestID, reserved.InstanceID)
	if stillReserved.State != "reserved" {
		t.Fatalf("path mismatch consumed reservation: %+v", stillReserved)
	}

	wantName := "operation-request-body-marker-not-logged"
	body := encodeTestJSON(t, map[string]string{"name": wantName})
	completed := operationRequestCall(t, srv, http.MethodPut, "/api/v1"+path, body, true, trackedHeaders)
	if !completed.ok || completed.reqID != reserved.RequestID || completed.header.Get("X-Request-Id") != reserved.RequestID {
		t.Fatalf("tracked response did not use server-issued correlation ID: %s", completed.body)
	}
	_, final := operationRequestState(t, srv, reserved.RequestID, reserved.InstanceID)
	if final.State != "completed" || final.HTTPStatus != http.StatusOK || final.OperationID == "" {
		t.Fatalf("completed write state missing handler terminal evidence: %+v", final)
	}
	if operation, ok := srv.operationForRequestID(reserved.RequestID); !ok || operation.ID != final.OperationID {
		t.Fatalf("request ID lookup failed to recover its real terminal operation: %+v, %v", operation, ok)
	}
	if operation, ok := srv.operationForRequestID("opreq_ffffffffffffffff_1_999"); ok || operation.ID != "" {
		t.Fatalf("request ID lookup fabricated a missing terminal operation: %+v, %v", operation, ok)
	}
	updated := srv.callRaw(t, http.MethodGet, "/api/v1/tools/pi/profiles", "", true)
	var list struct {
		Items []Profile `json:"items"`
	}
	if err := json.Unmarshal(updated.data, &list); err != nil {
		t.Fatal(err)
	}
	if got := findProfileName(list.Items, profile.ID); got != wantName {
		t.Fatalf("matched tracked handler did not apply exactly once: got name %q", got)
	}

	replayHeaders := map[string]string{
		"X-Agmgr-Operation-Id":       reserved.RequestID,
		"X-Agmgr-Operation-Instance": reserved.InstanceID,
	}
	replay := operationRequestCall(t, srv, http.MethodPut, "/api/v1"+path,
		`{"name":"replay-must-not-write"}`, true, replayHeaders)
	if replay.ok {
		t.Fatalf("same reservation replay reached mutation: %s", replay.body)
	}
	updated = srv.callRaw(t, http.MethodGet, "/api/v1/tools/pi/profiles", "", true)
	if err := json.Unmarshal(updated.data, &list); err != nil {
		t.Fatal(err)
	}
	if got := findProfileName(list.Items, profile.ID); got != wantName {
		t.Fatalf("replayed request changed saved profile: %q", got)
	}
	page, err := srv.Ops.Query(operations.Filter{Action: "profile.update", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || page.Items[0].RequestID != reserved.RequestID {
		t.Fatalf("tracked mutation audit log = %+v; expected one correlated operation", page.Items)
	}

	failedReservation := operationRequestReserve(t, srv, http.MethodPut, path)
	failedHeaders := map[string]string{
		"X-Agmgr-Operation-Id":       failedReservation.RequestID,
		"X-Agmgr-Operation-Instance": failedReservation.InstanceID,
	}
	failed := operationRequestCall(t, srv, http.MethodPut, "/api/v1"+path, `{"name":" "}`, true, failedHeaders)
	if failed.ok || failed.status != http.StatusBadRequest || failed.reqID != failedReservation.RequestID {
		t.Fatalf("tracked handler error response lost correlation: %s", failed.body)
	}
	_, failedState := operationRequestState(t, srv, failedReservation.RequestID, failedReservation.InstanceID)
	if failedState.State != "completed" || failedState.HTTPStatus != http.StatusBadRequest || failedState.ErrorCode != string(apierr.CodeInvalidRequest) {
		t.Fatalf("completed error state is not accurately recorded: %+v", failedState)
	}
	page, err = srv.Ops.Query(operations.Filter{Action: "profile.update", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || page.Items[0].RequestID != failedReservation.RequestID {
		t.Fatalf("tracked failed handler wasn't recorded exactly once: %+v", page.Items)
	}
	legacy := operationRequestCall(t, srv, http.MethodPut, "/api/v1"+path,
		`{"name":"legacy untracked client"}`, true, nil)
	if !legacy.ok || legacy.reqID == "" || legacy.header.Get("X-Request-Id") != legacy.reqID {
		t.Fatalf("untracked legacy client behavior changed: %s", legacy.body)
	}
	page, err = srv.Ops.Query(operations.Filter{Action: "profile.update", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 3 || page.Items[0].RequestID != legacy.reqID {
		t.Fatalf("legacy request-id audit behavior changed: %+v", page.Items)
	}
	wrongInstanceState, wrongInstance := operationRequestState(t, srv, reserved.RequestID, "inst_ffffffffffffffffffffffffffffffff_1")
	if !wrongInstanceState.ok || wrongInstance.State != "unknown" || wrongInstance.OperationID != "" {
		t.Fatalf("valid but unrelated instance recovered another request's terminal result: %+v / %s", wrongInstance, wrongInstanceState.body)
	}
	logBytes, err := os.ReadFile(srv.Env.OperationsFile())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logBytes), wantName) {
		t.Fatal("operation log persisted a request-body value instead of bounded metadata")
	}
}

func TestOperationRequestLedgerExpiryAndAdmissionAreAtomic(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	ledger := newOperationRequestLedger("inst_1111111111111111_1", clock)
	if _, err := ledger.Reserve(http.MethodGet, "/workbuddy"); err == nil {
		t.Fatal("ledger accepted a read-only method reservation")
	}
	reserved, err := ledger.Reserve(http.MethodDelete, "/workbuddy/channels/office")
	if err != nil || reserved.State != "reserved" || reserved.RequestID == "" {
		t.Fatalf("ledger reserve = %+v, %v", reserved, err)
	}
	if reserved.RequestID == "" || reserved.InstanceID != ledger.instanceID {
		t.Fatalf("ledger did not bind process instance: %+v", reserved)
	}
	if _, err := ledger.Consume(nil, reserved.RequestID, "inst_9999999999999999_1", http.MethodDelete, reserved.Path); err == nil {
		t.Fatal("nil-context foreign admission unexpectedly consumed reservation")
	}
	canceledCtx, cancelConsume := context.WithCancel(context.Background())
	cancelConsume()
	if _, err := ledger.Consume(canceledCtx, reserved.RequestID, reserved.InstanceID, http.MethodDelete, reserved.Path); apierr.From(err).Code != apierr.CodeCancelled {
		t.Fatalf("pre-canceled consume acquired execution eligibility: %v", err)
	}
	if state := operationRequestStateValue(t, ledger.Status(reserved.RequestID, reserved.InstanceID, nil, false)); state.State != "reserved" {
		t.Fatalf("canceled consume changed reservation state: %+v", state)
	}
	if _, err := ledger.Consume(context.Background(), reserved.RequestID, "different-instance", http.MethodDelete, reserved.Path); err == nil {
		t.Fatal("reservation consumed by another instance")
	}
	if _, err := ledger.Consume(context.Background(), "missing-request", reserved.InstanceID, http.MethodDelete, reserved.Path); err == nil {
		t.Fatal("missing reservation acquired execution eligibility")
	}
	wrong, err := ledger.Consume(context.Background(), reserved.RequestID, reserved.InstanceID, http.MethodPost, "/workbuddy/channels/office")
	if err == nil || wrong.State == "running" {
		t.Fatalf("method mismatch consumed operation request: %+v / %v", wrong, err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	var consumedValue any
	var consumeErr error
	var closedValue any
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		consumedValue, consumeErr = ledger.Consume(context.Background(), reserved.RequestID, reserved.InstanceID, http.MethodDelete, "/workbuddy/channels/office")
	}()
	go func() {
		defer wg.Done()
		<-start
		closedValue = ledger.Close(reserved.RequestID, reserved.InstanceID, nil, false)
	}()
	close(start)
	wg.Wait()
	consumed := operationRequestStateValue(t, consumedValue)
	closed := operationRequestStateValue(t, closedValue)
	if consumeErr == nil {
		if consumed.State != "running" || closed.State != "running" {
			t.Fatalf("consume won race but close didn't observe running: consume=%+v close=%+v err=%v", consumed, closed, consumeErr)
		}
		ledger.Complete(reserved.RequestID, http.StatusOK, "", "op_fixture")
	} else if closed.State == "closed" {
		if consumed.State == "running" {
			t.Fatalf("close won but late consume started: %+v", consumed)
		}
	} else {
		t.Fatalf("close/admission race has no atomic winner: consume=%+v close=%+v err=%v", consumed, closed, consumeErr)
	}
	if consumeErr == nil {
		if got := operationRequestStateValue(t, ledger.Close(reserved.RequestID, reserved.InstanceID, nil, false)); got.State != "completed" {
			t.Fatalf("close pretended to change a completed handler: %+v", got)
		}
	}
	if replay, err := ledger.Consume(context.Background(), reserved.RequestID, reserved.InstanceID, http.MethodDelete, "/workbuddy/channels/office"); err == nil || replay.State == "running" {
		t.Fatalf("reservation replay gained execution rights: %+v / %v", replay, err)
	}

	clock2 := now
	expiring := newOperationRequestLedger("inst_2222222222222222_1", func() time.Time { return clock2 })
	expired, err := expiring.Reserve(http.MethodPost, "/workbuddy/set-default")
	if err != nil {
		t.Fatal(err)
	}
	clock2 = clock2.Add(61 * time.Second)
	state := operationRequestStateValue(t, expiring.Status(expired.RequestID, expired.InstanceID, nil, false))
	if state.State != "closed" || state.Reason != "expired_unclaimed" {
		t.Fatalf("unclaimed expiry did not close conservatively: %+v", state)
	}
	if _, err := expiring.Consume(context.Background(), expired.RequestID, expired.InstanceID, http.MethodPost, "/workbuddy/set-default"); err == nil {
		t.Fatal("expired request acquired a late execution claim")
	}
	capacityLedger := newOperationRequestLedger("inst_3333333333333333_1", func() time.Time { return clock2 })
	for i := 0; i < operationRequestMaxEntries; i++ {
		if _, err := capacityLedger.Reserve(http.MethodPost, "/workbuddy/set-default"); err != nil {
			t.Fatalf("reservation %d/%d unexpectedly rejected before capacity: %v", i+1, operationRequestMaxEntries, err)
		}
	}
	if _, err := capacityLedger.Reserve(http.MethodPost, "/workbuddy/set-default"); err == nil {
		t.Fatal("full in-flight ledger silently evicted or accepted another reservation")
	}

	terminalNow := clock2
	terminalLedger := newOperationRequestLedger("inst_4444444444444444_1", func() time.Time { return terminalNow })
	terminal, err := terminalLedger.Reserve(http.MethodPut, "/tools/pi/profiles/id")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := terminalLedger.Consume(context.Background(), terminal.RequestID, terminal.InstanceID, http.MethodPut, "/tools/pi/profiles/id"); err != nil {
		t.Fatal(err)
	}
	terminalLedger.Complete(terminal.RequestID, http.StatusOK, "", "op_terminal_fixture")
	terminalNow = terminalNow.Add(9 * time.Minute)
	if got := operationRequestStateValue(t, terminalLedger.Status(terminal.RequestID, terminal.InstanceID, nil, false)); got.State != "completed" {
		t.Fatalf("terminal record was evicted before minimum retention: %+v", got)
	}
	terminalNow = terminalNow.Add(2 * time.Minute)
	if got := operationRequestStateValue(t, terminalLedger.Status(terminal.RequestID, terminal.InstanceID, nil, false)); got.State != "unknown" {
		t.Fatalf("terminal record stayed in the bounded cache beyond retention: %+v", got)
	}
	newer, err := terminalLedger.Reserve(http.MethodPut, "/tools/pi/profiles/id")
	if err != nil || newer.RequestID == terminal.RequestID {
		t.Fatalf("pruned terminal ID was reused: new=%+v old=%s err=%v", newer, terminal.RequestID, err)
	}

	runningNow := now
	runningLedger := newOperationRequestLedger("inst_5555555555555555_1", func() time.Time { return runningNow })
	runningReservation, err := runningLedger.Reserve(http.MethodPost, "/workbuddy/set-default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runningLedger.Consume(context.Background(), runningReservation.RequestID, runningReservation.InstanceID,
		http.MethodPost, "/workbuddy/set-default"); err != nil {
		t.Fatal(err)
	}
	runningNow = runningNow.Add(24 * time.Hour)
	if state := operationRequestStateValue(t, runningLedger.Status(runningReservation.RequestID, runningReservation.InstanceID, nil, false)); state.State != "running" {
		t.Fatalf("running mutation was evicted by TTL: %+v", state)
	}
}

func TestOperationRequestOldInstanceStatusUsesTerminalLogAndFenceMeaning(t *testing.T) {
	now := time.Now().UTC()
	ledger := newOperationRequestLedger("inst_aaaaaaaaaaaaaaaa_2", func() time.Time { return now })
	requestID := "opreq_0123456789abcdef_1_1"
	oldInstanceID := "inst_0123456789abcdef_1"
	lookup := func(id string) (operations.Operation, bool) {
		if id != requestID {
			return operations.Operation{}, false
		}
		return operations.Operation{ID: "op_terminal", RequestID: id, Result: "ok", At: now}, true
	}
	prior := operationRequestStateValue(t, ledger.Status(requestID, oldInstanceID, lookup, true))
	if prior.State != "completed" || prior.OperationID != "op_terminal" {
		t.Fatalf("terminal log must win over lost local ledger: %+v", prior)
	}
	closedFromLog := operationRequestStateValue(t, ledger.Close(requestID, oldInstanceID, lookup, false))
	if closedFromLog.State != "completed" || closedFromLog.OperationID != "op_terminal" {
		t.Fatalf("close must consult existing terminal log before fencing: %+v", closedFromLog)
	}
	wrongInstanceID := "inst_ffffffffffffffff_1"
	lookups := 0
	wrongLookup := func(id string) (operations.Operation, bool) {
		lookups++
		return lookup(id)
	}
	if got := operationRequestStateValue(t, ledger.Status(requestID, wrongInstanceID, wrongLookup, true)); got.State != "unknown" || got.OperationID != "" {
		t.Fatalf("status exposed a valid request under another valid instance: %+v", got)
	}
	if got := operationRequestStateValue(t, ledger.Close(requestID, wrongInstanceID, wrongLookup, true)); got.State != "unknown" || got.OperationID != "" {
		t.Fatalf("close exposed a valid request under another valid instance: %+v", got)
	}
	if lookups != 0 {
		t.Fatalf("unrelated valid instance reached terminal lookup %d times", lookups)
	}
	unknown := operationRequestStateValue(t, ledger.Status("unknown-id", oldInstanceID, lookup, false))
	if unknown.State != "unknown" {
		t.Fatalf("absent ledger/log must stay unknown absent completed fence: %+v", unknown)
	}
	unknownClose := operationRequestStateValue(t, ledger.Close("opreq_0123456789abcdef_1_888", oldInstanceID, lookup, false))
	if unknownClose.State != "unknown" {
		t.Fatalf("absent old-instance close without proof must stay unknown: %+v", unknownClose)
	}
	fenced := operationRequestStateValue(t, ledger.Close("opreq_0123456789abcdef_1_999", oldInstanceID, lookup, true))
	if fenced.State != "fenced_unknown" || fenced.Reason != "instance_replaced" {
		t.Fatalf("completed generation fence must not infer old business result: %+v", fenced)
	}
}

func TestOperationRequestLedgerOutstandingAndCompletionAreMetadataOnly(t *testing.T) {
	if operationTime(time.Time{}) != "" {
		t.Fatal("zero timestamp must remain absent metadata")
	}
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	ledger := newOperationRequestLedger("inst_6666666666666666_1", func() time.Time { return now })
	if ledger.HasOutstanding() || len(ledger.Outstanding()) != 0 {
		t.Fatal("empty ledger reported outstanding writes")
	}
	// The production constructor uses wall-clock UTC when no test clock is
	// supplied; this verifies the fallback produces a usable reservation.
	wallClockLedger := newOperationRequestLedger("inst_0123456789abcdef_1", nil)
	wallClockReservation, err := wallClockLedger.Reserve(http.MethodPost, "/workbuddy/set-default")
	if err != nil || wallClockReservation.ExpiresAt == "" {
		t.Fatalf("nil-clock ledger did not fall back to a live clock: %+v, %v", wallClockReservation, err)
	}
	first, err := ledger.Reserve(http.MethodPost, "/workbuddy/set-default")
	if err != nil {
		t.Fatal(err)
	}
	second, err := ledger.Reserve(http.MethodPut, "/workbuddy/channels/office")
	if err != nil {
		t.Fatal(err)
	}
	ledger.Complete("missing-id", http.StatusOK, "", "op-missing")
	ledger.Complete(first.RequestID, http.StatusOK, "", "op-not-started")
	if !ledger.HasOutstanding() {
		t.Fatal("reserved entries must block install even before dispatch")
	}
	ordered := ledger.Outstanding()
	if len(ordered) != 2 || ordered[0].RequestID != first.RequestID || ordered[1].RequestID != second.RequestID {
		t.Fatalf("equal-expiry entries should sort deterministically by request ID: %+v", ordered)
	}
	if _, err := ledger.Consume(context.Background(), second.RequestID, second.InstanceID, http.MethodPut, "/workbuddy/channels/office"); err != nil {
		t.Fatal(err)
	}
	ledger.Complete(second.RequestID, http.StatusBadRequest, string(apierr.CodeInvalidRequest), "op-failed")
	outstanding := ledger.Outstanding()
	if len(outstanding) != 1 || outstanding[0].RequestID != first.RequestID || outstanding[0].State != "reserved" {
		t.Fatalf("outstanding returned terminal or omitted unstarted request: %+v", outstanding)
	}
	if got := operationRequestStateValue(t, ledger.Status(second.RequestID, second.InstanceID, nil, false)); got.State != "completed" || got.HTTPStatus != http.StatusBadRequest || got.ErrorCode != string(apierr.CodeInvalidRequest) || got.OperationID != "op-failed" {
		t.Fatalf("completed error metadata was not retained: %+v", got)
	}
	if got := ledger.Close(first.RequestID, first.InstanceID, nil, false); got.State != "closed" {
		t.Fatalf("reserved request close = %+v", got)
	}
	if ledger.HasOutstanding() {
		t.Fatal("closed and completed entries must not block installer")
	}
	if absent := operationRequestStateValue(t, ledger.Close("opreq_6666666666666666_1_999", first.InstanceID, nil, false)); absent.State != "fenced_unknown" || absent.Reason != "not_claimable" {
		t.Fatalf("same-instance unclaimable ID was not fenced conservatively: %+v", absent)
	}
	fencedReserved, err := ledger.Reserve(http.MethodPut, "/tools/pi/profiles/id")
	if err != nil {
		t.Fatal(err)
	}
	if status := operationRequestStateValue(t, ledger.Status(fencedReserved.RequestID, fencedReserved.InstanceID, nil, true)); status.State != "fenced_unknown" || status.Reason != "instance_replaced" {
		t.Fatalf("reserved ID in a fenced instance was presented as unstarted: %+v", status)
	}
	terminalRequestID := "opreq_6666666666666666_1_777"
	lookup := func(id string) (operations.Operation, bool) {
		if id != terminalRequestID {
			return operations.Operation{}, false
		}
		return operations.Operation{ID: "op-from-log", RequestID: id, At: now}, true
	}
	if status := operationRequestStateValue(t, ledger.Close(terminalRequestID, ledger.instanceID, lookup, false)); status.State != operationRequestCompleted || status.OperationID != "op-from-log" {
		t.Fatalf("same-instance close failed to recover a terminal audit entry: %+v", status)
	}
	callCount := 0
	differentExpiryLedger := newOperationRequestLedger("inst_7777777777777777_1", func() time.Time {
		callCount++
		return now.Add(time.Duration(callCount) * time.Second)
	})
	earlier, err := differentExpiryLedger.Reserve(http.MethodPost, "/workbuddy/set-default")
	if err != nil {
		t.Fatal(err)
	}
	later, err := differentExpiryLedger.Reserve(http.MethodPut, "/workbuddy/channels/office")
	if err != nil {
		t.Fatal(err)
	}
	expiryOrder := differentExpiryLedger.Outstanding()
	if len(expiryOrder) != 2 || expiryOrder[0].RequestID != earlier.RequestID || expiryOrder[1].RequestID != later.RequestID {
		t.Fatalf("different expiries were not ordered by earliest expiry first: %+v", expiryOrder)
	}
}

func TestOperationRequestCoordinatorErrorsAndRouteBinding(t *testing.T) {
	if got := coordinatorAPIError(nil); got != nil {
		t.Fatalf("nil coordinator error = %v", got)
	}
	if apierr.From(coordinatorAPIError(errWriteFrozen)).Code != apierr.CodeInvalidRequest {
		t.Fatal("freeze admission error was not mapped to invalid_request")
	}
	if apierr.From(coordinatorAPIError(errOperationInstanceStale)).Code != apierr.CodeInvalidRequest {
		t.Fatal("stale instance error was not mapped to invalid_request")
	}
	if apierr.From(coordinatorAPIError(os.ErrPermission)).Code != apierr.CodeStorage {
		t.Fatal("filesystem coordination failure was not mapped to storage")
	}
	if server := (&Server{}); server.reservableOperationRoute(http.MethodPost, "/workbuddy/set-default") {
		t.Fatal("uninitialized router unexpectedly has a reservable target")
	}
	for _, path := range []string{"", "relative", "//example.invalid/x", "/x/./y", "/x/../y", "/x//y", "/x/%2f/y", "/x/%zz", "/x\\y", "/x\u0000y"} {
		if err := validateOperationRequestPath(http.MethodPost, path); err == nil {
			t.Errorf("noncanonical path accepted: %q", path)
		}
	}
	for _, requestID := range []string{"", "bad-prefix_0123_1", "opreq_badhex_1_1", "opreq_abc_1_1", "opreq__1_1", "opreq_0123__1", "opreq_0123_1_", "opreq_0123_x_1", "opreq_0123_1_x"} {
		if validOperationRequestID(requestID) {
			t.Errorf("malformed request ID accepted: %q", requestID)
		}
	}
	for _, instanceID := range []string{"", "bad-prefix_0123_1", "inst_badhex_1", "inst_abc_1", "inst__1", "inst_0123_", "inst_0123_x"} {
		if validOperationInstanceID(instanceID) {
			t.Errorf("malformed instance ID accepted: %q", instanceID)
		}
	}
}

func findProfileName(items []Profile, id string) string {
	for _, item := range items {
		if item.ID == id {
			return item.Name
		}
	}
	return ""
}

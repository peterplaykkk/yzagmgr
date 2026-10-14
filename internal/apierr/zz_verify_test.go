package apierr

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFailNilStillCarriesErrorField(t *testing.T) {
	w := httptest.NewRecorder()
	Fail(nil).WriteJSON(w, "req_x")
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["ok"] != false {
		t.Fatalf("ok 应为 false: %v", got)
	}
	e, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatalf("失败信封必须带 error 字段，实际: %s", w.Body.String())
	}
	if e["code"] != string(CodeInternal) {
		t.Fatalf("error.code 应为 internal: %v", e)
	}
	if _, has := got["requestId"]; !has {
		t.Fatalf("requestId 应在顶层: %s", w.Body.String())
	}
}

func TestOKNilStillCarriesDataField(t *testing.T) {
	w := httptest.NewRecorder()
	OK(nil).WriteJSON(w, "req_y")
	if !strings.Contains(w.Body.String(), `"data"`) {
		t.Fatalf("成功信封必须带 data 字段: %s", w.Body.String())
	}
}

func TestTypedNilErrorIsDoesNotPanic(t *testing.T) {
	var e *Error
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("typed-nil 上 errors.Is 不得 panic: %v", r)
		}
	}()
	if errors.Is(e, New(CodeInternal, "x")) {
		t.Fatal("nil 不应等于任何错误")
	}
}

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/family"
)

const (
	profileTestChallengePrefix = "YZAGMGR_CHALLENGE_"
	profileTestChallengePrompt = "Reply with exactly "
	profileTestChallengeSuffix = " and nothing else."
)

func challengeFromProfileTestRequest(req profileTestExecRequest) (string, bool) {
	if len(req.Args) == 0 {
		return "", false
	}
	prompt := req.Args[len(req.Args)-1]
	if !strings.HasPrefix(prompt, profileTestChallengePrompt) || !strings.HasSuffix(prompt, profileTestChallengeSuffix) {
		return "", false
	}
	challenge := strings.TrimSuffix(strings.TrimPrefix(prompt, profileTestChallengePrompt), profileTestChallengeSuffix)
	if !strings.HasPrefix(challenge, profileTestChallengePrefix) || len(challenge) != len(profileTestChallengePrefix)+32 {
		return "", false
	}
	for _, c := range strings.TrimPrefix(challenge, profileTestChallengePrefix) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	return challenge, true
}

func profileTestSuccessOutput(cli, answer string) string {
	encodeLine := func(value any) string {
		blob, _ := json.Marshal(value)
		return string(blob)
	}
	switch cli {
	case "claude":
		return encodeLine(map[string]any{
			"type": "result", "subtype": "success", "is_error": false, "result": answer,
		})
	case "codex":
		return strings.Join([]string{
			encodeLine(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": answer}}),
			encodeLine(map[string]any{"type": "turn.completed"}),
		}, "\n") + "\n"
	case "opencode":
		return strings.Join([]string{
			encodeLine(map[string]any{"type": "step_start"}),
			encodeLine(map[string]any{"type": "text", "part": map[string]any{"type": "text", "text": answer, "time": map[string]any{"end": 1}}}),
			encodeLine(map[string]any{"type": "step_finish", "part": map[string]any{"type": "step-finish", "reason": "stop"}}),
		}, "\n") + "\n"
	case "pi":
		return piEventStream("deepseek-flash", "yzagmgr-local", answer)
	default:
		return ""
	}
}

// profileTestShellChallengePrelude is the setup for tiny local executable fixtures. It
// extracts the per-run challenge from the final prompt argument; it never launches a
// vendor CLI.
func profileTestShellChallengePrelude(challengeFile string) string {
	quotedChallengeFile := "'" + strings.ReplaceAll(challengeFile, "'", "'\\''") + "'"
	return "#!/bin/sh\nset -e\n" +
		"prompt=\nfor arg do prompt=$arg; done\n" +
		"case \"$prompt\" in \"Reply with exactly YZAGMGR_CHALLENGE_\"*\" and nothing else.\") ;; *) exit 91 ;; esac\n" +
		"answer=\u0024{prompt#Reply with exactly }\n" +
		"answer=\u0024{answer% and nothing else.}\n" +
		"printf '%s\\n' \"$answer\" > " + quotedChallengeFile + "\n"
}

func profileTestShellStructuredAnswer(cli string) string {
	switch cli {
	case "claude":
		return `printf '{"type":"result","subtype":"success","is_error":false,"result":"%s"}\n' "$answer"` + "\n"
	case "pi":
		return `printf '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"%s"}],"provider":"stub-stream-provider","model":"stub-stream-model","responseModel":"stub-stream-model"}}\n' "$answer"` + "\n"
	default:
		return ""
	}
}

func assertProfileTestMachineReadableArgs(t *testing.T, req profileTestExecRequest, cli string) {
	t.Helper()
	hasPair := func(left, right string) bool {
		for i := 0; i+1 < len(req.Args); i++ {
			if req.Args[i] == left && req.Args[i+1] == right {
				return true
			}
		}
		return false
	}
	switch cli {
	case "claude":
		if len(req.Args) == 0 {
			t.Errorf("Claude legacy recipe is empty")
			return
		}
		assertClaudeTestArgs(t, req.Args, req.Args[len(req.Args)-1])
	case "codex":
		if len(req.Args) < 2 || req.Args[0] != "exec" || req.Args[1] != "--skip-git-repo-check" ||
			!containsString(req.Args, "--json") {
			t.Errorf("Codex legacy recipe must retain exec trust shape and enable JSON events: %q", req.Args)
		}
	case "opencode":
		if len(req.Args) == 0 || req.Args[0] != "run" || !hasPair("--format", "json") {
			t.Errorf("OpenCode legacy recipe must request JSON answer events: %q", req.Args)
		}
	case "pi":
		if !hasPair("--mode", "json") {
			t.Errorf("Pi legacy recipe must retain JSON event mode: %q", req.Args)
		}
	default:
		t.Fatalf("test has no frozen machine-readable recipe for %q", cli)
	}
}

func assertClaudeTestArgs(t *testing.T, args []string, prompt string) {
	t.Helper()
	want := []string{
		"-p", "--output-format", "json",
		"--tools", "", "--disallowedTools", "mcp__*", "--max-turns", "1",
		"--no-session-persistence", "--settings", `{"disableAllHooks":true}`, prompt,
	}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("Claude one-turn, no-tools recipe args = %q, want %q", args, want)
	}
}

func profileTestResponseOf(t *testing.T, srv *contractServer, tool string) (envelope, profileTestPayload) {
	return profileTestResponseForCard(t, srv, tool, managedCardID)
}

func profileTestResponseForCard(t *testing.T, srv *contractServer, tool, profileID string) (envelope, profileTestPayload) {
	t.Helper()
	res := srv.callRaw(t, http.MethodPost,
		"/api/v1/tools/"+tool+"/profiles/"+profileID+"/test", "", true)
	if !res.hasData {
		t.Fatalf("profile test response omitted data: %s", res.body)
	}
	var payload profileTestPayload
	if err := json.Unmarshal(res.data, &payload); err != nil {
		t.Fatalf("decode profile test response: %v (%s)", err, res.body)
	}
	return res, payload
}

func assertProfileTestRunDoesNotLeak(t *testing.T, srv *contractServer, response envelope, challenge, secret string) {
	t.Helper()
	if challenge != "" && strings.Contains(response.body, challenge) {
		t.Errorf("test response leaked per-run challenge %q: %s", challenge, response.body)
	}
	if secret != "" && strings.Contains(response.body, secret) {
		t.Errorf("test response leaked synthetic secret: %s", response.body)
	}
	logs := srv.callRaw(t, http.MethodGet,
		"/api/v1/operations?limit=100&requestId="+url.QueryEscape(response.reqID), "", true)
	if !logs.ok {
		t.Fatalf("read operation log for profile test: %s", logs.body)
	}
	if challenge != "" && strings.Contains(logs.body, challenge) {
		t.Errorf("operation log leaked per-run challenge %q: %s", challenge, logs.body)
	}
	if secret != "" && strings.Contains(logs.body, secret) {
		t.Errorf("operation log leaked synthetic secret: %s", logs.body)
	}
}

func TestProfileTestUsesFreshAnswerChallengeForEveryCLI(t *testing.T) {
	assertClaudeTestArgs(t, profileTestRecipes[family.Claude].args, profileTestPrompt)
	if validProfileTestAnswerFor(profileTestSuccessOutput("pi", "answer"), "pi", " \t") {
		t.Error("empty expected challenge must fail closed")
	}
	emptyRecipe := profileTestRecipe{cli: "empty-fixture"}
	if got := profileTestRecipeWithPrompt(emptyRecipe, "replacement"); got.cli != emptyRecipe.cli || len(got.args) != 0 {
		t.Errorf("empty-argument recipe should be returned unchanged: got=%+v", got)
	}
	originalArgs := strings.Join(profileTestRecipes[family.Claude].args, "\x00")
	clone := profileTestRecipeWithPrompt(profileTestRecipes[family.Claude], "replacement prompt")
	if len(clone.args) == 0 || clone.args[len(clone.args)-1] != "replacement prompt" {
		t.Errorf("recipe did not replace exactly the final prompt: %q", clone.args)
	}
	if strings.Join(profileTestRecipes[family.Claude].args, "\x00") != originalArgs {
		t.Fatal("prompt replacement mutated the reusable Claude recipe")
	}
	clone.args[0] = "mutated clone"
	if profileTestRecipes[family.Claude].args[0] == "mutated clone" {
		t.Fatal("prompt replacement did not clone the source argument slice")
	}

	for _, tool := range []string{"claude", "codex", "opencode", "pi"} {
		t.Run(tool, func(t *testing.T) {
			srv, fake := testableServer(t)
			fake.respond = func(req profileTestExecRequest) profileTestExecResult {
				challenge, ok := challengeFromProfileTestRequest(req)
				if !ok {
					// Keep the pre-change test executable so the failure is an assertion about the
					// missing per-run challenge, not an unrelated parser/fixture error.
					return profileTestExecResult{
						Stdout: piEventStream("deepseek-flash", "yzagmgr-local", "正常"), ExitCode: 0,
					}
				}
				return profileTestExecResult{Stdout: profileTestSuccessOutput(tool, challenge), ExitCode: 0}
			}

			challenges := make([]string, 0, 2)
			for i := 0; i < 2; i++ {
				response, payload := profileTestResponseOf(t, srv, tool)
				calls := fake.recorded()
				if len(calls) != i+1 {
					t.Fatalf("CLI invocation count=%d after run %d", len(calls), i+1)
				}
				assertProfileTestMachineReadableArgs(t, calls[i], tool)
				challenge, ok := challengeFromProfileTestRequest(calls[i])
				if !ok {
					t.Errorf("%s run %d did not use a fresh-answer prompt: args=%q", tool, i+1, calls[i].Args)
				} else {
					challenges = append(challenges, challenge)
				}
				if payload.Status != profileTestStatusOK {
					t.Errorf("valid %s challenge response status=%q reason=%q output=%q", tool, payload.Status, payload.Reason, payload.Output)
				}
				if payload.Output == "" || strings.Contains(payload.Output, profileTestChallengePrompt) {
					t.Errorf("%s result should expose a safe summary, not its challenge prompt: %q", tool, payload.Output)
				}
				assertProfileTestRunDoesNotLeak(t, srv, response, challenge, srv.TokenSeed)
			}
			if len(challenges) == 2 && challenges[0] == challenges[1] {
				t.Errorf("%s reused a prior run's challenge %q", tool, challenges[0])
			}
		})
	}
}

func TestProfileTestExitZeroRequiresAValidCurrentAnswer(t *testing.T) {
	cases := []struct {
		name   string
		result func(challenge string) profileTestExecResult
	}{
		{
			name: "no answer",
			result: func(string) profileTestExecResult {
				return profileTestExecResult{ExitCode: 0}
			},
		},
		{
			name: "wrong challenge answer",
			result: func(string) profileTestExecResult {
				return profileTestExecResult{Stdout: piEventStream("deepseek-flash", "yzagmgr-local", "YZAGMGR_CHALLENGE_00000000000000000000000000000000"), ExitCode: 0}
			},
		},
		{
			name: "error envelope with zero exit",
			result: func(string) profileTestExecResult {
				return profileTestExecResult{Stdout: `{"type":"result","subtype":"error","is_error":true,"result":"request rejected"}`, ExitCode: 0}
			},
		},
		{
			name: "diagnostic text is not an answer",
			result: func(string) profileTestExecResult {
				return profileTestExecResult{Stdout: "authentication succeeded; request prepared", ExitCode: 0}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, fake := testableServer(t)
			fake.respond = func(req profileTestExecRequest) profileTestExecResult {
				challenge, _ := challengeFromProfileTestRequest(req)
				return tc.result(challenge)
			}
			response, payload := profileTestResponseOf(t, srv, "pi")
			if payload.Status != profileTestStatusFailed {
				t.Errorf("exit-zero %q was reported %q, want failed (reason=%q output=%q)", tc.name, payload.Status, payload.Reason, payload.Output)
			}
			challenge, _ := challengeFromProfileTestRequest(fake.recorded()[0])
			assertProfileTestRunDoesNotLeak(t, srv, response, challenge, srv.TokenSeed)
		})
	}

	t.Run("nonzero exit cannot pass even with a valid-looking envelope", func(t *testing.T) {
		srv, fake := testableServer(t)
		fake.respond = func(req profileTestExecRequest) profileTestExecResult {
			challenge, _ := challengeFromProfileTestRequest(req)
			return profileTestExecResult{
				Stdout: profileTestSuccessOutput("pi", challenge), ExitCode: 1,
				Stderr: "nonzero exit after apparent answer",
			}
		}
		response, payload := profileTestResponseOf(t, srv, "pi")
		if payload.Status != profileTestStatusFailed {
			t.Errorf("nonzero exit was reported %q, want failed", payload.Status)
		}
		challenge, _ := challengeFromProfileTestRequest(fake.recorded()[0])
		assertProfileTestRunDoesNotLeak(t, srv, response, challenge, srv.TokenSeed)
	})

	srv, fake := testableServer(t)
	const executorErrorSentinel = "synthetic executor error details must stay private"
	fake.respond = func(req profileTestExecRequest) profileTestExecResult {
		challenge, _ := challengeFromProfileTestRequest(req)
		return profileTestExecResult{
			Stdout: profileTestSuccessOutput("pi", challenge), Stderr: executorErrorSentinel,
			ExitCode: 0, Err: errors.New(executorErrorSentinel),
		}
	}
	response, payload := profileTestResponseOf(t, srv, "pi")
	if payload.Status != profileTestStatusFailed || payload.Reason != "CLI 执行未成功完成" {
		t.Errorf("exit-zero executor error should remain a generic failure: status=%q reason=%q", payload.Status, payload.Reason)
	}
	if payload.Output != "原始输出已脱敏并隐藏" {
		t.Errorf("executor error response exposed raw output: %q", payload.Output)
	}
	challenge, _ := challengeFromProfileTestRequest(fake.recorded()[0])
	assertProfileTestRunDoesNotLeak(t, srv, response, challenge, executorErrorSentinel)
}

func TestProfileTestCancelWinsTimeoutAndDoesNotExposeChallengeOutput(t *testing.T) {
	srv, _ := testableServer(t)
	entered := make(chan profileTestExecRequest, 1)
	release := make(chan struct{})
	secret := srv.TokenSeed
	srv.profileTestExecute = func(ctx context.Context, req profileTestExecRequest) profileTestExecResult {
		entered <- req
		select {
		case <-release:
		case <-ctx.Done():
			// The fake models the CLI process returning a timeout after the stop signal.
		}
		challenge, _ := challengeFromProfileTestRequest(req)
		return profileTestExecResult{
			Stdout: profileTestSuccessOutput("pi", challenge), Stderr: secret + " diagnostic",
			ExitCode: -1, TimedOut: true, Err: errors.New("synthetic timed out process"),
		}
	}
	requestDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/tools/pi/profiles/"+managedCardID+"/test", nil)
		req.Header.Set("X-Agmgr-Token", srv.token)
		recorder := httptest.NewRecorder()
		srv.ServeHTTP(recorder, req)
		requestDone <- recorder
	}()
	var execRequest profileTestExecRequest
	select {
	case execRequest = <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("profile test never entered its executor")
	}
	challenge, ok := challengeFromProfileTestRequest(execRequest)
	if !ok {
		t.Errorf("profile test did not use a challenge prompt: %q", execRequest.Args)
	}
	_, stopped := cancelTestRun(t, srv, "pi")
	close(release)
	if !stopped.Stopped || stopped.Result == nil || stopped.Result.Status != profileTestStatusCancelled {
		t.Fatalf("cancel should win over timeout: %+v", stopped)
	}
	var recorder *httptest.ResponseRecorder
	select {
	case recorder = <-requestDone:
	case <-time.After(5 * time.Second):
		t.Fatal("profile test response did not settle after cancellation")
	}
	var response struct {
		OK        bool                `json:"ok"`
		Data      *profileTestPayload `json:"data"`
		RequestID string              `json:"requestId"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.Data == nil {
		t.Fatalf("decode cancelled profile test response: %v (%s)", err, recorder.Body.String())
	}
	if response.Data.Status != profileTestStatusCancelled || !response.Data.Cancelled {
		t.Errorf("terminal result must be cancelled despite executor timeout: %+v", response.Data)
	}
	if strings.Contains(recorder.Body.String(), secret) || (challenge != "" && strings.Contains(recorder.Body.String(), challenge)) {
		t.Errorf("cancel response leaked synthetic secret or challenge: %s", recorder.Body.String())
	}
	logs := srv.callRaw(t, http.MethodGet,
		"/api/v1/operations?limit=100&requestId="+url.QueryEscape(response.RequestID), "", true)
	if !logs.ok {
		t.Fatalf("read cancelled-operation log: %s", logs.body)
	}
	if strings.Contains(logs.body, secret) || (challenge != "" && strings.Contains(logs.body, challenge)) {
		t.Errorf("cancel operation log leaked synthetic secret or challenge: %s", logs.body)
	}
}

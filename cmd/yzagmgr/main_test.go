// cmd/yzagmgr 的装配期单测：只覆盖「不启动进程也能判定」的部分。
//
// 重点是一个真实缺陷的回归钉：策略种子的落盘形态一旦经过 `json.Marshal`，
// `catalogs` 的空白就会被压平，而契约要求数据面下发的目录与策略载荷**逐字节等同**
// （§7 / UC-007 A6）。这类丢字节**用规范空白的载荷测不出来**，所以夹具刻意用不规则空白。
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/logging"
	"github.com/peterplaykkk/yzagmgr/internal/policy"
	"github.com/peterplaykkk/yzagmgr/internal/proxy"
	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

// irregularPayload 是带不规则空白与特殊数字字面量的策略载荷。
const irregularPayload = `{"version":5,"defaultModel":"deepseek-flash",` +
	`"models":[{"id":"deepseek-flash","name":"DeepSeek Flash"}],` +
	`"catalogs":{ "codex" : [ {"zId":"first","n":1e6},   {"aId":"second","n":1.50} ]  }}`

// irregularCatalog 是上面那份载荷里 **codex 键的值**的原始字节（含全部空白）。
// 注意不含 `"codex":` 本身 —— 那是 catalogs 对象的成员名，不是目录值的一部分。
const irregularCatalog = `[ {"zId":"first","n":1e6},   {"aId":"second","n":1.50} ]`

// Startup wiring coverage boundary: only run's invalid-allowlist early return is
// exercised here. Its full startup path and assemble belong to the isolated daemon
// fixture: run writes token/log files, binds loopback ports, and starts background
// work; assemble opens the usage database and constructs real runtime clients.

func TestParseVersionProbeFamilies(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		set     bool
		want    []family.ID
		wantErr bool
	}{
		{name: "unset keeps production default", want: nil},
		{name: "explicit empty disables probing", set: true, want: []family.ID{}},
		{
			name:  "trim spaces and deduplicate in registry order",
			value: "pi, opencode,claude,pi",
			set:   true,
			want:  []family.ID{family.Claude, family.OpenCode, family.Pi},
		},
		{name: "unknown family fails closed", value: "pi,codexx", set: true, wantErr: true},
		{name: "empty list item fails closed", value: "pi,,claude", set: true, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseVersionProbeFamilies(tt.value, tt.set)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseVersionProbeFamilies() error = %v, wantErr %t", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseVersionProbeFamilies() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestVersionProbeFamiliesFromEnv(t *testing.T) {
	original, wasSet := os.LookupEnv(versionProbeFamiliesEnvVar)
	t.Cleanup(func() {
		if wasSet {
			_ = os.Setenv(versionProbeFamiliesEnvVar, original)
		} else {
			_ = os.Unsetenv(versionProbeFamiliesEnvVar)
		}
	})

	tests := []struct {
		name  string
		value string
		set   bool
		want  []family.ID
	}{
		{name: "unset keeps production default", want: nil},
		{name: "set reads allowlist", value: "opencode,claude", set: true, want: []family.ID{family.Claude, family.OpenCode}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				if err := os.Setenv(versionProbeFamiliesEnvVar, tt.value); err != nil {
					t.Fatalf("set %s: %v", versionProbeFamiliesEnvVar, err)
				}
			} else if err := os.Unsetenv(versionProbeFamiliesEnvVar); err != nil {
				t.Fatalf("unset %s: %v", versionProbeFamiliesEnvVar, err)
			}
			got, err := versionProbeFamiliesFromEnv()
			if err != nil {
				t.Fatalf("versionProbeFamiliesFromEnv() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("versionProbeFamiliesFromEnv() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestRunRejectsInvalidVersionProbeFamiliesBeforeSideEffects(t *testing.T) {
	apiReservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve isolated API port: %v", err)
	}
	defer func() { _ = apiReservation.Close() }()
	proxyReservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve isolated proxy port: %v", err)
	}
	defer func() { _ = proxyReservation.Close() }()

	isolatedHome := filepath.Join(t.TempDir(), "home")
	t.Setenv(config.EnvHomeVar, isolatedHome)
	t.Setenv(versionProbeFamiliesEnvVar, "pi,not-a-family")
	t.Setenv("YZAGMGR_API_PORT", strconv.Itoa(apiReservation.Addr().(*net.TCPAddr).Port))
	t.Setenv("YZAGMGR_PROXY_PORT", strconv.Itoa(proxyReservation.Addr().(*net.TCPAddr).Port))
	t.Setenv("YZAGMGR_NO_TRAY", "1")

	err = run()
	if err == nil || !strings.Contains(err.Error(), versionProbeFamiliesEnvVar) || !strings.Contains(err.Error(), "unknown family ID") {
		t.Fatalf("run() error = %v; want the invalid %s error before startup side effects", err, versionProbeFamiliesEnvVar)
	}

	dataRoot := filepath.Join(isolatedHome, ".yzagmgr", "data")
	for _, path := range []string{dataRoot, filepath.Join(dataRoot, "admin.token")} {
		if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("run() created startup state at %q before rejecting the allowlist (stat error: %v)", path, statErr)
		}
	}

	// The test holds both configured ports throughout run(). Rebinding after
	// releasing each reservation confirms no daemon listener was left behind.
	for _, reservation := range []net.Listener{apiReservation, proxyReservation} {
		address := reservation.Addr().String()
		if err := reservation.Close(); err != nil {
			t.Fatalf("release test port %s: %v", address, err)
		}
		listener, err := net.Listen("tcp", address)
		if err != nil {
			t.Errorf("run() left a listener at %s: %v", address, err)
			continue
		}
		_ = listener.Close()
	}
}

func TestSeedPolicyDocumentKeepsPayloadBytesExact(t *testing.T) {
	document, err := seedPolicyDocument([]byte(irregularPayload), "https://gw.invalid",
		time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("构造策略种子文档失败: %v", err)
	}

	// 1) 载荷原文必须**逐字节**出现在文档里（这是 json.Marshal 会破坏的那条性质）。
	if !containsBytes(document, []byte(irregularPayload)) {
		t.Fatalf("种子文档里的载荷被改写了：\n%q\n原载荷 %q", document, irregularPayload)
	}
	// 2) 文档必须是合法 JSON，且 payload 字段解码后与原载荷逐字节相等。
	var envelope struct {
		Schema    int             `json:"schema"`
		Origin    string          `json:"origin"`
		FetchedAt string          `json:"fetchedAt"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(document, &envelope); err != nil {
		t.Fatalf("种子文档不是合法 JSON: %v\n%s", err, document)
	}
	if envelope.Schema != 1 || envelope.Origin != "https://gw.invalid" || envelope.FetchedAt == "" {
		t.Errorf("种子文档表头不对: %+v", envelope)
	}
	if string(envelope.Payload) != irregularPayload {
		t.Errorf("payload 字段被改写了：\n  实际 %q\n  期望 %q", envelope.Payload, irregularPayload)
	}
}

// TestSeedPolicyDocumentSurvivesPolicyStore 是端到端那半：
// 种子文档经策略仓库读回后，目录的原始字节必须一字不差 —— 这一层过了，
// 数据面 `/<prefix>/v1/models` 的「逐字节等同」才有前提。
func TestSeedPolicyDocumentSurvivesPolicyStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "managed", "policy.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	document, err := seedPolicyDocument([]byte(irregularPayload), "https://gw.invalid", time.Now())
	if err != nil {
		t.Fatalf("构造策略种子文档失败: %v", err)
	}
	if err := os.WriteFile(path, document, 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	// client 传 nil：Load 只读盘，不出网。
	store := policy.NewStore(path, policy.Options{})
	loaded, err := store.Load()
	if err != nil || loaded == nil {
		t.Fatalf("读回策略失败: %v %v", loaded, err)
	}
	if loaded.DefaultModel != "deepseek-flash" || loaded.Version != 5 {
		t.Errorf("读回的策略与种子不符: %+v", loaded)
	}
	raw, ok := loaded.Catalogs["codex"]
	if !ok {
		t.Fatalf("读回的策略里没有 codex 目录: %+v", loaded.Catalogs)
	}
	if string(raw) != irregularCatalog {
		t.Errorf("目录的原始字节被改写了（空白被压平？）:\n  实际 %q\n  期望 %q", raw, irregularCatalog)
	}
}

// TestSeedPolicyDocumentRejectsNothing 只是把「表头 json 编码 + 载荷手工拼接」这条缝钉住：
// 载荷里出现 `"payload":` 字样或非 ASCII 也不得让文档崩掉（手工拼接的经典坑）。
func TestSeedPolicyDocumentHandlesEdgePayloads(t *testing.T) {
	for _, payload := range []string{
		`{"catalogs":{}}`,
		`{"note":"payload: 出现在字符串里","catalogs":{}}`,
		`{"note":"中文与 emoji 🙂","catalogs":{}}`,
	} {
		document, err := seedPolicyDocument([]byte(payload), "https://gw.invalid", time.Now())
		if err != nil {
			t.Fatalf("载荷 %q 构造失败: %v", payload, err)
		}
		var envelope struct {
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(document, &envelope); err != nil {
			t.Fatalf("载荷 %q 的文档不是合法 JSON: %v", payload, err)
		}
		if string(envelope.Payload) != payload {
			t.Errorf("载荷 %q 被改写为 %q", payload, envelope.Payload)
		}
	}
}

const (
	darwinTLSFallbackChildModeEnv = "YZAGMGR_TEST_TLS_FALLBACK_CHILD_MODE"
	darwinTLSFallbackModeEnv      = "YZAGMGR_TEST_TLS_FALLBACK_MODE"
	darwinTLSFallbackPeerEnv      = "YZAGMGR_TEST_TLS_FALLBACK_PEER"
	darwinTLSFallbackOtherPeerEnv = "YZAGMGR_TEST_TLS_FALLBACK_OTHER_PEER"
	darwinTLSFallbackServerEnv    = "YZAGMGR_TEST_TLS_FALLBACK_SERVER_NAME"
	darwinTLSFallbackErrorEnv     = "YZAGMGR_TEST_TLS_FALLBACK_EXPECT_ERROR"
	darwinTLSFallbackSentinelEnv  = "YZAGMGR_TEST_TLS_FALLBACK_ERROR_SENTINEL"
)

func TestDarwinFallbackRootsSubprocess(t *testing.T) {
	if os.Getenv(darwinTLSFallbackChildModeEnv) != "" {
		runDarwinFallbackRootsChild(t, os.Getenv(darwinTLSFallbackModeEnv))
		return
	}
	if runtime.GOOS != "darwin" {
		t.Skip("Go Darwin system-root behavior is the contract under test")
	}
	if !strings.HasPrefix(runtime.Version(), "go1.25.") {
		t.Skip("the platform compatibility assertion is pinned to Go 1.25")
	}

	parentEnv := snapshotFallbackRootsParentEnvironment()
	privateRoot := t.TempDir()
	privateHome := filepath.Join(privateRoot, "home")
	if err := os.MkdirAll(privateHome, 0o700); err != nil {
		t.Fatalf("create child private HOME: %v", err)
	}
	goodCertificate := fallbackRootsOtherTLSCertificate(t)
	goodPeer := startFallbackRootsTLSPeer(t, &goodCertificate)
	otherCertificate := fallbackRootsOtherTLSCertificate(t)
	wrongPeer := startFallbackRootsTLSPeer(t, &otherCertificate)
	goodBundle := writeFallbackRootsBundle(t, privateRoot, "good-root.pem", goodPeer.Certificate())
	wrongBundle := writeFallbackRootsBundle(t, privateRoot, "other-root.pem", wrongPeer.Certificate())
	emptyBundle := filepath.Join(privateRoot, "empty-root.pem")
	if err := os.WriteFile(emptyBundle, nil, 0o600); err != nil {
		t.Fatalf("create empty private certificate bundle: %v", err)
	}
	invalidBundle := filepath.Join(privateRoot, "invalid-root.pem")
	const invalidBundleSentinel = "synthetic-invalid-pem-content-sentinel"
	if err := os.WriteFile(invalidBundle, []byte(invalidBundleSentinel), 0o600); err != nil {
		t.Fatalf("create malformed private certificate bundle: %v", err)
	}
	missingBundle := filepath.Join(privateRoot, "missing-root.pem")
	if _, err := os.Lstat(missingBundle); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing-bundle fixture unexpectedly exists: %v", err)
	}

	cases := []struct {
		name       string
		mode       string
		debug      string
		bundle     string
		peer       string
		otherPeer  string
		serverName string
		wantError  bool
		sentinel   string
	}{
		{
			name: "exact opt-in trusts only the supplied local CA",
			mode: "tls-success", debug: "x509usefallbackroots=1", bundle: goodBundle,
			peer: goodPeer.URL,
		},
		{
			name: "wrong server name is rejected after trusting its CA",
			mode: "tls-wrong-san", debug: "x509usefallbackroots=1", bundle: goodBundle,
			peer: goodPeer.URL, serverName: "wrong.invalid",
		},
		{
			name: "other private CA authenticates only its own peer",
			mode: "tls-other-ca", debug: "x509usefallbackroots=1", bundle: wrongBundle,
			peer: wrongPeer.URL, otherPeer: goodPeer.URL,
		},
		{
			name: "SSL_CERT_FILE alone is a no-op and does not read even a missing path",
			mode: "setter-unused", bundle: missingBundle,
		},
		{
			name: "flag zero is a no-op",
			mode: "setter-unused", debug: "x509usefallbackroots=0", bundle: goodBundle,
		},
		{
			name: "repeated GODEBUG values use final one",
			mode: "tls-success", debug: "x509usefallbackroots=0,x509usefallbackroots=1",
			bundle: goodBundle, peer: goodPeer.URL,
		},
		{
			name: "repeated GODEBUG final zero leaves platform trust untouched",
			mode: "setter-unused", debug: "x509usefallbackroots=1,x509usefallbackroots=0", bundle: goodBundle,
		},
		{
			name: "opt-in with no certificate path is a no-op",
			mode: "setter-unused", debug: "x509usefallbackroots=1",
		},
		{
			name: "missing bundle is a safe error and does not consume the setter",
			mode: "config-error", debug: "x509usefallbackroots=1", bundle: missingBundle, wantError: true,
		},
		{
			name: "empty bundle is a safe error and does not consume the setter",
			mode: "config-error", debug: "x509usefallbackroots=1", bundle: emptyBundle, wantError: true,
		},
		{
			name: "malformed bundle is a safe error without echoing its content",
			mode: "config-error", debug: "x509usefallbackroots=1", bundle: invalidBundle, wantError: true,
			sentinel: invalidBundleSentinel,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runDarwinFallbackRootsSubprocess(t, privateRoot, privateHome, tc.mode, tc.debug,
				tc.bundle, tc.peer, tc.otherPeer, tc.serverName, tc.wantError, tc.sentinel)
		})
	}

	if after := snapshotFallbackRootsParentEnvironment(); !reflect.DeepEqual(after, parentEnv) {
		t.Errorf("subprocess fixture changed parent GODEBUG/certificate environment")
	}

	// run() must reject an opted-in unreadable bundle before it opens listeners,
	// prepares user roots, or starts background services. Keep this in the parent
	// test process so -coverprofile counts the production early-return path.
	t.Setenv("GODEBUG", "x509usefallbackroots=1")
	runMissingBundle := filepath.Join(privateRoot, "missing-before-run.pem")
	t.Setenv("SSL_CERT_FILE", runMissingBundle)
	if _, err := os.Lstat(runMissingBundle); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("early-run missing-bundle fixture unexpectedly exists: %v", err)
	}
	runErr := run()
	if runErr == nil || !strings.Contains(runErr.Error(), "readable SSL_CERT_FILE certificate bundle") {
		t.Fatalf("run should fail closed before startup on an unreadable opted-in bundle: %v", runErr)
	}
	if _, err := os.Lstat(runMissingBundle); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("run early-failure path created an unexpected bundle: %v", err)
	}
}

func runDarwinFallbackRootsSubprocess(
	t *testing.T,
	privateRoot, privateHome, mode, godebugValue, bundle, peer, otherPeer, serverName string,
	wantError bool,
	sentinel string,
) {
	t.Helper()
	childArgs := []string{"-test.run=^TestDarwinFallbackRootsSubprocess$"}
	if coverageDir := goTestCoverageDirectory(); coverageDir != "" {
		// Go 1.25's test runner writes counters for a child test binary into the
		// same private coverage directory when it receives -test.gocoverdir.
		childArgs = append(childArgs, "-test.gocoverdir="+coverageDir)
	}
	command := exec.Command(os.Args[0], childArgs...)
	childEnv := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + privateHome,
		"TMPDIR=" + privateRoot,
		darwinTLSFallbackChildModeEnv + "=1",
		darwinTLSFallbackModeEnv + "=" + mode,
		darwinTLSFallbackPeerEnv + "=" + peer,
		darwinTLSFallbackOtherPeerEnv + "=" + otherPeer,
		darwinTLSFallbackServerEnv + "=" + serverName,
		darwinTLSFallbackErrorEnv + "=" + strconv.FormatBool(wantError),
		darwinTLSFallbackSentinelEnv + "=" + sentinel,
	}
	if godebugValue != "" {
		childEnv = append(childEnv, "GODEBUG="+godebugValue)
	}
	if bundle != "" {
		childEnv = append(childEnv, "SSL_CERT_FILE="+bundle)
	}
	command.Env = childEnv
	output, err := command.CombinedOutput()
	if err != nil {
		safeOutput := string(output)
		for _, privateValue := range []string{privateRoot, privateHome, bundle, sentinel} {
			if privateValue != "" {
				safeOutput = strings.ReplaceAll(safeOutput, privateValue, "<private-test-value>")
			}
		}
		if sentinel != "" && strings.Contains(string(output), sentinel) {
			t.Errorf("child diagnostics leaked malformed bundle contents")
		}
		t.Fatalf("isolated TLS child mode %q failed: %v (%s)", mode, err, strings.TrimSpace(safeOutput))
	}
}

func goTestCoverageDirectory() string {
	const prefix = "-test.gocoverdir="
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, prefix) {
			return strings.TrimPrefix(arg, prefix)
		}
	}
	return ""
}

func runDarwinFallbackRootsChild(t *testing.T, mode string) {
	t.Helper()
	if runtime.GOOS != "darwin" || !strings.HasPrefix(runtime.Version(), "go1.25.") {
		t.Fatalf("unexpected TLS subprocess runtime %s/%s", runtime.GOOS, runtime.Version())
	}
	configureErr := configureProcessFallbackRoots()
	sentinel := os.Getenv(darwinTLSFallbackSentinelEnv)
	expectConfigError, err := strconv.ParseBool(os.Getenv(darwinTLSFallbackErrorEnv))
	if err != nil {
		t.Fatalf("invalid private child expectation marker")
	}
	if expectConfigError {
		if configureErr == nil {
			t.Fatalf("expected safe certificate configuration error")
		}
		if sentinel != "" && strings.Contains(configureErr.Error(), sentinel) {
			t.Fatalf("certificate configuration error echoed private bundle contents")
		}
		if !probeFallbackRootsSetterAvailable() {
			t.Fatalf("invalid certificate input consumed the one-shot roots setter")
		}
		return
	}
	if configureErr != nil {
		t.Fatalf("certificate configuration unexpectedly failed")
	}
	if mode == "setter-unused" {
		if !probeFallbackRootsSetterAvailable() {
			t.Fatalf("disabled/missing-path configuration consumed the one-shot roots setter")
		}
		return
	}
	if mode != "tls-success" && mode != "tls-wrong-san" && mode != "tls-other-ca" {
		t.Fatalf("unknown private TLS child mode")
	}
	// If production failed to install the requested roots, consume the public one-shot setter
	// with an empty pool. GODEBUG=...=1 then forces pure-Go verification to fail closed, so a
	// private peer can never appear trusted by the host platform verifier in the RED case.
	_ = probeFallbackRootsSetterAvailable()
	peer := os.Getenv(darwinTLSFallbackPeerEnv)
	serverName := os.Getenv(darwinTLSFallbackServerEnv)
	switch mode {
	case "tls-success":
		if err := requestFallbackTLSPeer(peer, serverName); err != nil {
			t.Fatalf("private CA did not authenticate expected loopback TLS peer")
		}
	case "tls-wrong-san":
		err := requestFallbackTLSPeer(peer, serverName)
		var hostnameErr x509.HostnameError
		if !errors.As(err, &hostnameErr) {
			t.Fatalf("wrong SAN was not the TLS rejection (the supplied CA must first be trusted)")
		}
	case "tls-other-ca":
		if err := requestFallbackTLSPeer(peer, ""); err != nil {
			t.Fatalf("configured CA did not authenticate its matching peer")
		}
		otherPeer := os.Getenv(darwinTLSFallbackOtherPeerEnv)
		err := requestFallbackTLSPeer(otherPeer, "")
		var authorityErr x509.UnknownAuthorityError
		if !errors.As(err, &authorityErr) {
			t.Fatalf("unrelated CA was not the TLS rejection (peer must be reachable and CA setup active)")
		}
	default:
		t.Fatalf("unknown private TLS child mode")
	}
}

func requestFallbackTLSPeer(peer, serverName string) error {
	if peer == "" {
		return errors.New("missing private TLS peer")
	}
	config := &tls.Config{}
	if serverName != "" {
		config.ServerName = serverName
	}
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			Proxy:           nil,
			TLSClientConfig: config,
		},
	}
	response, err := client.Get(peer)
	if response != nil {
		_ = response.Body.Close()
		if err == nil && response.StatusCode != http.StatusNoContent {
			return errors.New("unexpected loopback TLS response status")
		}
	}
	return err
}

func probeFallbackRootsSetterAvailable() (available bool) {
	func() {
		defer func() { _ = recover() }()
		x509.SetFallbackRoots(x509.NewCertPool())
		available = true
	}()
	return available
}

func startFallbackRootsTLSPeer(t *testing.T, certificate *tls.Certificate) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	if certificate != nil {
		server.TLS = &tls.Config{Certificates: []tls.Certificate{*certificate}}
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func fallbackRootsOtherTLSCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate synthetic in-memory test certificate key: %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(735192),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create synthetic in-memory test certificate: %v", err)
	}
	privateKeyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal synthetic in-memory test certificate key: %v", err)
	}
	pair, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyDER}),
	)
	if err != nil {
		t.Fatalf("load synthetic in-memory test TLS pair: %v", err)
	}
	return pair
}

func writeFallbackRootsBundle(t *testing.T, root, name string, cert *x509.Certificate) string {
	t.Helper()
	path := filepath.Join(root, name)
	content := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write private public-certificate bundle: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("private public-certificate bundle permissions are not 0600: info=%v err=%v", info, err)
	}
	return path
}

func snapshotFallbackRootsParentEnvironment() map[string]struct {
	value string
	set   bool
} {
	keys := []string{"GODEBUG", "SSL_CERT_FILE", "SSL_CERT_DIR", "HOME", "TMPDIR"}
	snapshot := make(map[string]struct {
		value string
		set   bool
	}, len(keys))
	for _, key := range keys {
		value, set := os.LookupEnv(key)
		snapshot[key] = struct {
			value string
			set   bool
		}{value: value, set: set}
	}
	return snapshot
}

func containsBytes(haystack, needle []byte) bool {
	return len(needle) == 0 || indexBytes(haystack, needle) >= 0
}

func indexBytes(haystack, needle []byte) int {
outer:
	for i := 0; i+len(needle) <= len(haystack); i++ {
		for j := range needle {
			if haystack[i+j] != needle[j] {
				continue outer
			}
		}
		return i
	}
	return -1
}

// ── 进程日志（UC-006 A12） ──────────────────────────────────────────────────

// TestDaemonLogsToDataRootAndAnnouncesPath 守 UC-006 A12：守护进程的日志落在数据根下、
// 有硬上限（轮转有界）、stderr 同时保留，且**启动日志自己给出文件路径**。
//
// 这条用例的存在理由是一个真实事故：`open` 启动的进程没有终端，stderr 进黑洞，
// 一次 tray 动作以 unauthenticated 失败后 `data/logs/` 是空的 —— 想查却查不到，
// 连「文件在哪」都得靠猜。所以路径必须由进程自己宣告，而不是写在文档里。
func TestDaemonLogsToDataRootAndAnnouncesPath(t *testing.T) {
	root := t.TempDir()
	t.Setenv(config.EnvHomeVar, root)
	env, err := config.FromProcess()
	if err != nil {
		t.Fatalf("构造隔离环境失败: %v", err)
	}
	if !env.Isolated {
		t.Fatalf("YZAGMGR_HOME=%q 时 Isolated 应为真（否则会写到真实 home）", root)
	}
	if err := env.EnsureLayout(); err != nil {
		t.Fatalf("建立数据根失败: %v", err)
	}

	var stderrCopy bytes.Buffer
	logger, sink, err := buildLogger(env, &stderrCopy)
	if err != nil {
		t.Fatalf("装配日志失败: %v", err)
	}

	// 1) 路径只能来自数据根（隔离纪律：测试与验收都写自己的临时根）。
	dataRoot, err := filepath.Abs(env.DataRoot())
	if err != nil {
		t.Fatalf("取数据根绝对路径失败: %v", err)
	}
	logPath, err := filepath.Abs(env.LogFile())
	if err != nil {
		t.Fatalf("取日志绝对路径失败: %v", err)
	}
	if !strings.HasPrefix(logPath, dataRoot+string(filepath.Separator)) {
		t.Fatalf("日志路径 %q 不在数据根 %q 下", logPath, dataRoot)
	}

	// 2) 宣告行：启动日志里就有路径与上限 —— 这就是「可发现」的可判定形式。
	raw, err := os.ReadFile(env.LogFile())
	if err != nil {
		t.Fatalf("日志文件不存在（buildLogger 之后必须已经有它）: %v", err)
	}
	for _, want := range []string{
		"进程日志已就绪",
		env.LogFile(),
		"maxBytes=" + strconv.FormatInt(logging.DefaultMaxBytes, 10),
		"keep=" + strconv.Itoa(logging.DefaultKeep),
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("启动日志里缺 %q: %s", want, raw)
		}
	}

	// 3) stderr 副本仍在：从终端跑时行为不变（MultiWriter 两份都写）。
	if !strings.Contains(stderrCopy.String(), "进程日志已就绪") {
		t.Errorf("stderr 副本丢了这条记录（终端里跑就没有日志了）: %q", stderrCopy.String())
	}

	// 4) 有界：写远超上限的量之后，文件数、单文件大小与盘上总量都在界内。
	pad := strings.Repeat("x", 900)
	const records = 4200 // 每条约 1 KiB ⇒ 约 4.2 MiB，越过 3 MiB 的界
	for i := 0; i < records; i++ {
		logger.Info("压力记录", "i", i, "pad", pad)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("关闭日志失败: %v", err)
	}

	entries, err := os.ReadDir(filepath.Dir(env.LogFile()))
	if err != nil {
		t.Fatalf("读日志目录失败: %v", err)
	}
	if len(entries) != logging.DefaultKeep+1 {
		t.Errorf("日志文件数 = %d，期望 %d（1 份当前 + %d 份轮转）", len(entries), logging.DefaultKeep+1, logging.DefaultKeep)
	}
	var total int64
	for _, e := range entries {
		st, err := e.Info()
		if err != nil {
			t.Fatalf("取 %s 信息失败: %v", e.Name(), err)
		}
		if st.Size() > logging.DefaultMaxBytes {
			t.Errorf("%s = %d 字节，超过单文件上限 %d", e.Name(), st.Size(), logging.DefaultMaxBytes)
		}
		total += st.Size()
	}
	if bound := int64(logging.DefaultKeep+1) * logging.DefaultMaxBytes; total > bound {
		t.Errorf("盘上总量 = %d，超过界 %d", total, bound)
	}
	// 轮转不是清空：最新一条必须还在（在界内不等于「什么都没留下」）。
	latest, err := os.ReadFile(env.LogFile())
	if err != nil {
		t.Fatalf("读当前日志失败: %v", err)
	}
	if !strings.Contains(string(latest), "i="+strconv.Itoa(records-1)) {
		t.Errorf("最新记录 i=%d 不在当前日志里", records-1)
	}
}

// TestDatabaseDSNFromEnv 覆盖 `YZAGMGR_DB_DSN` 的读法：未设/空白 ⇒ 空串（SQLite 形态），
// 设了就原样（去除首尾空白）传下去。
func TestDatabaseDSNFromEnv(t *testing.T) {
	t.Setenv("YZAGMGR_DB_DSN", "")
	if got := databaseDSN(); got != "" {
		t.Fatalf("未设置时应为空串（SQLite 形态），实际 %q", got)
	}
	t.Setenv("YZAGMGR_DB_DSN", "   ")
	if got := databaseDSN(); got != "" {
		t.Fatalf("全空白应视为未设置，实际 %q", got)
	}
	t.Setenv("YZAGMGR_DB_DSN", "  postgres://u@/db?host=/tmp  ")
	if got := databaseDSN(); got != "postgres://u@/db?host=/tmp" {
		t.Fatalf("应去除首尾空白后原样返回（DSN 语义由驱动解释），实际 %q", got)
	}
}

// TestProxyBindAddrFromEnv 覆盖 `YZAGMGR_PROXY_BIND` 的读法：未设 ⇒ 回环默认值；
// 设了就把**原值**交给 proxy.NewServer 去校验（形状规则只在一处，不在这里重写一份）。
func TestProxyBindAddrFromEnv(t *testing.T) {
	t.Setenv("YZAGMGR_PROXY_BIND", "")
	if got := proxyBindAddr(); got != proxy.DefaultListenAddr {
		t.Fatalf("未设置时应取回环默认值 %q，实际 %q", proxy.DefaultListenAddr, got)
	}
	t.Setenv("YZAGMGR_PROXY_BIND", " 172.17.0.1 ")
	if got := proxyBindAddr(); got != "172.17.0.1" {
		t.Fatalf("应去除首尾空白，实际 %q", got)
	}
}

// TestUsageRecorderCarriesArmIdentity 覆盖记账适配：代理交出的臂标识必须落到用量库的
// user_slug/session_id 两列（归集链路的最后一段）。
func TestUsageRecorderCarriesArmIdentity(t *testing.T) {
	home := t.TempDir()
	env := &config.Env{Home: home, Overrides: map[string]string{}, Isolated: true}
	if err := env.EnsureLayout(); err != nil {
		t.Fatalf("建立数据根失败: %v", err)
	}
	store, err := usage.Open(context.Background(), env.UsageDBFile(), usage.Options{})
	if err != nil {
		t.Fatalf("打开用量库失败: %v", err)
	}
	defer func() { _ = store.Close() }()

	rec := &usageRecorder{store: store}
	if err := rec.Record(proxy.RequestRecord{
		ID: "rq_rec01", At: time.Now(), App: "claude", Prefix: "glm", Provider: "glm",
		User: "user-a", Session: "9f2c1a4b7e0d3c55",
		ModelRequested: "glm-5.3-flash", Status: 200, DurationMs: 1200,
		InputTokens: 10, OutputTokens: 20, Streamed: true,
	}); err != nil {
		t.Fatalf("记账失败: %v", err)
	}
	page, err := store.Requests(context.Background(), usage.RequestsQuery{
		User: "user-a", Session: "9f2c1a4b7e0d3c55", Limit: 10,
	})
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if page.Total != 1 || page.Items[0].Provider != "glm" {
		t.Fatalf("按臂标识应读回刚记的那条：%+v", page)
	}
	// 用量库不可用（nil store）：记账静默跳过（代理侧另有日志），不得 panic。
	if err := (&usageRecorder{}).Record(proxy.RequestRecord{ID: "rq_rec02"}); err != nil {
		t.Fatalf("无库时记账应静默跳过：%v", err)
	}
}

// TestUsageMaintenanceSwitch 覆盖周期维护（UC-048 触发点 a）的装配语义：
// 节奏是冻结的 10 分钟；关闭开关按 off/0/false 判定；nil 库与 stop 都是安全空操作。
func TestUsageMaintenanceSwitch(t *testing.T) {
	if usageMaintenanceInterval != 10*time.Minute {
		t.Fatalf("周期维护节奏 = %v，期望 10 分钟（UC-048 冻结）", usageMaintenanceInterval)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("nil 库：空操作，stop 幂等", func(t *testing.T) {
		stop := startUsageMaintenance(nil, logger)
		if stop == nil {
			t.Fatal("stop 不得为 nil")
		}
		stop()
		stop()
	})

	t.Run("关闭开关", func(t *testing.T) {
		for _, value := range []string{"off", "OFF", " 0 ", "false", "False"} {
			t.Setenv(usageMaintenanceEnv, value)
			store, err := usage.Open(t.Context(), filepath.Join(t.TempDir(), "usage.db"), usage.Options{})
			if err != nil {
				t.Fatalf("打开用量库失败: %v", err)
			}
			stop := startUsageMaintenance(store, logger)
			stop()
			stop() // 幂等
			if err := store.Close(); err != nil {
				t.Fatalf("关闭用量库失败: %v", err)
			}
		}
	})

	t.Run("默认开启：可起可停", func(t *testing.T) {
		t.Setenv(usageMaintenanceEnv, "")
		store, err := usage.Open(t.Context(), filepath.Join(t.TempDir(), "usage.db"), usage.Options{})
		if err != nil {
			t.Fatalf("打开用量库失败: %v", err)
		}
		stop := startUsageMaintenance(store, logger)
		stop()
		if err := store.Close(); err != nil {
			t.Fatalf("关闭用量库失败: %v", err)
		}
	})
}

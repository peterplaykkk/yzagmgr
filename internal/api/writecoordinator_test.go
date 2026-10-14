package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const writeCoordinatorHelperEnv = "YZAGMGR_TEST_WRITE_COORDINATOR_HELPER"

type cancelOnErrContext struct {
	context.Context
	mu     sync.Mutex
	calls  int
	cancel int
}

type errorRandomReader struct{ err error }

func (r errorRandomReader) Read([]byte) (int, error) { return 0, r.err }

type operationRoundTripper func(*http.Request) (*http.Response, error)

func (f operationRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type gatedOperationBody struct {
	started chan struct{}
	release <-chan struct{}
	closed  chan struct{}
	reader  *bytes.Reader
	start   sync.Once
	close   sync.Once
}

type expiredWorkerBody struct {
	cancel context.CancelFunc
	err    error
	once   sync.Once
}

func (b *expiredWorkerBody) Read([]byte) (int, error) {
	b.once.Do(b.cancel)
	return 0, b.err
}

func (b *expiredWorkerBody) Close() error { return nil }

type cancelOnEOFBody struct {
	reader *bytes.Reader
	cancel context.CancelFunc
	done   bool
}

func (b *cancelOnEOFBody) Read(p []byte) (int, error) {
	if b.done {
		return 0, io.EOF
	}
	if n, err := b.reader.Read(p); n > 0 {
		return n, err
	}
	b.done = true
	b.cancel()
	return 0, io.EOF
}

func (b *cancelOnEOFBody) Close() error { return nil }

func (b *gatedOperationBody) Read(p []byte) (int, error) {
	b.start.Do(func() { close(b.started) })
	<-b.release
	return b.reader.Read(p)
}

func (b *gatedOperationBody) Close() error {
	b.close.Do(func() { close(b.closed) })
	return nil
}

func (c *cancelOnErrContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls >= c.cancel {
		return context.Canceled
	}
	return nil
}

// TestWriteCoordinatorSubprocessHelper is invoked only by the cross-process
// fencing test below. The helper is a real independent process using the same
// temporary data root and OS lock implementation as production.
func TestWriteCoordinatorSubprocessHelper(t *testing.T) {
	if os.Getenv(writeCoordinatorHelperEnv) != "1" {
		return
	}
	root := os.Getenv("YZAGMGR_TEST_WRITE_COORDINATOR_ROOT")
	attempting := os.Getenv("YZAGMGR_TEST_WRITE_COORDINATOR_ATTEMPTING")
	ready := os.Getenv("YZAGMGR_TEST_WRITE_COORDINATOR_READY")
	if root == "" || attempting == "" || ready == "" {
		t.Fatal("incomplete isolated coordinator helper parameters")
	}
	if err := os.WriteFile(attempting, []byte("attempting"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newWriteCoordinator(root, "")
	instance, err := c.Startup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ready, []byte(instance), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWriteCoordinatorWaitsForCrossProcessWriterThenFencesOldGeneration(t *testing.T) {
	root := t.TempDir()
	old := newWriteCoordinator(root, "")
	oldInstance, err := old.Startup(context.Background())
	if err != nil {
		t.Fatalf("initial isolated startup: %v", err)
	}
	releaseOldWriter, err := old.AcquireShared(context.Background(), oldInstance)
	if err != nil {
		t.Fatalf("old generation shared writer admission: %v", err)
	}
	defer releaseOldWriter()

	attempting := filepath.Join(t.TempDir(), "attempting")
	ready := filepath.Join(filepath.Dir(attempting), "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestWriteCoordinatorSubprocessHelper$")
	cmd.Env = append(os.Environ(),
		writeCoordinatorHelperEnv+"=1",
		"YZAGMGR_TEST_WRITE_COORDINATOR_ROOT="+root,
		"YZAGMGR_TEST_WRITE_COORDINATOR_ATTEMPTING="+attempting,
		"YZAGMGR_TEST_WRITE_COORDINATOR_READY="+ready,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start isolated helper process: %v", err)
	}
	finished := false
	defer func() {
		if !finished {
			_ = cmd.Process.Kill() // only this test's child process
			_ = cmd.Wait()
		}
	}()
	waitForCoordinatorFile(t, attempting, 5*time.Second)
	if _, err := os.Stat(ready); !os.IsNotExist(err) {
		t.Fatalf("new generation crossed an active old writer: ready stat err=%v", err)
	}
	releaseOldWriter()
	waitForCoordinatorFile(t, ready, 5*time.Second)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("new generation startup helper exited with error: %v", err)
	}
	finished = true

	fenced, err := old.CurrentInstanceFenced(context.Background(), oldInstance)
	if err != nil || !fenced {
		t.Fatalf("old process could not prove its generation was fenced: fenced=%v err=%v", fenced, err)
	}
	if _, err := old.AcquireShared(context.Background(), oldInstance); err == nil {
		t.Fatal("stale generation admitted a late writer after replacement")
	}
	if _, err := old.AcquireExclusive(context.Background(), oldInstance); !errors.Is(err, errOperationInstanceStale) {
		t.Fatalf("stale generation acquired an exclusive replacement freeze: %v", err)
	}
	current := newWriteCoordinator(root, "")
	currentInstance, err := current.Startup(context.Background())
	if err != nil {
		t.Fatalf("post-replacement process startup: %v", err)
	}
	currentWriter, err := current.AcquireShared(context.Background(), currentInstance)
	if err != nil {
		t.Fatalf("fenced current generation rejected writer: %v", err)
	}
	currentWriter()
	currentLease, err := current.AcquireExclusive(context.Background(), currentInstance)
	if err != nil {
		t.Fatal(err)
	}
	current.removeFreezeMarker(currentLease.markerToken) // isolate the cross-process lock from the earlier marker fast path
	if _, err := current.CurrentInstanceFenced(context.Background(), currentInstance); !errors.Is(err, errWriteFrozen) {
		t.Fatalf("fence read bypassed an active exclusive file lock: %v", err)
	}
	if _, err := current.AcquireShared(context.Background(), currentInstance); !errors.Is(err, errWriteFrozen) {
		t.Fatalf("shared admission bypassed an active exclusive file lock: %v", err)
	}
	currentLease.Release()
}

func TestWriteCoordinatorPolicyWarmBlocksReplacementUntilPersisted(t *testing.T) {
	srv := newContractServer(t)
	freshPolicyRoot(t, srv)

	// FIFO 夹具：Refresh 读策略文件时阻塞在 FIFO 上，制造可观测的「读取窗口」——
	// warmPolicy 在 Refresh 全程持有 shared admission（源改本地盘后这仍是接线不变量，
	// 持久化回写若回归也受同一道闸保护）。
	fifo := srv.env.PolicyFile()
	if err := exec.Command("mkfifo", "-m", "0600", fifo).Run(); err != nil {
		t.Skipf("mkfifo 不可用（本机不具备 FIFO 夹具）: %v", err)
	}
	warmDone := make(chan struct{})
	go func() {
		srv.warmPolicy()
		close(warmDone)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for srv.writeCoordinator.ActiveShared() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if srv.writeCoordinator.ActiveShared() != 1 {
		t.Fatal("policy warm did not hold one shared admission across Refresh")
	}

	next := newWriteCoordinator(srv.Env.DataRoot(), "")
	startupDone := make(chan struct {
		instance string
		err      error
	}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		instance, err := next.Startup(ctx)
		startupDone <- struct {
			instance string
			err      error
		}{instance: instance, err: err}
	}()
	select {
	case result := <-startupDone:
		<-warmDone
		t.Fatalf("generation startup crossed an active policy read snapshot: %q, %v", result.instance, result.err)
	case <-time.After(25 * time.Millisecond):
	}

	// 放开：写端把种子策略灌进 FIFO，Refresh 读到后采纳并释放 admission。
	document := fmt.Sprintf(`{"schema":1,"origin":%q,"fetchedAt":%q,"payload":%s}`,
		contractOrigin, time.Now().Format(time.RFC3339), warmPolicyData)
	writerDone := make(chan error, 1)
	go func() {
		f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			writerDone <- err
			return
		}
		_, err = f.WriteString(document)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		writerDone <- err
	}()
	select {
	case err := <-writerDone:
		if err != nil {
			t.Fatalf("灌入 FIFO 失败: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FIFO 写端未就绪")
	}
	select {
	case <-warmDone:
	case <-time.After(2 * time.Second):
		t.Fatal("policy warm did not settle after the FIFO payload arrived")
	}
	loaded, err := srv.Policy.Load()
	if err != nil || loaded == nil || loaded.Version != 8 {
		t.Fatalf("policy was not committed before releasing the old generation: %+v, %v", loaded, err)
	}
	select {
	case result := <-startupDone:
		if result.err != nil || result.instance == "" {
			t.Fatalf("successor startup after policy persistence = %q, %v", result.instance, result.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("successor startup did not proceed after policy Refresh released its shared admission")
	}
}

func TestWriteCoordinatorStalePolicyWarmSkipsAdoption(t *testing.T) {
	srv := newContractServer(t)
	freshPolicyRoot(t, srv)
	writePolicyFile(t, srv, warmPolicyData)
	// 伪造失效代际：warmPolicy 必须在 shared admission 处被拦下、不跑 Refresh。
	srv.operationInstanceID = "inst_ffffffffffffffff_1"
	srv.warmPolicy()
	// 若 warm 跑过，内存已有 v8；把盘上文件改坏后 Load 仍会回内存值。
	// 跳过时内存为空 ⇒ Load 读到坏文件 ⇒ nil——两种路径由此可分辨。
	if err := os.WriteFile(srv.env.PolicyFile(), []byte("garbage"), 0o600); err != nil {
		t.Fatalf("改坏策略文件失败: %v", err)
	}
	if p, err := srv.Policy.Load(); err != nil || p != nil {
		t.Fatalf("stale generation 仍采纳了策略: %+v %v", p, err)
	}
}

func TestWriteCoordinatorUsesIndependentSharedFDsAndDataRoots(t *testing.T) {
	root := t.TempDir()
	c := newWriteCoordinator(root, "")
	instance, err := c.Startup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	releaseOne, err := c.AcquireShared(context.Background(), instance)
	if err != nil {
		t.Fatal(err)
	}
	releaseTwo, err := c.AcquireShared(context.Background(), instance)
	if err != nil {
		releaseOne()
		t.Fatal(err)
	}
	releaseOne()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := c.AcquireExclusive(ctx, instance); !errors.Is(err, context.DeadlineExceeded) {
		releaseTwo()
		t.Fatalf("exclusive freeze passed while a distinct shared writer remained: %v", err)
	}
	releaseTwo()
	releaseExclusive, err := c.AcquireExclusive(context.Background(), instance)
	if err != nil {
		t.Fatalf("exclusive freeze did not proceed after every writer released: %v", err)
	}
	releaseExclusive.Release()

	other := newWriteCoordinator(t.TempDir(), "")
	otherInstance, err := other.Startup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	releaseRootWriter, err := c.AcquireShared(context.Background(), instance)
	if err != nil {
		t.Fatal(err)
	}
	releaseOtherExclusive, err := other.AcquireExclusive(context.Background(), otherInstance)
	if err != nil {
		releaseRootWriter()
		t.Fatalf("lock in another data root was not isolated: %v", err)
	}
	releaseOtherExclusive.Release()
	releaseRootWriter()
}

func TestWriteCoordinatorLocalFreezeRejectsNewWritersUntilUnfrozen(t *testing.T) {
	c := newWriteCoordinator(t.TempDir(), "")
	instance, err := c.Startup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	freeze, err := c.AcquireExclusive(context.Background(), instance)
	if err != nil {
		t.Fatalf("installer freeze: %v", err)
	}
	if _, err := c.AcquireShared(context.Background(), instance); err == nil {
		t.Fatal("local writer admission crossed an installer freeze")
	}
	freeze.Release()
	release, err := c.AcquireShared(context.Background(), instance)
	if err != nil {
		t.Fatalf("local writer admission stayed frozen after release: %v", err)
	}
	release()
}

func TestWriteCoordinatorHeldLeaseAndFilesystemStateFailures(t *testing.T) {
	t.Run("nil lease operations are safe", func(t *testing.T) {
		var lease *writeCoordinatorLease
		lease.Release()
		var coordinator *writeCoordinator
		coordinator.Hold(lease)
		if coordinator.ActiveShared() != 0 {
			t.Fatalf("nil coordinator has active shared leases: %d", coordinator.ActiveShared())
		}
	})
	t.Run("held replacement lease remains frozen until release", func(t *testing.T) {
		c := newWriteCoordinator(t.TempDir(), "")
		instance, err := c.Startup(nil)
		if err != nil {
			t.Fatal(err)
		}
		lease, err := c.AcquireExclusive(context.Background(), instance)
		if err != nil {
			t.Fatal(err)
		}
		c.Hold(lease)
		c.Hold(nil)
		foreign := newWriteCoordinator(t.TempDir(), "")
		foreignLease, err := foreign.AcquireExclusive(context.Background(), "")
		if err == nil {
			foreignLease.Release()
		}
		c.Hold(foreignLease)
		if _, err := c.AcquireShared(context.Background(), instance); !errors.Is(err, errWriteFrozen) {
			t.Fatalf("held replacement lease admitted a new shared writer: %v", err)
		}
		lease.Release()
		if err := lease.Err(); err != nil {
			t.Fatalf("healthy lease close reported an error: %v", err)
		}
		if _, held := c.heldExclusives[lease]; held {
			t.Fatal("released replacement lease remained in held lease set")
		}
		release, err := c.AcquireShared(context.Background(), instance)
		if err != nil {
			t.Fatalf("shared admission stayed frozen after held lease release: %v", err)
		}
		release()
	})

	t.Run("release reports descriptor failure and is idempotent", func(t *testing.T) {
		c := newWriteCoordinator(t.TempDir(), "")
		instance, err := c.Startup(nil)
		if err != nil {
			t.Fatal(err)
		}
		lease, err := c.AcquireExclusive(context.Background(), instance)
		if err != nil {
			t.Fatal(err)
		}
		if err := lease.file.Close(); err != nil {
			t.Fatal(err)
		}
		lease.Release()
		if lease.Err() == nil {
			t.Fatal("release discarded an unlock error from an already closed descriptor")
		}
		firstErr := lease.Err()
		lease.Release()
		if lease.Err() != firstErr {
			t.Fatal("idempotent second release changed the original close result")
		}
	})

	t.Run("runtime root and lock path errors fail closed", func(t *testing.T) {
		parentFile := filepath.Join(t.TempDir(), "parent-file")
		if err := os.WriteFile(parentFile, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		c := newWriteCoordinator(filepath.Join(parentFile, "data"), "")
		if err := c.prepareRuntimeDir(); err == nil {
			t.Fatal("runtime setup accepted a non-directory parent")
		}
		if _, err := c.openLockFile(); err == nil {
			t.Fatal("lock open unexpectedly succeeded below a non-directory parent")
		}
	})

	t.Run("generation file missing malformed and invalid are distinguished", func(t *testing.T) {
		c := newWriteCoordinator(t.TempDir(), "")
		if _, err := c.readGeneration(); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing generation should retain not-exist identity: %v", err)
		}
		if err := c.prepareRuntimeDir(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(c.statePath, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := c.readGeneration(); err == nil {
			t.Fatal("malformed generation JSON was accepted")
		}
		for _, blob := range []string{
			`{"schema":2,"generation":1,"instanceId":"inst_0123456789abcdef_1"}`,
			`{"schema":1,"generation":0,"instanceId":"inst_0123456789abcdef_1"}`,
			`{"schema":1,"generation":1,"instanceId":""}`,
		} {
			if err := os.WriteFile(c.statePath, []byte(blob), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := c.readGeneration(); err == nil {
				t.Errorf("invalid generation accepted: %s", blob)
			}
		}
	})

	t.Run("freeze marker ownership never removes another marker", func(t *testing.T) {
		c := newWriteCoordinator(t.TempDir(), "")
		if err := c.prepareRuntimeDir(); err != nil {
			t.Fatal(err)
		}
		if frozen, err := c.freezeExists(); err != nil || frozen {
			t.Fatalf("missing freeze marker state = %v, %v", frozen, err)
		}
		marker := operationFreezeMarker{InstanceID: "inst_test_1", Token: "owner-token", CreatedAt: time.Now().UTC().Format(time.RFC3339)}
		if err := c.createFreezeMarker(marker); err != nil {
			t.Fatal(err)
		}
		if frozen, err := c.freezeExists(); err != nil || !frozen {
			t.Fatalf("created freeze marker state = %v, %v", frozen, err)
		}
		c.removeFreezeMarker("not-owner")
		if _, err := os.Stat(c.freezePath); err != nil {
			t.Fatalf("wrong owner removed freeze marker: %v", err)
		}
		c.removeFreezeMarker(marker.Token)
		if _, err := os.Stat(c.freezePath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owner token did not remove marker: %v", err)
		}
		if err := os.WriteFile(c.freezePath, []byte("not-json"), 0o600); err != nil {
			t.Fatal(err)
		}
		c.removeFreezeMarker("owner-token")
		if _, err := os.Stat(c.freezePath); err != nil {
			t.Fatalf("malformed marker was removed without proof of ownership: %v", err)
		}
	})
}

func TestWriteCoordinatorHeldExclusiveGuardsConcurrentRelease(t *testing.T) {
	var nilCoordinator *writeCoordinator
	called := false
	if nilCoordinator.withHeldExclusive(func() { called = true }) || called {
		t.Fatal("nil coordinator claimed a held-exclusive admission")
	}
	c := newWriteCoordinator(t.TempDir(), "")
	instance, err := c.Startup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := c.AcquireExclusive(context.Background(), instance)
	if err != nil {
		t.Fatal(err)
	}
	c.Hold(lease)
	callbackStarted := make(chan struct{})
	allowCallbackReturn := make(chan struct{})
	callbackDone := make(chan bool, 1)
	go func() {
		callbackDone <- c.withHeldExclusive(func() {
			close(callbackStarted)
			<-allowCallbackReturn
		})
	}()
	select {
	case <-callbackStarted:
	case <-time.After(time.Second):
		t.Fatal("held-exclusive audit callback did not start")
	}
	releaseStarted := make(chan struct{})
	releaseDone := make(chan struct{})
	go func() {
		close(releaseStarted)
		lease.Release()
		close(releaseDone)
	}()
	<-releaseStarted
	select {
	case <-releaseDone:
		t.Fatal("lease release escaped while its held-exclusive callback was active")
	case <-time.After(20 * time.Millisecond):
	}
	if _, err := c.AcquireShared(context.Background(), instance); !errors.Is(err, errWriteFrozen) {
		t.Fatalf("held-exclusive freeze disappeared before callback completion: %v", err)
	}
	close(allowCallbackReturn)
	select {
	case held := <-callbackDone:
		if !held {
			t.Fatal("held-exclusive callback lost its retained lease before execution")
		}
	case <-time.After(time.Second):
		t.Fatal("held-exclusive callback did not return")
	}
	select {
	case <-releaseDone:
	case <-time.After(time.Second):
		t.Fatal("lease release did not finish after callback completion")
	}
	if _, err := c.AcquireShared(context.Background(), instance); err != nil {
		t.Fatalf("shared admission remained frozen after callback and release: %v", err)
	}
}

func TestWriteCoordinatorAdmissionAndStartupFailureBranches(t *testing.T) {
	failure := errors.New("synthetic isolated coordinator IO failure")
	t.Run("canceled and stale instances never acquire leases", func(t *testing.T) {
		c := newWriteCoordinator(t.TempDir(), "")
		if _, err := c.CurrentInstanceFenced(context.Background(), "inst_old_1"); !errors.Is(err, errOperationFenceUnready) {
			t.Fatalf("unstarted coordinator fence check = %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := c.Startup(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("pre-canceled startup = %v", err)
		}
		instance, err := c.Startup(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.AcquireShared(context.Background(), ""); !errors.Is(err, errOperationInstanceStale) {
			t.Fatalf("empty issuer instance admitted shared lease: %v", err)
		}
		if _, err := c.AcquireExclusive(context.Background(), "inst_other_1"); !errors.Is(err, errOperationInstanceStale) {
			t.Fatalf("stale issuer instance admitted exclusive lease: %v", err)
		}
		ctx, cancel = context.WithCancel(context.Background())
		cancel()
		if _, err := c.AcquireShared(ctx, instance); !errors.Is(err, context.Canceled) {
			t.Fatalf("pre-canceled shared admission = %v", err)
		}
		if _, err := c.AcquireExclusive(ctx, instance); !errors.Is(err, context.Canceled) {
			t.Fatalf("pre-canceled exclusive admission = %v", err)
		}
		if _, err := c.CurrentInstanceFenced(ctx, instance); !errors.Is(err, context.Canceled) {
			t.Fatalf("pre-canceled generation read = %v", err)
		}
		if fenced, err := c.CurrentInstanceFenced(context.Background(), instance); err != nil || fenced {
			t.Fatalf("current instance was reported fenced: %v, %v", fenced, err)
		}
		if fenced, err := c.CurrentInstanceFenced(nil, instance); err != nil || fenced {
			t.Fatalf("nil-context current generation read = %v, %v", fenced, err)
		}
		release, err := c.AcquireShared(nil, instance)
		if err != nil {
			t.Fatalf("nil-context shared admission = %v", err)
		}
		release()
		lease, err := c.AcquireExclusive(nil, instance)
		if err != nil {
			t.Fatalf("nil-context exclusive admission = %v", err)
		}
		lease.Release()
	})

	t.Run("startup rejects corrupt and exhausted generations", func(t *testing.T) {
		for name, blob := range map[string]string{
			"malformed": `{"generation":`,
			"schema":    `{"schema":2,"generation":1,"instanceId":"inst_0123456789abcdef_1"}`,
			"overflow":  `{"schema":1,"generation":18446744073709551615,"instanceId":"inst_0123456789abcdef_18446744073709551615"}`,
		} {
			t.Run(name, func(t *testing.T) {
				root := t.TempDir()
				c := newWriteCoordinator(root, "")
				if err := c.prepareRuntimeDir(); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(c.statePath, []byte(blob), 0o600); err != nil {
					t.Fatal(err)
				}
				if instance, err := c.Startup(context.Background()); err == nil || instance != "" {
					t.Fatalf("startup accepted invalid generation: instance=%q err=%v", instance, err)
				}
				if c.fenceReady {
					t.Fatal("failed startup published a usable generation")
				}
			})
		}
	})

	t.Run("freeze intent and corrupt post-start generation reject writers", func(t *testing.T) {
		c := newWriteCoordinator(t.TempDir(), "")
		instance, err := c.Startup(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(c.freezePath, []byte(`{"instanceId":"other","token":"other"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := c.AcquireShared(context.Background(), instance); !errors.Is(err, errWriteFrozen) {
			t.Fatalf("freeze intent did not block writer admission: %v", err)
		}
		if _, err := c.AcquireExclusive(context.Background(), instance); !errors.Is(err, errWriteFrozen) {
			t.Fatalf("existing freeze intent did not block replacement: %v", err)
		}
		if err := os.Remove(c.freezePath); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(c.statePath, []byte("broken"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := c.AcquireShared(context.Background(), instance); err == nil {
			t.Fatal("writer admitted despite malformed generation proof")
		}
		if _, err := c.AcquireExclusive(context.Background(), instance); err == nil {
			t.Fatal("replacement admitted despite malformed generation proof")
		}
		if _, err := c.CurrentInstanceFenced(context.Background(), instance); err == nil {
			t.Fatal("fence check hid malformed generation state")
		}
	})

	t.Run("isolated filesystem failures fail closed at lock and marker boundaries", func(t *testing.T) {
		root := t.TempDir()
		c := newWriteCoordinator(root, "")
		instance, err := c.Startup(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		badParent := filepath.Join(t.TempDir(), "regular-file")
		if err := os.WriteFile(badParent, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		originalLock, originalFreeze := c.lockPath, c.freezePath
		c.freezePath = filepath.Join(badParent, "freeze")
		if frozen, err := c.freezeExists(); err == nil || frozen {
			t.Fatalf("freeze stat hid a non-directory parent: %v, %v", frozen, err)
		}
		c.removeFreezeMarker("any-token") // read failure is intentionally a no-op
		if _, err := c.AcquireShared(context.Background(), instance); err == nil {
			t.Fatal("shared admission ignored freeze metadata read failure")
		}
		c.freezePath = originalFreeze
		c.lockPath = filepath.Join(badParent, "lock")
		if _, err := c.openLockFile(); err == nil {
			t.Fatal("openLockFile accepted a non-directory parent")
		}
		if _, err := c.AcquireShared(context.Background(), instance); err == nil {
			t.Fatal("shared admission ignored lock open failure")
		}
		if _, err := c.AcquireExclusive(context.Background(), instance); err == nil {
			t.Fatal("exclusive admission ignored lock open failure")
		}
		if _, err := c.CurrentInstanceFenced(context.Background(), instance); err == nil {
			t.Fatal("instance fence read ignored lock open failure")
		}
		c.lockPath = originalLock
	})

	t.Run("startup timeout releases its freeze intent and failed cleanup is visible", func(t *testing.T) {
		root := t.TempDir()
		old := newWriteCoordinator(root, "")
		oldInstance, err := old.Startup(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		release, err := old.AcquireShared(context.Background(), oldInstance)
		if err != nil {
			t.Fatal(err)
		}
		newGeneration := newWriteCoordinator(root, "")
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		defer cancel()
		if _, err := newGeneration.Startup(ctx); !errors.Is(err, context.DeadlineExceeded) {
			release()
			t.Fatalf("startup did not stop waiting at its caller deadline: %v", err)
		}
		release()
		if frozen, err := old.freezeExists(); err != nil || frozen {
			t.Fatalf("timed-out startup left its unowned freeze marker: %v, %v", frozen, err)
		}

		cleanupFailure := newWriteCoordinator(t.TempDir(), "")
		if err := cleanupFailure.prepareRuntimeDir(); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(cleanupFailure.freezePath, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cleanupFailure.freezePath, "owned-marker"), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := cleanupFailure.Startup(context.Background()); err == nil {
			t.Fatal("startup concealed failure removing an old non-empty freeze directory")
		}
	})

	t.Run("directory permission and read error boundaries are propagated", func(t *testing.T) {
		c := newWriteCoordinator(t.TempDir(), "")
		base := defaultWriteCoordinatorIO()
		c.io.chmodPath = func(string, os.FileMode) error { return failure }
		if err := c.prepareRuntimeDir(); !errors.Is(err, failure) {
			t.Fatalf("runtime permission failure = %v", err)
		}

		c.io = base
		instance, err := c.Startup(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		c.io.readFile = func(name string) ([]byte, error) {
			if name == c.statePath {
				return nil, failure
			}
			return base.readFile(name)
		}
		if _, err := c.AcquireShared(context.Background(), instance); !errors.Is(err, failure) {
			t.Fatalf("shared admission did not surface generation read error: %v", err)
		}
	})

	t.Run("exclusive marker collision and IO failures clean up", func(t *testing.T) {
		t.Run("existing freeze marker", func(t *testing.T) {
			c := newWriteCoordinator(t.TempDir(), "")
			instance, err := c.Startup(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			lease, err := c.AcquireExclusive(context.Background(), instance)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release()
			if _, err := c.AcquireExclusive(context.Background(), instance); !errors.Is(err, errWriteFrozen) {
				t.Fatalf("second exclusive admission did not honor existing freeze: %v", err)
			}
		})
		t.Run("nonduplicate marker write failure", func(t *testing.T) {
			c := newWriteCoordinator(t.TempDir(), "")
			instance, err := c.Startup(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			base := defaultWriteCoordinatorIO()
			c.io.openFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
				if name == c.freezePath {
					return nil, failure
				}
				return base.openFile(name, flag, perm)
			}
			if _, err := c.AcquireExclusive(context.Background(), instance); !errors.Is(err, failure) {
				t.Fatalf("exclusive marker creation error = %v", err)
			}
		})
		t.Run("post-marker lock open failure removes owned marker", func(t *testing.T) {
			c := newWriteCoordinator(t.TempDir(), "")
			instance, err := c.Startup(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			base := defaultWriteCoordinatorIO()
			lockOpens := 0
			c.io.openFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
				if name == c.lockPath {
					lockOpens++
					if lockOpens == 2 { // first is CurrentInstanceFenced; second follows marker creation
						return nil, failure
					}
				}
				return base.openFile(name, flag, perm)
			}
			if _, err := c.AcquireExclusive(context.Background(), instance); !errors.Is(err, failure) {
				t.Fatalf("post-marker lock open failure = %v", err)
			}
			c.io = base
			if frozen, err := c.freezeExists(); err != nil || frozen {
				t.Fatalf("post-marker lock failure left owned marker: %v, %v", frozen, err)
			}
		})
		t.Run("exclusive wait deadline releases marker", func(t *testing.T) {
			c := newWriteCoordinator(t.TempDir(), "")
			instance, err := c.Startup(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			release, err := c.AcquireShared(context.Background(), instance)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			defer cancel()
			if _, err := c.AcquireExclusive(ctx, instance); !errors.Is(err, context.DeadlineExceeded) {
				release()
				t.Fatalf("exclusive wait did not honor deadline: %v", err)
			}
			release()
			if frozen, err := c.freezeExists(); err != nil || frozen {
				t.Fatalf("exclusive wait timeout left a freeze marker: %v, %v", frozen, err)
			}
		})
	})

	t.Run("current-generation lock failures are surfaced", func(t *testing.T) {
		for _, mode := range []string{"open", "try"} {
			t.Run(mode, func(t *testing.T) {
				c := newWriteCoordinator(t.TempDir(), "")
				instance, err := c.Startup(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				base := defaultWriteCoordinatorIO()
				if mode == "open" {
					c.io.openFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
						if name == c.lockPath {
							return nil, failure
						}
						return base.openFile(name, flag, perm)
					}
				} else {
					c.io.tryLock = func(*os.File, operationLockMode) (bool, error) { return false, failure }
				}
				if _, err := c.CurrentInstanceFenced(context.Background(), instance); !errors.Is(err, failure) {
					t.Fatalf("current generation %s lock failure = %v", mode, err)
				}
			})
		}
	})

	t.Run("nonexistent marker and generation write failures are returned", func(t *testing.T) {
		c := newWriteCoordinator(t.TempDir(), "")
		if err := c.prepareRuntimeDir(); err != nil {
			t.Fatal(err)
		}
		parentFile := filepath.Join(t.TempDir(), "parent-file")
		if err := os.WriteFile(parentFile, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		c.freezePath = filepath.Join(parentFile, "freeze")
		if err := c.createFreezeMarker(operationFreezeMarker{Token: "token"}); err == nil {
			t.Fatal("freeze marker creation ignored a non-duplicate filesystem error")
		}
		c.runtimeDir = parentFile
		if err := c.writeGeneration(operationGeneration{Schema: 1, Generation: 1, InstanceID: "inst_0123456789abcdef_1"}); err == nil {
			t.Fatal("generation writer ignored an unusable temp directory")
		}
	})
}

func TestWriteCoordinatorIOFailuresFailClosed(t *testing.T) {
	failure := errors.New("synthetic isolated coordinator IO failure")
	t.Run("randomHexFrom reports source failures without global state", func(t *testing.T) {
		if _, err := randomHexFrom(errorRandomReader{err: failure}, 16); !errors.Is(err, failure) {
			t.Fatalf("random byte source failure was hidden: %v", err)
		}
	})

	t.Run("startup random marker lock and generation failures", func(t *testing.T) {
		t.Run("random marker", func(t *testing.T) {
			c := newWriteCoordinator(t.TempDir(), "")
			c.io.randomHex = func(int) (string, error) { return "", failure }
			if instance, err := c.Startup(context.Background()); !errors.Is(err, failure) || instance != "" {
				t.Fatalf("startup random source failure = %q, %v", instance, err)
			}
		})
		t.Run("nonduplicate freeze marker", func(t *testing.T) {
			c := newWriteCoordinator(t.TempDir(), "")
			base := defaultWriteCoordinatorIO()
			c.io.openFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
				if name == c.freezePath {
					return nil, failure
				}
				return base.openFile(name, flag, perm)
			}
			if _, err := c.Startup(context.Background()); !errors.Is(err, failure) {
				t.Fatalf("startup marker error = %v", err)
			}
		})
		t.Run("lock open and lock operation", func(t *testing.T) {
			for _, failLock := range []bool{false, true} {
				c := newWriteCoordinator(t.TempDir(), "")
				base := defaultWriteCoordinatorIO()
				if failLock {
					c.io.tryLock = func(*os.File, operationLockMode) (bool, error) { return false, failure }
				} else {
					c.io.openFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
						if name == c.lockPath {
							return nil, failure
						}
						return base.openFile(name, flag, perm)
					}
				}
				if _, err := c.Startup(context.Background()); !errors.Is(err, failure) {
					t.Fatalf("startup lock failLock=%v: %v", failLock, err)
				}
			}
		})
		t.Run("instance random", func(t *testing.T) {
			c := newWriteCoordinator(t.TempDir(), "")
			base := defaultWriteCoordinatorIO()
			calls := 0
			c.io.randomHex = func(size int) (string, error) {
				calls++
				if calls == 2 {
					return "", failure
				}
				return base.randomHex(size)
			}
			if _, err := c.Startup(context.Background()); !errors.Is(err, failure) {
				t.Fatalf("instance random failure = %v", err)
			}
		})
	})

	t.Run("startup temp generation filesystem steps fail closed", func(t *testing.T) {
		for _, stage := range []string{"create", "chmod", "write", "sync", "close", "rename", "cleanup-marker"} {
			t.Run(stage, func(t *testing.T) {
				c := newWriteCoordinator(t.TempDir(), "")
				base := defaultWriteCoordinatorIO()
				switch stage {
				case "create":
					c.io.createTemp = func(string, string) (*os.File, error) { return nil, failure }
				case "chmod":
					c.io.chmodFile = func(file *os.File, mode os.FileMode) error {
						if strings.Contains(filepath.Base(file.Name()), ".operation-generation-") {
							return failure
						}
						return base.chmodFile(file, mode)
					}
				case "write":
					c.io.writeFile = func(file *os.File, data []byte) (int, error) {
						if strings.Contains(filepath.Base(file.Name()), ".operation-generation-") {
							return 0, failure
						}
						return base.writeFile(file, data)
					}
				case "sync":
					c.io.syncFile = func(file *os.File) error {
						if strings.Contains(filepath.Base(file.Name()), ".operation-generation-") {
							return failure
						}
						return base.syncFile(file)
					}
				case "close":
					c.io.closeFile = func(file *os.File) error {
						if strings.Contains(filepath.Base(file.Name()), ".operation-generation-") {
							_ = base.closeFile(file)
							return failure
						}
						return base.closeFile(file)
					}
				case "rename":
					c.io.rename = func(string, string) error { return failure }
				case "cleanup-marker":
					c.io.remove = func(name string) error {
						if name == c.freezePath {
							return failure
						}
						return base.remove(name)
					}
				}
				if _, err := c.Startup(context.Background()); !errors.Is(err, failure) {
					t.Fatalf("startup stage %s did not fail closed: %v", stage, err)
				}
				if stage != "cleanup-marker" && c.fenceReady {
					t.Fatalf("failed startup stage %s published a generation", stage)
				}
			})
		}
	})

	t.Run("lock file permissions and create marker write sync close errors", func(t *testing.T) {
		c := newWriteCoordinator(t.TempDir(), "")
		if err := c.prepareRuntimeDir(); err != nil {
			t.Fatal(err)
		}
		c.io.chmodFile = func(*os.File, os.FileMode) error { return failure }
		if file, err := c.openLockFile(); file != nil || !errors.Is(err, failure) {
			t.Fatalf("lock chmod failure = %v, %v", file, err)
		}

		for _, stage := range []string{"write", "sync", "close"} {
			t.Run(stage, func(t *testing.T) {
				c := newWriteCoordinator(t.TempDir(), "")
				if err := c.prepareRuntimeDir(); err != nil {
					t.Fatal(err)
				}
				base := defaultWriteCoordinatorIO()
				switch stage {
				case "write":
					c.io.writeFile = func(*os.File, []byte) (int, error) { return 0, failure }
				case "sync":
					c.io.syncFile = func(*os.File) error { return failure }
				case "close":
					c.io.closeFile = func(file *os.File) error {
						_ = base.closeFile(file)
						return failure
					}
				}
				if err := c.createFreezeMarker(operationFreezeMarker{InstanceID: "inst_fixture_1", Token: "owner"}); !errors.Is(err, failure) {
					t.Fatalf("marker %s error = %v", stage, err)
				}
			})
		}
	})

	t.Run("post-start exclusive randomness and shared generation errors", func(t *testing.T) {
		c := newWriteCoordinator(t.TempDir(), "")
		instance, err := c.Startup(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		c.io.randomHex = func(int) (string, error) { return "", failure }
		if _, err := c.AcquireExclusive(context.Background(), instance); !errors.Is(err, failure) {
			t.Fatalf("exclusive freeze random failure = %v", err)
		}
		c.io.randomHex = defaultWriteCoordinatorIO().randomHex
		base := defaultWriteCoordinatorIO()
		c.io.readFile = func(name string) ([]byte, error) {
			if name == c.statePath {
				return nil, failure
			}
			return base.readFile(name)
		}
		if _, err := c.AcquireShared(context.Background(), instance); !errors.Is(err, failure) {
			t.Fatalf("shared generation read failure = %v", err)
		}
		if _, err := c.CurrentInstanceFenced(context.Background(), instance); !errors.Is(err, failure) {
			t.Fatalf("fence generation read failure = %v", err)
		}
		if _, err := c.AcquireExclusive(context.Background(), instance); !errors.Is(err, failure) {
			t.Fatalf("exclusive generation read failure = %v", err)
		}
	})

	t.Run("exclusive final generation read and final cancellation are fenced", func(t *testing.T) {
		t.Run("generation read error after exclusive acquisition", func(t *testing.T) {
			c := newWriteCoordinator(t.TempDir(), "")
			instance, err := c.Startup(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			base := defaultWriteCoordinatorIO()
			stateReads := 0
			c.io.readFile = func(name string) ([]byte, error) {
				if name == c.statePath {
					stateReads++
					if stateReads == 2 {
						return nil, failure
					}
				}
				return base.readFile(name)
			}
			if _, err := c.AcquireExclusive(context.Background(), instance); !errors.Is(err, failure) {
				t.Fatalf("final exclusive generation read error = %v", err)
			}
			c.io = base
			if frozen, err := c.freezeExists(); err != nil || frozen {
				t.Fatalf("generation read failure left marker: %v, %v", frozen, err)
			}
		})
		t.Run("context canceled between lock return and final check", func(t *testing.T) {
			c := newWriteCoordinator(t.TempDir(), "")
			instance, err := c.Startup(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			ctx := &cancelOnErrContext{Context: context.Background(), cancel: 5}
			if _, err := c.AcquireExclusive(ctx, instance); !errors.Is(err, context.Canceled) {
				t.Fatalf("last pre-return cancellation check was skipped: %v (Err calls=%d)", err, ctx.calls)
			}
			if ctx.calls != 5 {
				t.Fatalf("unexpected cancellation check sequence: %d calls", ctx.calls)
			}
			if frozen, err := c.freezeExists(); err != nil || frozen {
				t.Fatalf("final-check cancellation left marker: %v, %v", frozen, err)
			}
		})
	})
}

func TestWriteCoordinatorInjectedLockRaceOutcomes(t *testing.T) {
	failure := errors.New("synthetic isolated lock failure")
	t.Run("shared lock system error and busy outcomes", func(t *testing.T) {
		for _, outcome := range []string{"error", "busy"} {
			t.Run(outcome, func(t *testing.T) {
				c := newWriteCoordinator(t.TempDir(), "")
				instance, err := c.Startup(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				base := defaultWriteCoordinatorIO()
				c.io.tryLock = func(file *os.File, mode operationLockMode) (bool, error) {
					if outcome == "error" {
						return false, failure
					}
					return false, nil
				}
				_, err = c.AcquireShared(context.Background(), instance)
				if outcome == "error" && !errors.Is(err, failure) {
					t.Fatalf("lock error was lost: %v", err)
				}
				if outcome == "busy" && !errors.Is(err, errWriteFrozen) {
					t.Fatalf("busy shared file lock did not reject admission: %v", err)
				}
				c.io = base
			})
		}
	})

	t.Run("shared cancellation and second freeze recheck release lock", func(t *testing.T) {
		for _, outcome := range []string{"cancel-after-lock", "second-freeze", "second-freeze-error"} {
			t.Run(outcome, func(t *testing.T) {
				c := newWriteCoordinator(t.TempDir(), "")
				instance, err := c.Startup(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				base := defaultWriteCoordinatorIO()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				statCalls := 0
				c.io.stat = func(name string) (os.FileInfo, error) {
					if name == c.freezePath {
						statCalls++
						if outcome == "second-freeze-error" && statCalls == 2 {
							return nil, failure
						}
						if outcome == "second-freeze" && statCalls == 2 {
							if err := os.WriteFile(c.freezePath, []byte(`{"instanceId":"late","token":"late"}`), 0o600); err != nil {
								return nil, err
							}
						}
					}
					return base.stat(name)
				}
				unlockCalls := 0
				c.io.unlock = func(file *os.File) error {
					unlockCalls++
					return base.unlock(file)
				}
				c.io.tryLock = func(file *os.File, mode operationLockMode) (bool, error) {
					locked, err := base.tryLock(file, mode)
					if outcome == "cancel-after-lock" && mode == operationLockShared && locked {
						cancel()
					}
					return locked, err
				}
				_, err = c.AcquireShared(ctx, instance)
				switch outcome {
				case "cancel-after-lock":
					if !errors.Is(err, context.Canceled) || unlockCalls == 0 || c.ActiveShared() != 0 {
						t.Fatalf("cancellation after lock was not cleaned up: err=%v unlocks=%d active=%d", err, unlockCalls, c.ActiveShared())
					}
				case "second-freeze":
					if !errors.Is(err, errWriteFrozen) || unlockCalls == 0 {
						t.Fatalf("late freeze was not rechecked/cleaned: err=%v unlocks=%d", err, unlockCalls)
					}
				case "second-freeze-error":
					if !errors.Is(err, failure) || unlockCalls == 0 {
						t.Fatalf("late freeze read error was not surfaced/cleaned: err=%v unlocks=%d", err, unlockCalls)
					}
				}
			})
		}
	})

	t.Run("exclusive cancellation after lock acquisition unfreezes", func(t *testing.T) {
		c := newWriteCoordinator(t.TempDir(), "")
		instance, err := c.Startup(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		base := defaultWriteCoordinatorIO()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c.io.tryLock = func(file *os.File, mode operationLockMode) (bool, error) {
			locked, err := base.tryLock(file, mode)
			if mode == operationLockExclusive && locked {
				cancel()
			}
			return locked, err
		}
		if _, err := c.AcquireExclusive(ctx, instance); !errors.Is(err, context.Canceled) {
			t.Fatalf("exclusive lock cancellation result = %v", err)
		}
		c.io = base
		if frozen, err := c.freezeExists(); err != nil || frozen {
			t.Fatalf("canceled exclusive attempt left freeze marker: %v, %v", frozen, err)
		}
		release, err := c.AcquireShared(context.Background(), instance)
		if err != nil {
			t.Fatalf("canceled exclusive attempt retained the file lock: %v", err)
		}
		release()
	})

	t.Run("exclusive generation changed while waiting is rejected", func(t *testing.T) {
		c := newWriteCoordinator(t.TempDir(), "")
		instance, err := c.Startup(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		base := defaultWriteCoordinatorIO()
		c.io.tryLock = func(file *os.File, mode operationLockMode) (bool, error) {
			locked, err := base.tryLock(file, mode)
			if mode == operationLockExclusive && locked {
				blob, marshalErr := json.Marshal(operationGeneration{Schema: 1, Generation: c.generation + 1, InstanceID: "inst_eeeeeeeeeeeeeeee_2"})
				if marshalErr != nil {
					return false, marshalErr
				}
				if writeErr := os.WriteFile(c.statePath, blob, 0o600); writeErr != nil {
					return false, writeErr
				}
			}
			return locked, err
		}
		if _, err := c.AcquireExclusive(context.Background(), instance); !errors.Is(err, errOperationInstanceStale) {
			t.Fatalf("exclusive admission accepted a changed generation: %v", err)
		}
		c.io = base
	})

	t.Run("nonblocking lock and lock error paths are deterministic", func(t *testing.T) {
		file, err := os.CreateTemp(t.TempDir(), "lock-helper")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if err := lockOperationFileWith(nil, file, operationLockShared, false,
			func(*os.File, operationLockMode) (bool, error) { return false, nil }, func(*os.File) error { return nil }); !errors.Is(err, errWriteFrozen) {
			t.Fatalf("nonblocking busy lock = %v", err)
		}
		if err := lockOperationFileWith(context.Background(), file, operationLockShared, false,
			func(*os.File, operationLockMode) (bool, error) { return false, failure }, func(*os.File) error { return nil }); !errors.Is(err, failure) {
			t.Fatalf("lock primitive error = %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := lockOperationFileWith(ctx, file, operationLockShared, true,
			func(*os.File, operationLockMode) (bool, error) { return true, nil }, func(*os.File) error { return nil }); !errors.Is(err, context.Canceled) {
			t.Fatalf("pre-canceled blocking lock = %v", err)
		}
	})
	t.Run("expired timer channel is drained only when stop fails", func(t *testing.T) {
		empty := make(chan time.Time)
		stopAndDrainOperationTimer(func() bool { return false }, empty)
		if len(empty) != 0 {
			t.Fatal("empty timer channel did not take the nonblocking default branch")
		}
		timerC := make(chan time.Time, 1)
		timerC <- time.Now()
		stopAndDrainOperationTimer(func() bool { return false }, timerC)
		if len(timerC) != 0 {
			t.Fatal("expired timer channel was not drained after Stop returned false")
		}
		kept := make(chan time.Time, 1)
		kept <- time.Now()
		stopAndDrainOperationTimer(func() bool { return true }, kept)
		if len(kept) != 1 {
			t.Fatal("successfully stopped timer channel was drained unexpectedly")
		}
	})
	if !assertNativeUnexpectedFileLockError(t) {
		t.Log("native flock unexpected-error branch is not available on this platform")
	}
}

func TestWriteCoordinatorStartupFailsClosedWhenDataRootCannotHostFence(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newWriteCoordinator(filepath.Join(file, "child"), "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	instance, err := c.Startup(ctx)
	if err == nil {
		t.Fatal("startup claimed writer generation without creating its fence/lock root")
	}
	if _, err := c.AcquireShared(context.Background(), instance); err == nil {
		t.Fatal("unfenced coordinator admitted a mutation after startup failure")
	}
}

func TestWriteCoordinatorNewFailsClosedAndReleasesConstructorSnapshot(t *testing.T) {
	t.Run("fingerprint key reader accepts exact material and zeros a short read", func(t *testing.T) {
		material := bytes.Repeat([]byte{0x5a}, 32)
		key, err := readProfileDraftFingerprintKey(bytes.NewReader(material))
		if err != nil || !bytes.Equal(key[:], material) {
			t.Fatalf("exact fingerprint key read = %x, %v", key, err)
		}
		shortKey, err := readProfileDraftFingerprintKey(bytes.NewReader([]byte{0x01, 0x02}))
		if err == nil || shortKey != ([32]byte{}) {
			t.Fatalf("short fingerprint key read did not fail closed and clear bytes: %x, %v", shortKey, err)
		}
	})

	t.Run("New rejects a short fingerprint source before fencing the data root", func(t *testing.T) {
		prior := newContractServer(t)
		deps := prior.Deps
		deps.profileDraftFingerprintKeyReader = bytes.NewReader([]byte{0x01, 0x02})
		beforeRequests := len(prior.upstream.recorded())
		if server, err := New(deps); err == nil || server != nil {
			t.Fatalf("New accepted incomplete fingerprint key material: server=%v err=%v", server, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if fenced, err := prior.writeCoordinator.CurrentInstanceFenced(ctx, prior.operationInstanceID); err != nil || fenced {
			t.Fatalf("fingerprint source failure advanced the existing data-root generation: fenced=%v err=%v", fenced, err)
		}
		if after := len(prior.upstream.recorded()); after != beforeRequests {
			t.Fatalf("fingerprint source failure issued upstream requests: before=%d after=%d", beforeRequests, after)
		}
	})

	t.Run("successor generation winning before constructor shared admission fails closed", func(t *testing.T) {
		root := t.TempDir()
		predecessor := newWriteCoordinator(root, "")
		successor := newWriteCoordinator(root, "")
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		var successorID string
		instanceID, release, err := startWriteGenerationWithSnapshot(ctx, predecessor, func() {
			var startupErr error
			successorID, startupErr = successor.Startup(ctx)
			if startupErr != nil {
				t.Errorf("successor startup in constructor race barrier: %v", startupErr)
			}
		})
		if release != nil || !errors.Is(err, errOperationInstanceStale) || instanceID == "" || successorID == "" {
			t.Fatalf("constructor admitted a snapshot after a successor generation won: old=%q successor=%q release=%v err=%v", instanceID, successorID, release != nil, err)
		}
	})

	t.Run("unusable data root never creates a server", func(t *testing.T) {
		srv := newContractServer(t)
		parentFile := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(parentFile, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		env := *srv.Env
		env.Home = filepath.Join(parentFile, "home")
		deps := srv.Deps
		deps.Env = &env
		if server, err := New(deps); err == nil || server != nil {
			t.Fatalf("New returned a writable server with an unusable coordinator root: server=%v err=%v", server, err)
		}
	})

	t.Run("failed store load releases startup shared snapshot", func(t *testing.T) {
		srv := newContractServer(t)
		env := *srv.Env
		env.Home = t.TempDir()
		if err := env.EnsureLayout(); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(env.ManagedDir(), "profiles.json"), 0o700); err != nil {
			t.Fatal(err)
		}
		deps := srv.Deps
		deps.Env = &env
		if server, err := New(deps); err == nil || server != nil {
			t.Fatalf("New unexpectedly accepted a profile store directory: server=%v err=%v", server, err)
		}
		replacement := newWriteCoordinator(env.DataRoot(), "")
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if instance, err := replacement.Startup(ctx); err != nil || instance == "" {
			t.Fatalf("failed New did not release its startup snapshot lease: instance=%q err=%v", instance, err)
		}
	})

	//（「failed install recovery」子测随 recoverInterruptedInstalls 装配删除注销——
	// 下载/任务面已删，New 不再有安装恢复段；design §2.2 deps.go 行。）
}

func waitForCoordinatorFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-ticker.C:
		case <-timer.C:
			t.Fatalf("timed out waiting for isolated coordinator marker %s (deadline %s)", path, deadline.UTC().Format(time.RFC3339Nano))
		}
	}
}

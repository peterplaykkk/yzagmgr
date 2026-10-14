package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	operationStartupFenceTimeout = 2 * time.Minute
	operationRuntimeDirName      = "runtime"
	operationLockFileName        = "operation-lifecycle.lock"
	operationStateFileName       = "operation-lifecycle.json"
	operationFreezeFileName      = "operation-lifecycle.freeze"
)

var (
	errWriteFrozen            = errors.New("写入入口正在冻结")
	errOperationInstanceStale = errors.New("服务实例已被新代际替换")
	errOperationFenceUnready  = errors.New("跨进程写入栅栏不可用")
	operationLockPollInterval = 10 * time.Millisecond
)

type operationGeneration struct {
	Schema     int    `json:"schema"`
	Generation uint64 `json:"generation"`
	InstanceID string `json:"instanceId"`
}

type operationFreezeMarker struct {
	InstanceID string `json:"instanceId"`
	Token      string `json:"token"`
	CreatedAt  string `json:"createdAt"`
}

// writeCoordinatorIO keeps the narrow filesystem and OS-lock boundary
// injectable for deterministic failure-path tests. Production binds every hook
// to the standard library and the current platform's file lock implementation.
type writeCoordinatorIO struct {
	randomHex  func(int) (string, error)
	mkdirAll   func(string, os.FileMode) error
	chmodPath  func(string, os.FileMode) error
	openFile   func(string, int, os.FileMode) (*os.File, error)
	chmodFile  func(*os.File, os.FileMode) error
	readFile   func(string) ([]byte, error)
	createTemp func(string, string) (*os.File, error)
	writeFile  func(*os.File, []byte) (int, error)
	syncFile   func(*os.File) error
	closeFile  func(*os.File) error
	rename     func(string, string) error
	remove     func(string) error
	stat       func(string) (os.FileInfo, error)
	tryLock    func(*os.File, operationLockMode) (bool, error)
	unlock     func(*os.File) error
}

func defaultWriteCoordinatorIO() writeCoordinatorIO {
	return writeCoordinatorIO{
		randomHex: randomHex,
		mkdirAll:  os.MkdirAll,
		chmodPath: os.Chmod,
		openFile: func(name string, flag int, perm os.FileMode) (*os.File, error) {
			return os.OpenFile(name, flag, perm)
		},
		chmodFile:  (*os.File).Chmod,
		readFile:   os.ReadFile,
		createTemp: os.CreateTemp,
		writeFile: func(file *os.File, data []byte) (int, error) {
			return file.Write(data)
		},
		syncFile:  (*os.File).Sync,
		closeFile: (*os.File).Close,
		rename:    os.Rename,
		remove:    os.Remove,
		stat:      os.Stat,
		tryLock:   tryOperationFileLock,
		unlock:    unlockOperationFile,
	}
}

// writeCoordinator joins in-process request admission with a data-root-scoped
// cross-process shared/exclusive file lock. The lock file is stable: it is
// never replaced, and every lease opens its own descriptor/handle.
type writeCoordinator struct {
	runtimeDir     string
	lockPath       string
	statePath      string
	freezePath     string
	io             writeCoordinatorIO
	instanceID     string
	generation     uint64
	fenceReady     bool
	activeShared   atomic.Int64
	holdMu         sync.Mutex
	heldExclusives map[*writeCoordinatorLease]struct{}
}

type writeCoordinatorLease struct {
	coordinator *writeCoordinator
	file        *os.File
	markerToken string
	once        sync.Once
	closeErr    error
}

func newWriteCoordinator(dataRoot, instanceID string) *writeCoordinator {
	runtimeDir := filepath.Join(dataRoot, operationRuntimeDirName)
	return &writeCoordinator{
		runtimeDir: runtimeDir,
		lockPath:   filepath.Join(runtimeDir, operationLockFileName),
		statePath:  filepath.Join(runtimeDir, operationStateFileName),
		freezePath: filepath.Join(runtimeDir, operationFreezeFileName),
		instanceID: instanceID,
		io:         defaultWriteCoordinatorIO(),
	}
}

// Startup acquires the data-root exclusive lock, waits out old shared writers,
// publishes a fresh generation, and removes only stale freeze metadata while no
// previous writer can still hold the lock.
func (c *writeCoordinator) Startup(ctx context.Context) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := c.prepareRuntimeDir(); err != nil {
		return "", err
	}
	startupToken, err := c.io.randomHex(16)
	if err != nil {
		return "", fmt.Errorf("生成启动栅栏编号失败: %w", err)
	}
	markerOwned := false
	if err := c.createFreezeMarker(operationFreezeMarker{
		InstanceID: "startup",
		Token:      startupToken,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
	}); err == nil {
		markerOwned = true
	} else if !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("建立启动写入冻结标记失败: %w", err)
	}
	removeStartupMarker := func() {
		if markerOwned {
			c.removeFreezeMarker(startupToken)
		}
	}
	file, err := c.openLockFile()
	if err != nil {
		removeStartupMarker()
		return "", err
	}
	defer c.io.closeFile(file)
	if err := c.lockFile(ctx, file, operationLockExclusive, true); err != nil {
		removeStartupMarker()
		return "", fmt.Errorf("取得启动代际栅栏失败: %w", err)
	}
	defer c.io.unlock(file)

	previous, err := c.readGeneration()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		removeStartupMarker()
		return "", fmt.Errorf("读取操作代际文件失败: %w", err)
	}
	var generation uint64 = 1
	if err == nil {
		if previous.Schema != 1 || previous.Generation == ^uint64(0) {
			removeStartupMarker()
			return "", errors.New("操作代际文件版本或计数非法")
		}
		generation = previous.Generation + 1
	}
	randomPart, err := c.io.randomHex(16)
	if err != nil {
		removeStartupMarker()
		return "", fmt.Errorf("生成操作实例编号失败: %w", err)
	}
	instanceID := fmt.Sprintf("inst_%s_%s", randomPart, fmt.Sprint(generation))
	state := operationGeneration{Schema: 1, Generation: generation, InstanceID: instanceID}
	if err := c.writeGeneration(state); err != nil {
		removeStartupMarker()
		return "", fmt.Errorf("写入操作代际文件失败: %w", err)
	}
	if err := c.io.remove(c.freezePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("清理旧代际冻结标记失败: %w", err)
	}
	c.instanceID = instanceID
	c.generation = generation
	c.fenceReady = true
	return instanceID, nil
}

// AcquireShared admits a synchronous API write/test. It never waits behind a
// freeze: admission is denied while a freeze intent or exclusive lock exists.
func (c *writeCoordinator) AcquireShared(ctx context.Context, instanceID string) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !c.fenceReady || instanceID == "" || instanceID != c.instanceID {
		return nil, errOperationInstanceStale
	}
	if frozen, err := c.freezeExists(); err != nil {
		return nil, fmt.Errorf("检查写入冻结状态失败: %w", err)
	} else if frozen {
		return nil, errWriteFrozen
	}
	file, err := c.openLockFile()
	if err != nil {
		return nil, err
	}
	locked, err := c.io.tryLock(file, operationLockShared)
	if err != nil {
		_ = c.io.closeFile(file)
		return nil, fmt.Errorf("取得写入共享栅栏失败: %w", err)
	}
	if !locked {
		_ = c.io.closeFile(file)
		return nil, errWriteFrozen
	}
	if err := ctx.Err(); err != nil {
		_ = c.io.unlock(file)
		_ = c.io.closeFile(file)
		return nil, err
	}
	state, err := c.readGeneration()
	if err != nil {
		_ = c.io.unlock(file)
		_ = c.io.closeFile(file)
		return nil, fmt.Errorf("读取写入代际失败: %w", err)
	}
	if state.InstanceID != instanceID || state.Generation != c.generation {
		_ = c.io.unlock(file)
		_ = c.io.closeFile(file)
		return nil, errOperationInstanceStale
	}
	if frozen, err := c.freezeExists(); err != nil {
		_ = c.io.unlock(file)
		_ = c.io.closeFile(file)
		return nil, fmt.Errorf("复核写入冻结状态失败: %w", err)
	} else if frozen {
		_ = c.io.unlock(file)
		_ = c.io.closeFile(file)
		return nil, errWriteFrozen
	}
	c.activeShared.Add(1)
	var once sync.Once
	release := func() {
		once.Do(func() {
			c.activeShared.Add(-1)
			_ = c.io.unlock(file)
			_ = c.io.closeFile(file)
		})
	}
	return release, nil
}

func (c *writeCoordinator) ActiveShared() int {
	if c == nil {
		return 0
	}
	return int(c.activeShared.Load())
}

// AcquireExclusive writes the freeze intent before waiting for shared writers.
// Thus no new writer can slip in while the exclusive lease drains prior work.
func (c *writeCoordinator) AcquireExclusive(ctx context.Context, instanceID string) (*writeCoordinatorLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !c.fenceReady || instanceID == "" || instanceID != c.instanceID {
		return nil, errOperationInstanceStale
	}
	fenced, err := c.CurrentInstanceFenced(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	if fenced {
		return nil, errOperationInstanceStale
	}
	token, err := c.io.randomHex(16)
	if err != nil {
		return nil, fmt.Errorf("生成冻结标记编号失败: %w", err)
	}
	marker := operationFreezeMarker{
		InstanceID: instanceID,
		Token:      token,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	if err := c.createFreezeMarker(marker); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, errWriteFrozen
		}
		return nil, fmt.Errorf("建立写入冻结标记失败: %w", err)
	}
	removeMarker := func() { c.removeFreezeMarker(token) }
	file, err := c.openLockFile()
	if err != nil {
		removeMarker()
		return nil, err
	}
	if err := c.lockFile(ctx, file, operationLockExclusive, true); err != nil {
		removeMarker()
		_ = c.io.closeFile(file)
		return nil, fmt.Errorf("等待既有写入收场失败: %w", err)
	}
	if err := ctx.Err(); err != nil {
		_ = c.io.unlock(file)
		removeMarker()
		_ = c.io.closeFile(file)
		return nil, err
	}
	state, err := c.readGeneration()
	if err != nil || state.InstanceID != instanceID || state.Generation != c.generation {
		_ = c.io.unlock(file)
		removeMarker()
		_ = c.io.closeFile(file)
		if err != nil {
			return nil, fmt.Errorf("复核独占代际失败: %w", err)
		}
		return nil, errOperationInstanceStale
	}
	return &writeCoordinatorLease{coordinator: c, file: file, markerToken: token}, nil
}

// Hold keeps a successful replacement freeze and its independent file handle
// alive until the owning process exits. The OS releases the lock on exit.
func (c *writeCoordinator) Hold(lease *writeCoordinatorLease) {
	if lease == nil || lease.coordinator != c {
		return
	}
	c.holdMu.Lock()
	if c.heldExclusives == nil {
		c.heldExclusives = make(map[*writeCoordinatorLease]struct{})
	}
	c.heldExclusives[lease] = struct{}{}
	c.holdMu.Unlock()
}

// withHeldExclusive keeps the held lease from being released until fn returns.
// A post-handler audit append can therefore remain covered when a successful
// updater deliberately holds its freeze through process exit.
func (c *writeCoordinator) withHeldExclusive(fn func()) bool {
	if c == nil {
		return false
	}
	c.holdMu.Lock()
	defer c.holdMu.Unlock()
	if len(c.heldExclusives) == 0 {
		return false
	}
	fn()
	return true
}

func (l *writeCoordinatorLease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.coordinator != nil {
			l.coordinator.holdMu.Lock()
			delete(l.coordinator.heldExclusives, l)
			l.coordinator.holdMu.Unlock()
		}
		if l.coordinator != nil && l.markerToken != "" {
			l.coordinator.removeFreezeMarker(l.markerToken)
		}
		if l.file != nil {
			if err := l.coordinator.io.unlock(l.file); err != nil {
				l.closeErr = err
			}
			if err := l.coordinator.io.closeFile(l.file); l.closeErr == nil {
				l.closeErr = err
			}
		}
	})
}

func (l *writeCoordinatorLease) Err() error { return l.closeErr }

// CurrentInstanceFenced reports whether the generation currently stored under
// the cross-process lock is different from this request's issuer generation.
// It only returns proof after obtaining a fresh shared lock and reading state.
func (c *writeCoordinator) CurrentInstanceFenced(ctx context.Context, instanceID string) (bool, error) {
	if !c.fenceReady {
		return false, errOperationFenceUnready
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return false, err
		}
	}
	file, err := c.openLockFile()
	if err != nil {
		return false, err
	}
	defer c.io.closeFile(file)
	locked, err := c.io.tryLock(file, operationLockShared)
	if err != nil {
		return false, err
	}
	if !locked {
		return false, errWriteFrozen
	}
	defer c.io.unlock(file)
	state, err := c.readGeneration()
	if err != nil {
		return false, err
	}
	return state.InstanceID != instanceID || state.Generation != c.generation, nil
}

func (c *writeCoordinator) prepareRuntimeDir() error {
	if err := c.io.mkdirAll(c.runtimeDir, 0o700); err != nil {
		return err
	}
	if err := c.io.chmodPath(c.runtimeDir, 0o700); err != nil {
		return err
	}
	return nil
}

func (c *writeCoordinator) openLockFile() (*os.File, error) {
	file, err := c.io.openFile(c.lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开操作栅栏文件失败: %w", err)
	}
	if err := c.io.chmodFile(file, 0o600); err != nil {
		_ = c.io.closeFile(file)
		return nil, fmt.Errorf("收紧操作栅栏权限失败: %w", err)
	}
	return file, nil
}

func (c *writeCoordinator) readGeneration() (operationGeneration, error) {
	blob, err := c.io.readFile(c.statePath)
	if err != nil {
		return operationGeneration{}, err
	}
	var state operationGeneration
	if err := json.Unmarshal(blob, &state); err != nil {
		return operationGeneration{}, err
	}
	if state.Schema != 1 || state.Generation == 0 || state.InstanceID == "" {
		return operationGeneration{}, errors.New("操作代际状态缺少必需字段")
	}
	return state, nil
}

func (c *writeCoordinator) writeGeneration(state operationGeneration) error {
	// operationGeneration contains only JSON primitives; encoding/json cannot
	// fail for this closed metadata shape.
	blob, _ := json.Marshal(state)
	tmp, err := c.io.createTemp(c.runtimeDir, ".operation-generation-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer c.io.remove(tmpPath)
	if err := c.io.chmodFile(tmp, 0o600); err != nil {
		_ = c.io.closeFile(tmp)
		return err
	}
	if _, err := c.io.writeFile(tmp, append(blob, '\n')); err != nil {
		_ = c.io.closeFile(tmp)
		return err
	}
	if err := c.io.syncFile(tmp); err != nil {
		_ = c.io.closeFile(tmp)
		return err
	}
	if err := c.io.closeFile(tmp); err != nil {
		return err
	}
	return c.io.rename(tmpPath, c.statePath)
}

func (c *writeCoordinator) createFreezeMarker(marker operationFreezeMarker) error {
	file, err := c.io.openFile(c.freezePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	// operationFreezeMarker contains only JSON primitives; its encoding cannot fail.
	blob, _ := json.Marshal(marker)
	_, writeErr := c.io.writeFile(file, append(blob, '\n'))
	if writeErr == nil {
		writeErr = c.io.syncFile(file)
	}
	closeErr := c.io.closeFile(file)
	if writeErr != nil {
		_ = c.io.remove(c.freezePath)
		return writeErr
	}
	return closeErr
}

func (c *writeCoordinator) freezeExists() (bool, error) {
	_, err := c.io.stat(c.freezePath)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func (c *writeCoordinator) removeFreezeMarker(token string) {
	blob, err := c.io.readFile(c.freezePath)
	if err != nil {
		return
	}
	var marker operationFreezeMarker
	if json.Unmarshal(blob, &marker) != nil || marker.Token != token {
		return
	}
	_ = c.io.remove(c.freezePath)
}

func randomHex(size int) (string, error) {
	return randomHexFrom(rand.Reader, size)
}

func randomHexFrom(reader io.Reader, size int) (string, error) {
	buf := make([]byte, size)
	if _, err := io.ReadFull(reader, buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func (c *writeCoordinator) lockFile(ctx context.Context, file *os.File, mode operationLockMode, wait bool) error {
	return lockOperationFileWith(ctx, file, mode, wait, c.io.tryLock, c.io.unlock)
}

func lockOperationFileWith(
	ctx context.Context,
	file *os.File,
	mode operationLockMode,
	wait bool,
	tryLock func(*os.File, operationLockMode) (bool, error),
	unlock func(*os.File) error,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		locked, err := tryLock(file, mode)
		if err != nil {
			return err
		}
		if locked {
			if err := ctx.Err(); err != nil {
				_ = unlock(file)
				return err
			}
			return nil
		}
		if !wait {
			return errWriteFrozen
		}
		timer := time.NewTimer(operationLockPollInterval)
		select {
		case <-ctx.Done():
			stopAndDrainOperationTimer(timer.Stop, timer.C)
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func stopAndDrainOperationTimer(stop func() bool, timerC <-chan time.Time) {
	if !stop() {
		select {
		case <-timerC:
		default:
		}
	}
}

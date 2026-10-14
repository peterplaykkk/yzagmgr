package api

// 本文件是跨用例共享的树快照/日志辅助——自 tasks_test.go 迁出（下载/任务面删除，
// design §2.2 §11；这些辅助被 configdraft/profiles/uc014/diagnostics 等存活用例共用，
// 随唯一消费面迁至共享 helper 文件）。

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// quietLog 是测试的日志落点：夹具告警会污染测试输出，而相关断言的是状态，不是日志。
func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type fileState struct {
	Size  int64
	Sum   string
	Mode  os.FileMode
	Mtime time.Time
}

// snapshotTree 把一棵树的每个文件快照下来（缺席的树 = 空表）。
func snapshotTree(t *testing.T, root string) map[string]fileState {
	t.Helper()
	out := map[string]fileState{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		sum := sha256.Sum256(raw)
		out[path] = fileState{
			Size: info.Size(), Sum: hex.EncodeToString(sum[:]),
			Mode: info.Mode().Perm(), Mtime: info.ModTime(),
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("快照 %s 失败: %v", root, err)
	}
	return out
}

// assertTreeUnchanged 断言两棵树的每个文件**逐字节**相同（并且没被重写过）。
func assertTreeUnchanged(t *testing.T, what string, before, after map[string]fileState) {
	t.Helper()
	assertTreeUnchangedIgnoring(t, what, before, after)
}

// assertTreeUnchangedIgnoring 同上，但允许若干**追加式**文件变化。
//
// 存在的理由：操作日志（`operations.jsonl`）按契约**必须**为失败留一条 result=failed 的记录
// （UC-014 A13），因此「拒绝不改盘」指的是**live 配置与记账**不变，不含这条审计尾迹。
// 允许清单必须是显式的：把「哪些文件可以变」写清楚，比把断言整体放宽安全得多。
func assertTreeUnchangedIgnoring(t *testing.T, what string, before, after map[string]fileState, allowed ...string) {
	t.Helper()
	permitted := func(path string) bool {
		for _, prefix := range allowed {
			if strings.HasPrefix(path, prefix) {
				return true
			}
		}
		return false
	}
	for path, want := range before {
		if permitted(path) {
			continue
		}
		got, ok := after[path]
		if !ok {
			t.Errorf("%s：%s 消失了", what, path)
			continue
		}
		if got.Sum != want.Sum {
			t.Errorf("%s：%s 字节变了（%s → %s）", what, path, want.Sum, got.Sum)
		}
		if !got.Mtime.Equal(want.Mtime) {
			t.Errorf("%s：%s 被重写过（mtime %v → %v）", what, path, want.Mtime, got.Mtime)
		}
	}
	for path := range after {
		if permitted(path) {
			continue
		}
		if _, ok := before[path]; !ok {
			t.Errorf("%s：多出了 %s", what, path)
		}
	}
}

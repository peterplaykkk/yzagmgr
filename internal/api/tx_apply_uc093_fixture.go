//go:build yzagmgr_uc093_fixture

package api

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

const uc093FixtureOperationEnv = "YZAGMGR_UC093_FIXTURE_OP"

// applyTransaction is the only fault-aware dispatcher in the dedicated
// UC-093 daemon build. The ordinary binary has no environment reader or fault
// adapter; an unset fixture mode here also forwards directly to tx.Apply.
func (s *Server) applyTransaction(lease *tx.Lease, opts tx.Options) (tx.Result, error) {
	mode := os.Getenv(uc093FixtureOperationEnv)
	if mode == "" {
		return tx.Apply(lease, opts)
	}
	if mode != "apply" && mode != "restore" {
		return tx.Result{}, fmt.Errorf("invalid UC-093 fixture operation %q", mode)
	}
	if lease == nil {
		return tx.Result{}, errors.New("UC-093 fixture received an empty transaction lease")
	}
	if lease.Operation != mode {
		return tx.Apply(lease, opts)
	}
	return s.applyUC093FilesystemFault(lease, opts)
}

type uc093FixtureTarget struct {
	path          string
	canonicalPath string
	dir           string
	mode          os.FileMode
	deny          os.FileMode
}

func (s *Server) applyUC093FilesystemFault(lease *tx.Lease, opts tx.Options) (result tx.Result, err error) {
	if runtime.GOOS != "darwin" {
		return tx.Result{}, errors.New("UC-093 filesystem fixture requires native macOS permission semantics")
	}
	if os.Geteuid() == 0 {
		return tx.Result{}, errors.New("UC-093 filesystem fixture refuses to run as root")
	}
	if lease.ToolID != string(family.Codex) {
		return tx.Result{}, fmt.Errorf("UC-093 fixture only accepts the Codex two-file transaction, got %q", lease.ToolID)
	}
	if opts.Backuper == nil {
		return tx.Result{}, errors.New("UC-093 fixture requires the normal backup writer")
	}
	if len(lease.Files) != 2 {
		return tx.Result{}, fmt.Errorf("UC-093 fixture requires the complete two-file plan, got %d files", len(lease.Files))
	}

	homePath, homeCanonical, err := uc093FixtureCanonicalHome(s.Env.Home)
	if err != nil {
		return tx.Result{}, err
	}
	var targets [2]uc093FixtureTarget
	for i, plan := range lease.Files {
		if plan.Delete || !plan.Existed || bytes.Equal(plan.Before, plan.After) {
			return tx.Result{}, fmt.Errorf("UC-093 fixture file %d must be an existing changed regular file", i)
		}
		fingerprint, ok := lease.Fingerprints[plan.Path]
		if !ok || fingerprint != tx.HashBytes(plan.Before) {
			return tx.Result{}, fmt.Errorf("UC-093 fixture file %d has no fresh preview fingerprint", i)
		}
		target, err := uc093FixtureCanonicalTarget(homePath, homeCanonical, plan.Path)
		if err != nil {
			return tx.Result{}, fmt.Errorf("UC-093 fixture file %d: %w", i, err)
		}
		current, err := os.ReadFile(plan.Path)
		if err != nil {
			return tx.Result{}, fmt.Errorf("UC-093 fixture cannot reread file %d before apply: %w", i, err)
		}
		if !bytes.Equal(current, plan.Before) {
			return tx.Result{}, fmt.Errorf("UC-093 fixture file %d no longer matches preview bytes", i)
		}
		targets[i] = target
	}
	if targets[0].canonicalPath == targets[1].canonicalPath || targets[0].dir == targets[1].dir {
		return tx.Result{}, errors.New("UC-093 fixture requires two unique files in distinct parent directories")
	}

	// Register cleanup before any chmod preflight. A failed preflight restore
	// must get the same second restore attempt and retained-root signal as the
	// transaction's forward-write/rollback path.
	cleanupTargets := targets
	cleanupRequired := [2]bool{}
	defer func() {
		recovered := recover()
		cleanupErr := uc093FixtureRestoreDirectories(cleanupTargets[:], cleanupRequired[:])
		if cleanupErr != nil {
			cleanupErr = errors.Join(cleanupErr, uc093FixtureWriteCleanupMarker(homePath, homeCanonical, s.Env.DataRoot()))
			s.log.Error("UC-093 fixture permission/ACL cleanup failed; retain private HOME", "err", cleanupErr)
		}
		if recovered != nil {
			if cleanupErr != nil {
				panic(fmt.Errorf("UC-093 fixture cleanup failed during panic: %v; cleanup: %w", recovered, cleanupErr))
			}
			panic(recovered)
		}
		if cleanupErr != nil {
			result = tx.Result{}
			err = fmt.Errorf("UC-093 fixture permission/ACL cleanup failed: %w", cleanupErr)
		}
	}()

	for i, target := range targets {
		if target.mode.Perm()&0o300 != 0o300 || target.mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return tx.Result{}, fmt.Errorf("UC-093 fixture parent is not a plain owner-writable directory: %s", target.dir)
		}
		if err := uc093FixtureRequireNoACL(target.dir); err != nil {
			return tx.Result{}, err
		}
		cleanupRequired[i] = true
		if err := uc093FixtureVerifyChmodDeniesCreateTemp(target.dir, target.mode); err != nil {
			return tx.Result{}, err
		}
	}

	// The second target's directory is made read-only before Apply begins. The
	// first target remains writable until its actual post-write callback.
	cleanupRequired[1] = true
	if err := os.Chmod(targets[1].dir, targets[1].deny); err != nil {
		return tx.Result{}, fmt.Errorf("UC-093 fixture could not block second target: %w", err)
	}
	if err := uc093FixtureRequireNoACL(targets[1].dir); err != nil {
		return tx.Result{}, err
	}
	if err := uc093FixtureRequireCreateTempDenied(targets[1].dir); err != nil {
		return tx.Result{}, err
	}

	hookCalls := 0
	var hookErr error
	afterFirstWrite := func(path string) {
		if path != targets[0].path {
			return
		}
		hookCalls++
		if hookCalls != 1 {
			hookErr = errors.New("UC-093 first-file post-write hook ran more than once")
			return
		}
		cleanupRequired[0] = true
		if chmodErr := os.Chmod(targets[0].dir, targets[0].deny); chmodErr != nil {
			hookErr = fmt.Errorf("UC-093 fixture could not block first target after write: %w", chmodErr)
			return
		}
		if aclErr := uc093FixtureRequireNoACL(targets[0].dir); aclErr != nil {
			hookErr = aclErr
			return
		}
		if permissionErr := uc093FixtureRequireCreateTempDenied(targets[0].dir); permissionErr != nil {
			hookErr = permissionErr
		}
	}

	result, err = tx.ApplyWithUC093FixtureFault(lease, opts, afterFirstWrite)
	if hookErr != nil {
		return tx.Result{}, hookErr
	}
	if hookCalls != 1 {
		return tx.Result{}, fmt.Errorf("UC-093 fixture expected one first-file write callback, got %d", hookCalls)
	}
	return result, err
}

func uc093FixtureCanonicalHome(home string) (string, string, error) {
	configuredHome := os.Getenv("YZAGMGR_HOME")
	if configuredHome == "" {
		return "", "", errors.New("UC-093 fixture requires an explicit YZAGMGR_HOME")
	}
	configuredAbs, err := filepath.Abs(configuredHome)
	if err != nil {
		return "", "", fmt.Errorf("UC-093 fixture cannot resolve YZAGMGR_HOME: %w", err)
	}
	homeAbs, err := filepath.Abs(home)
	if err != nil || filepath.Clean(configuredAbs) != filepath.Clean(homeAbs) {
		return "", "", errors.New("UC-093 fixture YZAGMGR_HOME does not match the server HOME")
	}
	if !filepath.IsAbs(home) || filepath.Clean(home) != home {
		return "", "", errors.New("UC-093 fixture requires an absolute canonical private HOME")
	}
	st, err := os.Lstat(home)
	if err != nil {
		return "", "", fmt.Errorf("UC-093 fixture cannot inspect private HOME: %w", err)
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0o077 != 0 {
		return "", "", errors.New("UC-093 fixture requires a real private HOME with mode 0700 or stricter")
	}
	resolved, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", "", fmt.Errorf("UC-093 fixture cannot resolve private HOME: %w", err)
	}
	resolved = filepath.Clean(resolved)
	tempRoot, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return "", "", fmt.Errorf("UC-093 fixture cannot resolve system temporary root: %w", err)
	}
	relative, err := filepath.Rel(filepath.Clean(tempRoot), resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", errors.New("UC-093 fixture HOME is not below the system temporary root")
	}
	if err := uc093FixtureRequireNoACL(resolved); err != nil {
		return "", "", err
	}
	return filepath.Clean(home), resolved, nil
}

func uc093FixtureCanonicalTarget(homePath, homeCanonical, path string) (uc093FixtureTarget, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return uc093FixtureTarget{}, errors.New("target path is not absolute and clean")
	}
	lexicalRelative, err := filepath.Rel(homePath, path)
	if err != nil || lexicalRelative == "." || lexicalRelative == ".." || strings.HasPrefix(lexicalRelative, ".."+string(filepath.Separator)) {
		return uc093FixtureTarget{}, errors.New("target path is not lexically below private HOME")
	}
	components := strings.Split(lexicalRelative, string(filepath.Separator))
	cursor := homePath
	for i, component := range components {
		if component == "" || component == "." || component == ".." {
			return uc093FixtureTarget{}, errors.New("target path contains a non-canonical component")
		}
		cursor = filepath.Join(cursor, component)
		info, err := os.Lstat(cursor)
		if err != nil {
			return uc093FixtureTarget{}, fmt.Errorf("cannot lstat target path component: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return uc093FixtureTarget{}, fmt.Errorf("target path contains a symlink component: %s", cursor)
		}
		if i < len(components)-1 && !info.IsDir() {
			return uc093FixtureTarget{}, fmt.Errorf("target path component is not a directory: %s", cursor)
		}
		if i == len(components)-1 && !info.Mode().IsRegular() {
			return uc093FixtureTarget{}, fmt.Errorf("target is not an existing regular file: %s", cursor)
		}
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return uc093FixtureTarget{}, fmt.Errorf("cannot resolve target: %w", err)
	}
	resolved = filepath.Clean(resolved)
	relative, err := filepath.Rel(homeCanonical, resolved)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return uc093FixtureTarget{}, errors.New("target resolves outside private HOME")
	}
	canonicalPath := filepath.Join(homeCanonical, relative)
	if canonicalPath != resolved {
		return uc093FixtureTarget{}, errors.New("target physical path does not match its private HOME path")
	}
	fileInfo, err := os.Lstat(canonicalPath)
	if err != nil || !fileInfo.Mode().IsRegular() || fileInfo.Mode()&os.ModeSymlink != 0 {
		return uc093FixtureTarget{}, fmt.Errorf("target is not an existing regular file: %s", canonicalPath)
	}
	dir := filepath.Dir(canonicalPath)
	dirInfo, err := os.Lstat(dir)
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 {
		return uc093FixtureTarget{}, fmt.Errorf("target parent is not a real directory: %s", dir)
	}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil || filepath.Clean(resolvedDir) != filepath.Dir(canonicalPath) {
		return uc093FixtureTarget{}, fmt.Errorf("target parent contains a symlink: %s", dir)
	}
	parentRelative, err := filepath.Rel(homeCanonical, filepath.Clean(resolvedDir))
	if err != nil || parentRelative == ".." || strings.HasPrefix(parentRelative, ".."+string(filepath.Separator)) {
		return uc093FixtureTarget{}, errors.New("target parent resolves outside private HOME")
	}
	mode := dirInfo.Mode()
	return uc093FixtureTarget{
		path: path, canonicalPath: canonicalPath, dir: filepath.Clean(resolvedDir),
		mode: mode, deny: mode.Perm() &^ 0o222,
	}, nil
}

func uc093FixtureVerifyChmodDeniesCreateTemp(dir string, originalMode os.FileMode) (resultErr error) {
	defer func() {
		restoreErr := os.Chmod(dir, originalMode.Perm())
		aclErr := uc093FixtureRequireNoACL(dir)
		if restoreErr != nil || aclErr != nil {
			resultErr = errors.Join(resultErr, restoreErr, aclErr)
		}
	}()
	if err := os.Chmod(dir, originalMode.Perm()&^0o222); err != nil {
		return fmt.Errorf("UC-093 fixture chmod preflight failed: %w", err)
	}
	if err := uc093FixtureRequireCreateTempDenied(dir); err != nil {
		return err
	}
	return nil
}

func uc093FixtureRequireCreateTempDenied(dir string) error {
	file, err := os.CreateTemp(dir, ".yzagmgr-uc093-permission-probe-*")
	if err != nil {
		if os.IsPermission(err) {
			return nil
		}
		return fmt.Errorf("UC-093 fixture expected permission-denied CreateTemp, got: %w", err)
	}
	name := file.Name()
	closeErr := file.Close()
	removeErr := os.Remove(name)
	return errors.Join(errors.New("UC-093 fixture chmod did not physically deny CreateTemp"), closeErr, removeErr)
}

func uc093FixtureRequireNoACL(dir string) error {
	if runtime.GOOS != "darwin" {
		return errors.New("UC-093 fixture ACL verification currently requires native macOS")
	}
	cmd := exec.Command("/bin/ls", "-lde", dir)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("UC-093 fixture ACL readback failed: %w", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
	fields := strings.Fields(lines[0])
	if len(lines) != 1 || len(fields) == 0 || strings.Contains(fields[0], "+") {
		return fmt.Errorf("UC-093 fixture refuses ACL-bearing directory: %s", dir)
	}
	return nil
}

func uc093FixtureWriteCleanupMarker(homePath, homeCanonical, dataRoot string) error {
	canonicalDataRoot, err := uc093FixtureCanonicalDirectory(homePath, homeCanonical, dataRoot)
	if err != nil {
		return fmt.Errorf("locate UC-093 cleanup marker root: %w", err)
	}
	marker := filepath.Join(canonicalDataRoot, "uc093-fixture-cleanup-failed")
	if st, err := os.Lstat(marker); err == nil {
		if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() || st.Mode().Perm() != 0o600 {
			return errors.New("existing UC-093 cleanup marker is unsafe or has incorrect permissions")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect UC-093 cleanup marker: %w", err)
	}
	file, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create UC-093 cleanup marker without following links: %w", err)
	}
	content := []byte("UC-093 fixture chmod/ACL cleanup failed; preserve this private HOME for diagnosis.\n")
	_, writeErr := file.Write(content)
	chmodErr := file.Chmod(0o600)
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(writeErr, syncErr, closeErr, chmodErr)
}

func uc093FixtureCanonicalDirectory(homePath, homeCanonical, target string) (string, error) {
	if !filepath.IsAbs(target) || filepath.Clean(target) != target {
		return "", errors.New("directory path is not absolute and clean")
	}
	relative, err := filepath.Rel(homePath, target)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("directory path is not below private HOME")
	}
	cursor := homePath
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." || component == ".." {
			return "", errors.New("directory path contains a non-canonical component")
		}
		cursor = filepath.Join(cursor, component)
		st, err := os.Lstat(cursor)
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("directory path has an invalid or symlink component: %s", cursor)
		}
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", fmt.Errorf("resolve directory path: %w", err)
	}
	expected := filepath.Join(homeCanonical, relative)
	if filepath.Clean(resolved) != filepath.Clean(expected) {
		return "", errors.New("directory path does not resolve to its canonical private HOME location")
	}
	return filepath.Clean(resolved), nil
}

func uc093FixtureRestoreDirectories(targets []uc093FixtureTarget, required []bool) error {
	var restoreErrors []error
	for i := len(targets) - 1; i >= 0; i-- {
		if i >= len(required) || !required[i] {
			continue
		}
		target := targets[i]
		if err := os.Chmod(target.dir, target.mode.Perm()); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("restore directory mode %s: %w", target.dir, err))
			continue
		}
		st, err := os.Stat(target.dir)
		if err != nil || st.Mode().Perm() != target.mode.Perm() {
			restoreErrors = append(restoreErrors, fmt.Errorf("directory mode did not restore: %s", target.dir))
			continue
		}
		if err := uc093FixtureRequireNoACL(target.dir); err != nil {
			restoreErrors = append(restoreErrors, err)
		}
	}
	return errors.Join(restoreErrors...)
}

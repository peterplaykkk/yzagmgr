//go:build unix

package api

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/family/claudedesktop"
	"github.com/peterplaykkk/yzagmgr/internal/family/hermes"
	"github.com/peterplaykkk/yzagmgr/internal/family/pi"
)

const versionProbeStopWaitWindow = 200 * time.Millisecond

// TestInstallProbeStopWaitsForOwnedVersionRun holds a private Pi version stub
// inside its --version call, requests Stop while that exact run is active, and
// only then releases the stub. Stop must not return until the run and its cache
// publication have ended, so no owned probe work remains after shutdown.
func TestInstallProbeStopWaitsForOwnedVersionRun(t *testing.T) {
	t.Run("lifecycle guards without a CLI", func(t *testing.T) {
		registry := family.NewRegistry(claudedesktop.New())
		t.Run("stop before start fences later start", func(t *testing.T) {
			probe := newInstallProbe(registry, time.Hour, []family.ID{family.ClaudeDesktop})
			probe.stop()
			probe.start()
			probe.lifeMu.Lock()
			started, stopped := probe.started, probe.stopped
			probe.lifeMu.Unlock()
			if started || !stopped {
				t.Fatalf("stop-before-start lifecycle state started=%v stopped=%v", started, stopped)
			}
			if _, probed := probe.snapshot(family.ClaudeDesktop); probed {
				t.Fatal("starting after stop unexpectedly launched a background refresh")
			}
			probe.stop() // repeated stop must be safe after stop-before-start
		})
		t.Run("repeated start and stop are idempotent", func(t *testing.T) {
			probe := newInstallProbe(registry, time.Hour, []family.ID{family.ClaudeDesktop})
			probe.start()
			waitFor(t, "private no-CLI probe published its first snapshot", func() bool {
				_, probed := probe.snapshot(family.ClaudeDesktop)
				return probed
			})
			probe.start()
			probe.stop()
			probe.stop()
			if state, probed := probe.snapshot(family.ClaudeDesktop); !probed || len(state.copies) != 0 {
				t.Fatalf("idempotent lifecycle did not finish one selected no-CLI refresh: probed=%v state=%+v", probed, state)
			}
		})
	})

	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	readyPath := filepath.Join(home, "probe-ready")
	gatePath := filepath.Join(home, "probe-release.fifo")
	finishedPath := filepath.Join(home, "probe-finished")
	for _, path := range []string{gatePath} {
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatalf("create private version-probe FIFO: %v", err)
		}
	}
	stubPath := filepath.Join(bin, "pi")
	stub := `#!/bin/sh
set -eu
: > "$VERSION_PROBE_READY"
IFS= read -r release < "$VERSION_PROBE_GATE"
printf '%s\n' 'Pi version 1.2.3'
printf '%s' 'done' > "$VERSION_PROBE_FINISHED"
`
	if err := os.WriteFile(stubPath, []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin)
	t.Setenv("VERSION_PROBE_READY", readyPath)
	t.Setenv("VERSION_PROBE_GATE", gatePath)
	t.Setenv("VERSION_PROBE_FINISHED", finishedPath)
	copies := scanCopies(family.Pi)
	if len(copies) != 1 || copies[0].Path != stubPath || !copies[0].Active {
		t.Fatalf("version probe candidates are not exclusively the private Pi stub: %+v", copies)
	}

	probe := newInstallProbe(family.NewRegistry(pi.New()), time.Hour, []family.ID{family.Pi})
	probe.start()
	waitFor(t, "private Pi stub entered its gated version run", func() bool {
		_, err := os.Stat(readyPath)
		return err == nil
	})

	stopDone := make(chan struct{})
	var stopOnce sync.Once
	requestStop := func() {
		stopOnce.Do(func() {
			go func() {
				probe.stop()
				close(stopDone)
			}()
		})
	}
	var releaseOnce sync.Once
	releaseStub := func() {
		releaseOnce.Do(func() {
			writer, err := os.OpenFile(gatePath, os.O_WRONLY, 0)
			if err != nil {
				t.Errorf("open private release FIFO: %v", err)
				return
			}
			if _, err := fmt.Fprintln(writer, "continue"); err != nil {
				t.Errorf("release private Pi stub: %v", err)
			}
			if err := writer.Close(); err != nil {
				t.Errorf("close private release FIFO: %v", err)
			}
		})
	}
	t.Cleanup(func() {
		requestStop()
		if _, err := os.Stat(readyPath); err == nil {
			releaseStub()
			waitFor(t, "private Pi stub cleanup marker", func() bool {
				_, err := os.Stat(finishedPath)
				return err == nil
			})
		}
		select {
		case <-stopDone:
		case <-time.After(5 * time.Second):
			t.Error("version probe stop did not finish after releasing its private child")
		}
	})

	requestStop()
	select {
	case <-probe.stopCh:
	case <-time.After(5 * time.Second):
		t.Fatal("stop request did not close the probe stop channel")
	}
	returnedBeforeRelease := false
	select {
	case <-stopDone:
		returnedBeforeRelease = true
	case <-time.After(versionProbeStopWaitWindow):
		// This bounded assertion window observes the known in-flight child; it is
		// not used to synchronize the child, which is controlled by the FIFO gate.
	}

	// Unblock the exact test-owned child only after observing the Stop boundary.
	releaseStub()
	waitFor(t, "private Pi stub finished its version run", func() bool {
		_, err := os.Stat(finishedPath)
		return err == nil
	})
	if returnedBeforeRelease {
		t.Error("Stop returned while its owned --version child was still blocked and able to write")
	}
	select {
	case <-stopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not join the in-flight version probe after release")
	}
	if state, probed := probe.snapshot(family.Pi); !probed || state.version != "1.2.3" {
		t.Fatalf("Stop returned before the owned probe published its result: probed=%v version=%q", probed, state.version)
	}
	finished, err := os.ReadFile(finishedPath)
	if err != nil || string(finished) != "done" {
		t.Fatalf("owned probe completion marker invalid after Stop: %v", err)
	}

	t.Run("stop fences later family and copy probes", func(t *testing.T) {
		t.Run("later family is not launched", func(t *testing.T) {
			home := t.TempDir()
			bin := filepath.Join(home, ".local", "bin")
			if err := os.MkdirAll(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			readyPath := filepath.Join(home, "hermes-ready")
			gatePath := filepath.Join(home, "hermes-release.fifo")
			finishedPath := filepath.Join(home, "hermes-finished")
			piRanPath := filepath.Join(home, "pi-ran")
			if err := syscall.Mkfifo(gatePath, 0o600); err != nil {
				t.Fatalf("create private Hermes version FIFO: %v", err)
			}
			hermesStub := `#!/bin/sh
set -eu
: > "$VERSION_PROBE_READY"
IFS= read -r release < "$VERSION_PROBE_GATE"
printf '%s\n' 'Hermes version 9.8.7'
: > "$VERSION_PROBE_FINISHED"
`
			piStub := `#!/bin/sh
set -eu
: > "$VERSION_PROBE_PI_RAN"
printf '%s\n' 'Pi version 4.5.6'
`
			for path, content := range map[string]string{
				filepath.Join(bin, "hermes"): hermesStub,
				filepath.Join(bin, "pi"):     piStub,
			} {
				if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("HOME", home)
			t.Setenv("PATH", bin)
			t.Setenv("VERSION_PROBE_READY", readyPath)
			t.Setenv("VERSION_PROBE_GATE", gatePath)
			t.Setenv("VERSION_PROBE_FINISHED", finishedPath)
			t.Setenv("VERSION_PROBE_PI_RAN", piRanPath)
			if copies := scanCopies(family.Hermes); len(copies) != 1 || copies[0].Path != filepath.Join(bin, "hermes") {
				t.Fatalf("Hermes scan escaped the private stub: %+v", copies)
			}
			if copies := scanCopies(family.Pi); len(copies) != 1 || copies[0].Path != filepath.Join(bin, "pi") {
				t.Fatalf("Pi scan escaped the private stub: %+v", copies)
			}

			probe := newInstallProbe(family.NewRegistry(hermes.New(), pi.New()), time.Hour,
				[]family.ID{family.Hermes, family.Pi})
			probe.start()
			waitFor(t, "private Hermes stub entered its gated version run", func() bool {
				_, err := os.Stat(readyPath)
				return err == nil
			})
			stopDone := make(chan struct{})
			go func() {
				probe.stop()
				close(stopDone)
			}()
			select {
			case <-probe.stopCh:
			case <-time.After(5 * time.Second):
				t.Fatal("Stop did not fence the in-flight Hermes probe")
			}
			releaseVersionProbeFIFO(t, gatePath)
			waitFor(t, "private Hermes stub completed", func() bool {
				_, err := os.Stat(finishedPath)
				return err == nil
			})
			select {
			case <-stopDone:
			case <-time.After(5 * time.Second):
				t.Fatal("Stop did not join the in-flight Hermes probe")
			}
			if _, err := os.Stat(piRanPath); !os.IsNotExist(err) {
				t.Fatalf("later Pi family ran after Stop: stat err=%v", err)
			}
			if _, probed := probe.snapshot(family.Pi); probed {
				t.Fatal("Stop published a probe for the later Pi family")
			}
		})

		t.Run("later copy is not launched", func(t *testing.T) {
			home := t.TempDir()
			activeBin := filepath.Join(home, ".local", "bin")
			secondBin := filepath.Join(home, "bin")
			for _, dir := range []string{activeBin, secondBin} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			readyPath := filepath.Join(home, "pi-ready")
			gatePath := filepath.Join(home, "pi-release.fifo")
			finishedPath := filepath.Join(home, "pi-first-finished")
			secondRanPath := filepath.Join(home, "pi-second-ran")
			if err := syscall.Mkfifo(gatePath, 0o600); err != nil {
				t.Fatalf("create private Pi version FIFO: %v", err)
			}
			firstStub := `#!/bin/sh
set -eu
: > "$VERSION_PROBE_READY"
IFS= read -r release < "$VERSION_PROBE_GATE"
printf '%s\n' 'Pi version 1.2.3'
: > "$VERSION_PROBE_FINISHED"
`
			secondStub := `#!/bin/sh
set -eu
: > "$VERSION_PROBE_SECOND_RAN"
printf '%s\n' 'Pi version 7.8.9'
`
			firstPath := filepath.Join(activeBin, "pi")
			secondPath := filepath.Join(secondBin, "pi")
			if err := os.WriteFile(firstPath, []byte(firstStub), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(secondPath, []byte(secondStub), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			t.Setenv("PATH", activeBin)
			t.Setenv("VERSION_PROBE_READY", readyPath)
			t.Setenv("VERSION_PROBE_GATE", gatePath)
			t.Setenv("VERSION_PROBE_FINISHED", finishedPath)
			t.Setenv("VERSION_PROBE_SECOND_RAN", secondRanPath)
			copies := scanCopies(family.Pi)
			if len(copies) != 2 || copies[0].Path != firstPath || copies[1].Path != secondPath || !copies[0].Active {
				t.Fatalf("Pi scan did not resolve the two private copies in expected order: %+v", copies)
			}

			probe := newInstallProbe(family.NewRegistry(pi.New()), time.Hour, []family.ID{family.Pi})
			probe.start()
			waitFor(t, "first private Pi copy entered its gated version run", func() bool {
				_, err := os.Stat(readyPath)
				return err == nil
			})
			stopDone := make(chan struct{})
			go func() {
				probe.stop()
				close(stopDone)
			}()
			select {
			case <-probe.stopCh:
			case <-time.After(5 * time.Second):
				t.Fatal("Stop did not fence the first Pi copy probe")
			}
			releaseVersionProbeFIFO(t, gatePath)
			waitFor(t, "first private Pi copy completed", func() bool {
				_, err := os.Stat(finishedPath)
				return err == nil
			})
			select {
			case <-stopDone:
			case <-time.After(5 * time.Second):
				t.Fatal("Stop did not join the first Pi copy probe")
			}
			if _, err := os.Stat(secondRanPath); !os.IsNotExist(err) {
				t.Fatalf("second Pi copy ran after Stop: stat err=%v", err)
			}
			state, probed := probe.snapshot(family.Pi)
			if !probed || len(state.copies) != 2 || state.copies[0].Version != "1.2.3" || state.copies[1].Version != "" {
				t.Fatalf("Stop result should retain only the completed copy version: probed=%v copies=%+v", probed, state.copies)
			}
		})

		t.Run("already stopped probe skips family scan", func(t *testing.T) {
			probe := newInstallProbe(family.NewRegistry(pi.New()), time.Hour, []family.ID{family.Pi})
			probe.stop()
			probe.refreshAll()
			if _, probed := probe.snapshot(family.Pi); probed {
				t.Fatal("refreshAll scanned or published a family after Stop")
			}
		})

		t.Run("already stopped probe skips pre-scan refresh", func(t *testing.T) {
			probe := newInstallProbe(family.NewRegistry(pi.New()), time.Hour, []family.ID{family.Pi})
			probe.stop()
			if got := probe.refreshOneWithStopCheck(family.Pi, true); !reflect.DeepEqual(got, probeState{}) {
				t.Fatalf("stopped refreshOneWithStopCheck() = %+v, want empty state", got)
			}
			if _, probed := probe.snapshot(family.Pi); probed {
				t.Fatal("pre-scan stop guard published a probe state")
			}
		})
	})
}

func releaseVersionProbeFIFO(t *testing.T, path string) {
	t.Helper()
	writer, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open private version-probe release FIFO: %v", err)
	}
	if _, err := fmt.Fprintln(writer, "continue"); err != nil {
		t.Fatalf("release private version-probe child: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close private version-probe FIFO: %v", err)
	}
}

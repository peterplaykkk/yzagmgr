//go:build !unix

// 配置测试的进程编排（非 unix 兜底）。
//
// 本产品的主力平台是 macOS（unix，见 profiletest_process_unix.go 的整组回收）；这里只保证
// 「在别的平台上也能编译、也能跑一次测试」。能力差异**如实登记**：没有进程组语义时只能回收
// 直接子进程，CLI 拉起来的孙进程可能活过超时——这不是静默降级，`cmd.WaitDelay` 仍会保证
// 本端点自身在超时后有限时间内返回。
package api

import "os/exec"

// profileTestKillGrace 在非 unix 下无用（没有 TERM/KILL 两段式），保留常量让两平台的
// 引用面一致。
const profileTestKillGrace = 0

// setProcessGroup 在非 unix 上是空操作（没有 Setpgid 这一说）。
func setProcessGroup(*exec.Cmd) {}

// killProcessGroup 退化为「杀直接子进程」。
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

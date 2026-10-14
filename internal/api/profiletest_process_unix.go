//go:build unix

// 配置测试的进程编排（unix：自成进程组 + 按组回收）。
//
// 为什么必须按**进程组**回收（参考 scripts/verify/lib.sh 的 setsid/run_capped）：被测 CLI
// 会自己拉子进程（node worker、shell 包装、辅助进程），只对直接子进程 `Kill()` 会留下孤儿
// 继续持着管道与网络连接——`cmd.Wait()` 于是卡在 I/O 上不退，90s 上限形同虚设。
// 这里让子进程自成进程组（`Setpgid`），超时时向**整组**发 TERM、宽限后补 KILL。
//
// 平台分流只在「怎么让子进程脱离父进程组、怎么整组回收」这一点上不同；非 unix 的兜底
// 见 profiletest_process_other.go（能力如实登记为较弱：只回收直接子进程）。
package api

import (
	"os/exec"
	"syscall"
	"time"
)

// profileTestKillGrace 是 TERM 与 KILL 之间的宽限：给 CLI 一点时间收尾（写日志/关连接），
// 但它已经超时了，不值得等更久。
const profileTestKillGrace = 300 * time.Millisecond

// setProcessGroup 让子进程自成进程组（详见本文件头注释）。
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup 先 TERM 整组、宽限后 KILL 整组；进程已经退了就当成功（幂等）。
//
// 两处回落都不影响返回值：调用方（`cmd.Cancel`）要的是「尽力回收」，真正的退出码由
// `Wait` 之后的 ctx.Err() 判定（超时恒按 timedOut 上报，不依赖信号路径上的退出码）。
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		// 组已经不存在（或没有权限）：仍回落一次直接 kill，尽量把 pid 收掉。
		_ = cmd.Process.Kill()
		return nil
	}
	time.Sleep(profileTestKillGrace)
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		_ = cmd.Process.Kill()
	}
	return nil
}

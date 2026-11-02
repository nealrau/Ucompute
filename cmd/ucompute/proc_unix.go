//go:build !windows

package main

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
)

// shellCommand runs script with the worker's shell in its own process group,
// so cancelling a job stops everything it started.
func shellCommand(ctx context.Context, shell, script string) *exec.Cmd {
	if shell == "" {
		shell = defaultShell()
	}
	cmd := exec.CommandContext(ctx, shell, "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	return cmd
}

func defaultShell() string {
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	return "/bin/sh"
}

// killTree kills whatever is left of the job's process group.
func killTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

func exitCode(ps *os.ProcessState) int {
	if ps == nil {
		return exitUcompute
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

// preventSleep keeps a Mac awake while a job runs.
func preventSleep(pid int) {
	if runtime.GOOS != "darwin" {
		return
	}
	c := exec.Command("/usr/bin/caffeinate", "-i", "-w", strconv.Itoa(pid))
	if c.Start() == nil {
		go func() { _ = c.Wait() }()
	}
}

func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errLocked
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

func detachProcess(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

func processAlive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

func stopProcess(pid int) { _ = syscall.Kill(pid, syscall.SIGTERM) }

func platformMemory() uint64 { return 0 }

//go:build unix

package proc

import (
	"os/exec"
	"syscall"
)

// setGroup puts the command in its own process group, so wrappers like `go run`
// are stopped together with the binary they start.
func setGroup(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func terminate(c *exec.Cmd) { syscall.Kill(-c.Process.Pid, syscall.SIGTERM) }
func kill(c *exec.Cmd)      { syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }

// alive reports whether a process with this pid exists on this host.
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

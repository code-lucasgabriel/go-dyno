//go:build !unix

package proc

import (
	"os"
	"os/exec"
)

func setGroup(c *exec.Cmd)  {}
func terminate(c *exec.Cmd) { c.Process.Signal(os.Interrupt) }
func kill(c *exec.Cmd)      { c.Process.Kill() }

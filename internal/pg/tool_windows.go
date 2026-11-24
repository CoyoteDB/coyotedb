//go:build windows

package pg

import (
	"os"
	"os/exec"
)

// Tools inherit the agent's kill-on-close Job Object (guard_windows.go)
// and, like the server, run without administrative rights
// (token_windows.go): postgres --single refuses them too.
func (g *Guard) prepareTool(cmd *exec.Cmd) { cmd.SysProcAttr = g.postgresSysProcAttr() }

func (g *Guard) trackTool(int) error { return nil }
func (g *Guard) untrackTool(int)     {}

func killTool(p *os.Process) { _ = p.Kill() }

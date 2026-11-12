package kimi

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestRepeatedStopDoesNotClaimALiveChildExited(t *testing.T) {
	child := &Child{command: &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}}, stopped: true, exitWait: make(chan struct{})}
	for range 2 {
		if err := child.Stop(time.Millisecond); err == nil || err.Error() != "kimi_normal_exit_required" {
			t.Fatalf("a previous stop request is not proof of exit: %v", err)
		}
	}
	close(child.exitWait)
	if err := child.Stop(time.Millisecond); err != nil {
		t.Fatalf("exited child: %v", err)
	}
}

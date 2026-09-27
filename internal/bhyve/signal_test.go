package bhyve

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeRun struct {
	comm     string
	psErr    error
	commands []string
}

func (f *fakeRun) run(_ context.Context, name string, args ...string) (string, error) {
	f.commands = append(f.commands, name+" "+strings.Join(args, " "))
	if name == "ps" {
		return f.comm, f.psErr
	}
	return "", nil
}

func (f *fakeRun) killed() bool {
	for _, c := range f.commands {
		if strings.HasPrefix(c, "kill ") {
			return true
		}
	}
	return false
}

func TestSignalIfSupervisor_KillsADaemonProcess(t *testing.T) {
	f := &fakeRun{comm: "daemon\n"}
	signalIfSupervisor(context.Background(), 4242, f.run)
	if !f.killed() {
		t.Errorf("commands = %v, want a kill of the daemon(8) supervisor", f.commands)
	}
}

// The pidfile can outlive its process. If the pid now belongs to something
// else, it must not be signalled - this runs as root.
func TestSignalIfSupervisor_DoesNotKillAReusedPid(t *testing.T) {
	for _, comm := range []string{"sshd", "bhyve", "nginx", "", "daemonize"} {
		f := &fakeRun{comm: comm}
		signalIfSupervisor(context.Background(), 4242, f.run)
		if f.killed() {
			t.Errorf("comm %q: kill was sent to a process that is not a daemon(8) supervisor; commands = %v", comm, f.commands)
		}
	}
}

func TestSignalIfSupervisor_ProcessAlreadyGoneIsANoOp(t *testing.T) {
	f := &fakeRun{psErr: errors.New("ps: exit status 1")}
	signalIfSupervisor(context.Background(), 4242, f.run)
	if f.killed() {
		t.Errorf("kill was sent although ps reported the process gone; commands = %v", f.commands)
	}
}

func TestSignalIfSupervisor_RefusesPidsThatCouldNeverBeOurs(t *testing.T) {
	for _, pid := range []int{0, 1, -1} {
		f := &fakeRun{comm: "daemon"}
		signalIfSupervisor(context.Background(), pid, f.run)
		if len(f.commands) != 0 {
			t.Errorf("pid %d: ran %v, want nothing (kill -1 signals every process)", pid, f.commands)
		}
	}
}

func TestSignalIfSupervisor_AcceptsAFullPathComm(t *testing.T) {
	f := &fakeRun{comm: "/usr/sbin/daemon"}
	signalIfSupervisor(context.Background(), 4242, f.run)
	if !f.killed() {
		t.Errorf("a full-path comm should still be recognised as daemon(8); commands = %v", f.commands)
	}
}

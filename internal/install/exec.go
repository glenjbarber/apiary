package install

import (
	"bytes"
	"context"
	"os/exec"
)

// execRunner is Runner's real, host-shelling implementation - this
// package's own private exec helper, matching every other internal/*
// package's stated convention of keeping its own copy rather than
// sharing one across package boundaries.
type execRunner struct{}

// NewExecRunner returns the Runner every real (non-test) caller should
// use.
func NewExecRunner() Runner { return execRunner{} }

func (execRunner) Run(ctx context.Context, name string, args ...string) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

// Package infra drives the Docker engine: the compose stack (`up`/`down`),
// image builds, environment checks (`doctor`) and the containers of the
// services under test. It shells out to the docker CLI on purpose: the CLI is
// the one dependency every TestKit host already has, and every command it
// runs can be copy-pasted by a human to reproduce what TestKit did.
package infra

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Docker runs docker CLI commands.
type Docker struct {
	Bin string
	// Log, when set, receives every command line before it runs.
	Log io.Writer
	// Env is appended to the process environment of every command.
	Env []string
}

func NewDocker(log io.Writer) *Docker {
	bin := os.Getenv("TESTKIT_DOCKER")
	if bin == "" {
		bin = "docker"
	}
	return &Docker{Bin: bin, Log: log}
}

// CmdError carries the stderr of a failed docker command.
type CmdError struct {
	Args   []string
	Err    error
	Stderr string
}

func (e *CmdError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if len(msg) > 2000 {
		msg = "..." + msg[len(msg)-2000:]
	}
	return fmt.Sprintf("docker %s: %v: %s", strings.Join(e.Args, " "), e.Err, msg)
}

func (d *Docker) cmd(ctx context.Context, args []string) *exec.Cmd {
	c := exec.CommandContext(ctx, d.Bin, args...)
	c.Env = append(os.Environ(), d.Env...)
	if d.Log != nil {
		fmt.Fprintf(d.Log, "$ docker %s\n", shellJoin(args))
	}
	return c
}

// Run executes docker with args and returns trimmed stdout.
func (d *Docker) Run(ctx context.Context, args ...string) (string, error) {
	c := d.cmd(ctx, args)
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Run(); err != nil {
		return stdout.String(), &CmdError{Args: args, Err: err, Stderr: stderr.String() + stdout.String()}
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Stream executes docker and copies stdout+stderr to w.
func (d *Docker) Stream(ctx context.Context, w io.Writer, args ...string) error {
	c := d.cmd(ctx, args)
	var tail tailBuffer
	c.Stdout = io.MultiWriter(w, &tail)
	c.Stderr = io.MultiWriter(w, &tail)
	if err := c.Run(); err != nil {
		return &CmdError{Args: args, Err: err, Stderr: tail.String()}
	}
	return nil
}

// Lines splits non-empty output lines.
func Lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 8192 {
		t.b = t.b[len(t.b)-8192:]
	}
	return len(p), nil
}
func (t *tailBuffer) String() string { return string(t.b) }

func shellJoin(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\"'$`\\|&;<>(){}*?") {
			q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		} else {
			q[i] = a
		}
	}
	return strings.Join(q, " ")
}

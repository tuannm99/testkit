// Command testkit is the single entry point of TestKit:
//
//	doctor   check the host (docker, compose, privileges, ports, RAM, disk)
//	up       start the infrastructure profiles a set of services needs
//	down     remove everything TestKit created (idempotent)
//	status   show running infrastructure
//
// Later phases add lint, plan, run, report, pack and baseline.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	root := newRoot()
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		code := 1
		if e, ok := err.(exitCoder); ok {
			code = e.ExitCode()
		}
		os.Exit(code)
	}
}

type exitCoder interface{ ExitCode() int }

type exitErr struct {
	code int
	msg  string
}

func (e exitErr) Error() string { return e.msg }
func (e exitErr) ExitCode() int { return e.code }

type globals struct {
	root           string
	verbose        bool
	recordBaseline bool
	baselineRuns   int
}

func newRoot() *cobra.Command {
	g := &globals{}
	cmd := &cobra.Command{
		Use:           "testkit",
		Short:         "TestKit: functional, performance and chaos testing on Docker",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.PersistentFlags().StringVar(&g.root, "root", ".", "directory inside the TestKit project (testkit.yaml is searched upwards)")
	cmd.PersistentFlags().BoolVarP(&g.verbose, "verbose", "v", false, "print every docker command")
	cmd.AddCommand(newDoctorCmd(g), newAdmitCmd(g), newPackCmd(g), newQCCmd(g), newAICmd(g), newUpCmd(g), newDownCmd(g), newStatusCmd(g),
		newAnnotateCmd(g), newCollectCmd(g),
		newLintCmd(g), newPlanCmd(g), newStepsCmd(g), newRunCmd(g), newReportCmd(g), newVerifyCmd(g), newBaselineCmd(g))
	return cmd
}

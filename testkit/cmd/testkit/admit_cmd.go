package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tuannm99/testkit/testkit/core/evidence"
	"github.com/tuannm99/testkit/testkit/core/orchestrator"
	"github.com/tuannm99/testkit/testkit/core/result"
	"github.com/tuannm99/testkit/testkit/core/scenario"
)

// newAdmitCmd is the mutation gate a case passes before a person approves it:
// lint → green on the unbroken system N times → red under every mutation
// (→ compared with a baseline for perf cases). The tool only reports; the
// approval is the person named by --by.
func newAdmitCmd(g *globals) *cobra.Command {
	var opt orchestrator.Options
	var approve, noObs, build bool
	var by string
	cmd := &cobra.Command{
		Use:   "admit <case.yaml|dir>...",
		Short: "Mutation gate for new cases: green repeatedly, red under every declared defect; --approve records the approval",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if approve && strings.TrimSpace(by) == "" {
				return exitErr{2, "admit: --approve needs --by <person who reviewed the case>"}
			}
			s, err := prepareRun(cmd, g, args)
			if err != nil {
				return err
			}
			var runnable []*scenario.Case
			for _, c := range s.cases {
				if why := orchestrator.AdmissionPrecheck(c); why != "" {
					fmt.Fprintf(out, "REJECT  %s: %s\n", c.ID, why)
					continue
				}
				runnable = append(runnable, c)
			}
			run := &result.Run{}
			var dir *evidence.Dir
			if len(runnable) > 0 {
				if err := s.start(cmd, g, runnable, build, noObs); err != nil {
					return err
				}
				opt.Mutations, opt.Retries, opt.Command = true, 0, os.Args
				opt.Suite = "admission"
				if run, dir, err = s.r.Run(cmd.Context(), runnable, opt); err != nil {
					return err
				}
			}
			run.Admission = orchestrator.Admit(run, s.cases, opt.Stability)
			for _, a := range run.Admission {
				a.File = s.p.Rel(a.File) // the bundle must not depend on where the repo was checked out
			}
			sum := ""
			if dir != nil {
				if err := s.finish(cmd.Context(), run, dir); err != nil {
					return err
				}
				raw, err := os.ReadFile(dir.Path("manifest.json"))
				if err != nil {
					return err
				}
				h := sha256.Sum256(raw)
				sum = hex.EncodeToString(h[:])
			}
			ok := printAdmission(out, run.Admission)
			if dir != nil {
				fmt.Fprintf(out, "report: %s\n", dir.Path("report.html"))
			}
			if !ok {
				return exitErr{1, "admit: not every case was admitted; nothing was approved"}
			}
			if !approve {
				fmt.Fprintln(out, "every case admitted; approve with --approve --by <name> after reviewing the report")
				return nil
			}
			for _, a := range run.Admission {
				err := scenario.Approve(s.p.Abs(a.File), scenario.Admission{RunID: run.RunID, At: time.Now().UTC().Format(time.RFC3339),
					By: by, Stability: a.Stability, Killed: a.Killed, Manifest: sum})
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "approved %s by %s (%s)\n", a.CaseID, by, a.File)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&opt.Stability, "stability", 2, "passes in a row required on the unbroken system, per trigger")
	cmd.Flags().IntVar(&opt.Parallel, "parallel", 1, "executions in parallel")
	cmd.Flags().StringVar(&opt.RunID, "run-id", "", "run id (default: generated)")
	cmd.Flags().BoolVar(&approve, "approve", false, "if every case is admitted, set status: approved and record the admission in the case file")
	cmd.Flags().StringVar(&by, "by", "", "the person approving (required with --approve)")
	cmd.Flags().BoolVar(&noObs, "no-observability", false, "do not capture Grafana panels/annotations")
	cmd.Flags().BoolVar(&build, "build", false, "rebuild service images first")
	return cmd
}

// printAdmission prints one block per case and reports whether all were admitted.
func printAdmission(w io.Writer, adm []*result.Admission) bool {
	all := len(adm) > 0
	fmt.Fprintln(w, "\nadmission (mutation gate):")
	for _, a := range adm {
		fmt.Fprintf(w, "  %-10s %s\n", strings.ToUpper(a.Status), a.CaseID)
		for _, r := range a.Rules {
			mark := "ok  "
			if !r.OK {
				mark = "FAIL"
			} else if r.Skipped {
				mark = "skip"
			}
			fmt.Fprintf(w, "    %s %-18s %s\n", mark, r.Name, r.Detail)
		}
		if a.Status != result.Admitted {
			all = false
		}
	}
	return all
}

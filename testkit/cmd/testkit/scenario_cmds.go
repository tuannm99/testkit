package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/tuannm99/testkit/testkit/adapters/collect/grafana"
	"github.com/tuannm99/testkit/testkit/adapters/sut"
	"github.com/tuannm99/testkit/testkit/core/assert"
	"github.com/tuannm99/testkit/testkit/core/config"
	"github.com/tuannm99/testkit/testkit/core/evidence"
	"github.com/tuannm99/testkit/testkit/core/infra"
	"github.com/tuannm99/testkit/testkit/core/kit"
	"github.com/tuannm99/testkit/testkit/core/orchestrator"
	"github.com/tuannm99/testkit/testkit/core/perf"
	"github.com/tuannm99/testkit/testkit/core/report"
	"github.com/tuannm99/testkit/testkit/core/result"
	"github.com/tuannm99/testkit/testkit/core/scenario"
	"github.com/tuannm99/testkit/testkit/steps"
)

const toolVersion = "0.1.0"

// loadCases resolves files and directories (default: scenarios_dir).
func loadCases(p *config.Project, args []string) ([]*scenario.Case, error) {
	if len(args) == 0 {
		args = []string{p.Abs(p.ScenariosDir)}
	}
	var out []*scenario.Case
	seen := map[string]string{}
	for _, a := range args {
		st, err := os.Stat(a)
		if err != nil {
			return nil, err
		}
		var cs []*scenario.Case
		if st.IsDir() {
			cs, err = scenario.LoadDir(a)
		} else {
			var c *scenario.Case
			c, err = scenario.Load(a)
			cs = []*scenario.Case{c}
		}
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			if prev, dup := seen[c.ID]; dup {
				return nil, fmt.Errorf("case id %s declared twice (%s, %s)", c.ID, prev, c.File)
			}
			seen[c.ID] = c.File
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// lintAll lints cases and prints issues; it returns the number of errors.
func lintAll(w io.Writer, cases []*scenario.Case, services map[string]*config.Service, reg *kit.Registry) int {
	errs := 0
	for _, c := range cases {
		issues := scenario.Lint(c, services, reg)
		for _, i := range issues {
			fmt.Fprintln(w, i.String())
			if i.Severity == "error" {
				errs++
			}
		}
	}
	return errs
}

func newLintCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "lint [case.yaml|dir]...",
		Short: "Validate test cases: schema, mandatory fields, steps and checks exist, connectors declared by the service",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := config.LoadProject(g.root)
			if err != nil {
				return err
			}
			services, err := config.LoadServices(p.Abs(p.ServicesDir))
			if err != nil {
				return err
			}
			cases, err := loadCases(p, args)
			if err != nil {
				return err
			}
			n := lintAll(cmd.OutOrStdout(), cases, services, steps.Registry())
			if n > 0 {
				return exitErr{2, fmt.Sprintf("lint: %d error(s) in %d case(s)", n, len(cases))}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "lint: %d case(s) OK\n", len(cases))
			return nil
		},
	}
}

func newStepsCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "steps",
		Short: "List the steps and checks a scenario may use",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "STEP\tCONNECTOR\tPARAMS\tDESCRIPTION")
			for _, d := range steps.Defs {
				params := strings.Join(d.Required, ",")
				if len(d.Optional) > 0 && d.Name != "assert" && d.Name != "wait.until" {
					params += " [" + strings.Join(d.Optional, ",") + "]"
				}
				conn := d.Connector
				if conn == "" {
					conn = "(built-in)"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.Name, conn, params, d.Doc)
			}
			tw.Flush()
			fmt.Fprintln(out)
			tw = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "CHECK\tCONNECTOR\tEXAMPLES")
			for _, c := range steps.Checks {
				fmt.Fprintf(tw, "%s.*\t%s\t%s\n", c.Prefix, c.Connector, strings.Join(c.Examples, "  "))
			}
			tw.Flush()
			fmt.Fprintf(out, "\noperators: %s\n", strings.Join(assert.Operators, ", "))
			return nil
		},
	}
}

func newPlanCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "plan [case.yaml|dir]...",
		Short: "Dry run: print what would be provisioned, called and asserted (no infrastructure touched)",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			p, err := config.LoadProject(g.root)
			if err != nil {
				return err
			}
			services, err := config.LoadServices(p.Abs(p.ServicesDir))
			if err != nil {
				return err
			}
			cases, err := loadCases(p, args)
			if err != nil {
				return err
			}
			reg := steps.Registry()
			if n := lintAll(out, cases, services, reg); n > 0 {
				return exitErr{2, fmt.Sprintf("plan: fix %d lint error(s) first", n)}
			}
			for _, c := range cases {
				for _, trig := range c.Triggers() {
					if err := planCase(out, p, services[c.Service], reg, c, trig); err != nil {
						return err
					}
				}
			}
			return nil
		},
	}
}

func planCase(w io.Writer, p *config.Project, svc *config.Service, reg *kit.Registry, c *scenario.Case, trig string) error {
	runID := "r00000000-000000-plan"
	ns := kit.Namespace("tk_00000000000000plan_1")
	data, err := c.Data(runID, string(ns), trig)
	if err != nil {
		return err
	}
	specs, err := c.Expand()
	if err != nil {
		return err
	}
	stepsR, err := scenario.RenderSteps(specs, data)
	if err != nil {
		return err
	}
	id := c.ID
	if trig != "" {
		id += "[" + trig + "]"
	}
	fmt.Fprintf(w, "\n=== %s — %s (%s, %s, %s)\n", id, c.Title, strings.Join(c.Requirement, ","), c.Risk, c.Status)
	fmt.Fprintf(w, "purpose: %s\n", strings.TrimSpace(c.Purpose))
	fmt.Fprintf(w, "namespace: %s (one per execution; values below use a placeholder)\n", ns)
	fmt.Fprintln(w, "provision:")
	if svc.Stores.Postgres != nil {
		fmt.Fprintf(w, "  postgres       CREATE DATABASE %s; migrations %s\n", ns.Database(), svc.Stores.Postgres.Migrations)
	}
	if svc.Stores.Kafka != nil {
		var ts []string
		for _, t := range svc.Stores.Kafka.Topics {
			ts = append(ts, fmt.Sprintf("%s(%dp)", ns.Topic(t.Name), max(t.Partitions, 1)))
		}
		fmt.Fprintf(w, "  kafka          topics %s\n", strings.Join(ts, ", "))
	}
	if svc.Stores.Elasticsearch != nil {
		var is []string
		for n := range svc.Stores.Elasticsearch.Indices {
			is = append(is, ns.Index(n))
		}
		sort.Strings(is)
		fmt.Fprintf(w, "  elasticsearch  indices %s (replicas 0)\n", strings.Join(is, ", "))
	}
	if ch := svc.Stores.ClickHouse; ch != nil {
		fmt.Fprintf(w, "  clickhouse     CREATE DATABASE %s; migrations %s\n", ns.Database(), ch.Migrations)
	}
	if svc.Stores.Mongo != nil {
		fmt.Fprintf(w, "  mongo          database %s (dropped at teardown)\n", ns.Database())
	}
	if svc.Stores.Redis != nil {
		fmt.Fprintf(w, "  redis          key prefix %q (deleted at teardown)\n", ns.KeyPrefix())
	}
	for _, name := range config.SortedKeys(svc.Reconcile) {
		fmt.Fprintf(w, "  reconcile      %s across %d store(s)\n", name, len(svc.Reconcile[name].Sources))
	}
	for _, name := range config.SortedKeys(svc.Mocks) {
		m := svc.Mocks[name]
		fmt.Fprintf(w, "  mock           %s (%s) api %s verified %s against %s %s\n", name, m.Kind, m.APIVersion, m.VerifiedAt, m.VerifiedAgainst, m.OpenAPI)
	}
	if trig != "" {
		fmt.Fprintf(w, "  trigger        %s\n", trig)
	}
	env := &kit.Env{RunID: runID, NS: ns, CaseID: c.ID, Trigger: trig, Project: p, Service: svc,
		Internal: p.Endpoints(true), Vars: data["vars"].(map[string]any), Failpoint: c.Failpoints}
	vars, err := sut.RenderEnv(env)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "  service        %s image %s replicas %d restart %q\n", svc.Name, svc.ImageRef(len(c.Failpoints) > 0), max(c.SUT.Replicas, 1), c.SUT.Restart)
	for _, k := range config.SortedKeys(vars) {
		fmt.Fprintf(w, "                   %s=%s\n", k, sut.Redact(k, vars[k]))
	}
	fmt.Fprintln(w, "steps:")
	for i, s := range stepsR {
		def, _ := reg.Lookup(s.Name)
		conn := def.Connector
		if conn == "" {
			conn = "built-in"
		}
		fmt.Fprintf(w, "  %2d. %-16s [%s] %v\n", i+1, s.Name, conn, s.With)
	}
	fmt.Fprintf(w, "assert (poll until all hold, max %s; then drain the trigger and re-check):\n", c.WithinDuration())
	for _, e := range c.Expectations() {
		exp := ""
		if assert.NeedsExpected(e.Op) {
			exp = assert.Show(e.Expected)
		}
		fmt.Fprintf(w, "  %-4s %s %s %s   — %s\n", e.ID, e.Check, orchestrator.OpText(e.Op), exp, e.Why)
	}
	fmt.Fprintln(w, "evidence:")
	fmt.Fprintf(w, "  service logs, container state; snapshots %v; mock journals; mails; topic dumps\n", snapshotTables(svc))
	if len(c.Evidence.Grafana) > 0 {
		fmt.Fprintf(w, "  grafana panels %v (PNG + raw query_range JSON/CSV + locked link)\n", c.Evidence.Grafana)
	}
	for _, m := range c.Mutations {
		fmt.Fprintf(w, "mutation %s: with failpoint %s the case must go red %v (--mutations)\n", m.ID, m.Failpoint, m.ExpectRed)
	}
	return nil
}

func snapshotTables(svc *config.Service) []string {
	if svc.Stores.Postgres == nil {
		return nil
	}
	return svc.Stores.Postgres.Snapshot
}

// runSetup is what `run` and `admit` share: linted cases, healthy
// infrastructure, images, capabilities, observability and baselines.
type runSetup struct {
	p        *config.Project
	st       *infra.Stack
	services map[string]*config.Service
	cases    []*scenario.Case
	r        *orchestrator.Runner
}

func prepareRun(cmd *cobra.Command, g *globals, args []string) (*runSetup, error) {
	out := cmd.OutOrStdout()
	p, st, err := g.stack(out)
	if err != nil {
		return nil, err
	}
	services, err := config.LoadServices(p.Abs(p.ServicesDir))
	if err != nil {
		return nil, err
	}
	cases, err := loadCases(p, args)
	if err != nil {
		return nil, err
	}
	reg := steps.Registry()
	if n := lintAll(out, cases, services, reg); n > 0 {
		return nil, exitErr{2, fmt.Sprintf("run: %d lint error(s); nothing was run", n)}
	}
	return &runSetup{p: p, st: st, services: services, cases: cases,
		r: &orchestrator.Runner{Project: p, Registry: reg, Services: services, Out: out}}, nil
}

// start brings what the given cases need to a ready state and wires the runner.
func (s *runSetup) start(cmd *cobra.Command, g *globals, cases []*scenario.Case, build, noObs bool) error {
	out, ctx := cmd.OutOrStdout(), cmd.Context()
	// Infrastructure the cases need must be up and healthy.
	needSvc := map[string]*config.Service{}
	for _, c := range cases {
		needSvc[c.Service] = s.services[c.Service]
	}
	var need []string
	for _, svc := range needSvc {
		need = append(need, svc.Stores.Names()...)
		need = append(need, svc.MockComponents()...)
		if err := s.st.EnsureServiceImages(ctx, svc, build); err != nil {
			return err
		}
	}
	ui := false
	for _, c := range cases {
		if len(c.Chaos.Proxies) > 0 {
			need = append(need, "toxiproxy")
		}
		ui = ui || c.UsesUI()
	}
	if ui {
		if err := s.st.EnsureImage(ctx, "ui-runner", build); err != nil {
			return err
		}
	}
	if err := s.st.RequireHealthy(ctx, dedupeStrings(need)); err != nil {
		return err
	}
	caps, ok := s.st.LoadCapabilities()
	if !ok {
		caps = s.st.Doctor(ctx).Capabilities
	}
	s.r.Caps = map[string]bool{"docker.sock": caps.DockerSock, "NET_ADMIN": caps.NetAdmin, "netem": caps.Netem}
	if !noObs {
		if run, _ := s.st.Running(ctx); strings.HasPrefix(run["grafana"], "Up") && strings.HasPrefix(run["prometheus"], "Up") {
			obs := &grafana.Observer{C: collector(s.p)}
			if err := obs.Available(ctx); err == nil {
				s.r.Obs = obs
			} else {
				fmt.Fprintf(out, "WARN observability not reachable: %v\n", err)
			}
		}
	}
	s.r.Baselines, s.r.Fingerprint, s.r.Commit = baselineStore(ctx, s.p, s.st)
	s.r.RecordBaseline, s.r.BaselineRuns = g.recordBaseline, g.baselineRuns
	return nil
}

// finish writes the report files and seals the bundle.
func (s *runSetup) finish(ctx context.Context, w io.Writer, run *result.Run, dir *evidence.Dir) error {
	man := buildManifest(ctx, s.p, s.st, run, s.services)
	if err := report.WriteAll(dir, run, man); err != nil {
		return err
	}
	if err := exportQC(w, s.p, dir, run, s.cases); err != nil {
		return err
	}
	man.FinishedAt = time.Now().UTC()
	return dir.Seal(man)
}

func newRunCmd(g *globals) *cobra.Command {
	var opt orchestrator.Options
	var noObs, build, pack bool
	cmd := &cobra.Command{
		Use:   "run [case.yaml|dir]... | run <suite.yaml>",
		Short: "Run test cases and write the evidence bundle (report.html, junit.xml, traceability.csv, manifest.json)",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			// A suite file (kind: Suite) selects the cases and how to run them.
			var suite *scenario.Suite
			if len(args) == 1 && scenario.IsSuite(args[0]) {
				var err error
				if suite, err = scenario.LoadSuite(args[0]); err != nil {
					return err
				}
				args = suite.Paths()
				opt.Suite, opt.OnlyApproved = suite.Name, suite.ApprovedOnly()
				opt.Mutations = opt.Mutations || suite.Mutations
				if !cmd.Flags().Changed("retries") {
					opt.Retries = suite.Retries
				}
				if !cmd.Flags().Changed("parallel") && suite.Parallel > 0 {
					opt.Parallel = suite.Parallel
				}
			}
			s, err := prepareRun(cmd, g, args)
			if err != nil {
				return err
			}
			if suite != nil {
				s.cases = excludeCases(out, s.cases, suite.Exclude)
			}
			if err := s.start(cmd, g, s.cases, build, noObs); err != nil {
				return err
			}
			opt.Command = os.Args
			started := time.Now()
			run, dir, err := s.r.Run(cmd.Context(), s.cases, opt)
			if err != nil {
				return err
			}
			if suite != nil {
				run.Gate = orchestrator.Gate(run, suite, s.cases, time.Now())
			}
			if err := s.finish(cmd.Context(), out, run, dir); err != nil {
				return err
			}
			sumErr := printSummary(out, run, dir, time.Since(started))
			if suite == nil {
				return sumErr
			}
			printGate(out, run.Gate)
			if suite.Pack || pack {
				if _, err := packBundle(out, s.p, dir.Root); err != nil {
					return err
				}
			}
			if run.Gate.Decision != "GO" {
				return exitErr{1, "release gate: NO-GO"}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opt.RunID, "run-id", "", "run id (default: generated)")
	cmd.Flags().IntVar(&opt.Parallel, "parallel", 1, "executions in parallel (namespaces keep them isolated)")
	cmd.Flags().BoolVar(&opt.Keep, "keep", false, "keep namespaces and containers (debug)")
	cmd.Flags().StringSliceVar(&opt.Triggers, "trigger", nil, "only these triggers")
	cmd.Flags().IntVar(&opt.Retries, "retries", 0, "re-run failed executions to detect flaky tests (flaky never counts as pass)")
	cmd.Flags().BoolVar(&opt.Mutations, "mutations", false, "also run each case's mutations (counter-evidence)")
	cmd.Flags().BoolVar(&noObs, "no-observability", false, "do not capture Grafana panels/annotations")
	cmd.Flags().BoolVar(&build, "build", false, "rebuild service images first")
	cmd.Flags().BoolVar(&pack, "pack", false, "suite runs: zip the bundle to out/<run_id>.zip (also suite pack: true)")
	return cmd
}

func excludeCases(w io.Writer, cases []*scenario.Case, ids []string) []*scenario.Case {
	var out []*scenario.Case
	for _, c := range cases {
		if contains(ids, c.ID) {
			fmt.Fprintf(w, "suite: %s excluded\n", c.ID)
			continue
		}
		out = append(out, c)
	}
	return out
}

func printGate(w io.Writer, g *result.Gate) {
	fmt.Fprintf(w, "\nrelease gate: %s\n", g.Decision)
	for _, r := range g.Rules {
		mark := "ok  "
		if !r.Passed {
			mark = "FAIL"
		}
		fmt.Fprintf(w, "  %s %s — %s\n", mark, r.Name, r.Detail)
	}
	for _, q := range g.Flaky {
		fmt.Fprintf(w, "  quarantined %s (owner %s, until %s, %s)\n", q.CaseID, q.Owner, q.Deadline, q.Ticket)
	}
}

func dedupeStrings(l []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range l {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func printSummary(w io.Writer, run *result.Run, dir *evidence.Dir, d time.Duration) error {
	c := run.Counts()
	fmt.Fprintf(w, "\nrun %s: %d pass, %d fail, %d error, %d skipped in %s\n", run.RunID, c["pass"], c["fail"], c["error"], c["skipped"], d.Round(time.Second))
	for _, ex := range run.Executions {
		if ex.Mutation != "" {
			continue
		}
		killed := ""
		for _, m := range ex.Mutations {
			mark := "killed"
			switch {
			case m.Result == result.Error:
				mark = "NOT EVALUATED"
			case !m.Killed:
				mark = "SURVIVED"
			}
			killed += fmt.Sprintf(" [%s %s]", m.ID, mark)
		}
		fmt.Fprintf(w, "  %-7s %-40s %s%s\n", strings.ToUpper(ex.Result), ex.ID, ex.Reason, killed)
	}
	for _, p := range run.Parity {
		mark := "match"
		if !p.Match {
			mark = "MISMATCH " + strings.Join(p.Diffs, "; ")
		}
		fmt.Fprintf(w, "  parity  %-40s %s: %s\n", p.CaseID, strings.Join(p.Triggers, " vs "), mark)
	}
	fmt.Fprintf(w, "report: %s\n", dir.Path("report.html"))
	if c["fail"]+c["error"] > 0 {
		return exitErr{1, "run: failures"}
	}
	for _, p := range run.Parity {
		if !p.Match {
			return exitErr{1, "run: the same case gave different results through different triggers"}
		}
	}
	for _, ex := range run.Executions {
		for _, m := range ex.Mutations {
			if !m.Killed {
				return exitErr{1, "run: a mutation survived (a case did not go red with the system broken)"}
			}
		}
	}
	return nil
}

func gitOut(root string, args ...string) string {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	b, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func buildManifest(ctx context.Context, p *config.Project, st *infra.Stack, run *result.Run, services map[string]*config.Service) *evidence.Manifest {
	m := &evidence.Manifest{RunID: run.RunID, Tool: "testkit", ToolVersion: toolVersion, Command: run.Command,
		StartedAt: run.StartedAt, Images: p.PinnedImages(), Services: map[string]string{}, Digests: map[string]string{},
		Running: map[string]string{}, Env: map[string]string{}}
	m.Git = evidence.GitInfo{Commit: gitOut(p.Root, "rev-parse", "HEAD"), Branch: gitOut(p.Root, "rev-parse", "--abbrev-ref", "HEAD"),
		Dirty: gitOut(p.Root, "status", "--porcelain") != ""}
	host, _ := os.Hostname()
	dv, _ := st.D.Run(ctx, "version", "--format", "{{.Server.Version}}")
	m.Host = evidence.HostInfo{OS: runtime.GOOS, Arch: runtime.GOARCH, DockerVersion: dv, Hostname: host, CPUs: runtime.NumCPU()}
	if s, _ := st.LoadState(); s != nil {
		m.Running, m.Digests = s.Images, s.Digests
	}
	used := map[string]bool{}
	for _, ex := range run.Executions {
		used[ex.Service] = true
		if ex.Image != "" {
			m.Services[ex.Service+" ("+ex.Image+")"] = ex.ImageID
		}
	}
	for name := range used {
		svc := services[name]
		for _, mn := range config.SortedKeys(svc.Mocks) {
			mk := svc.Mocks[mn]
			m.Mocks = append(m.Mocks, evidence.MockProvenance{Service: name, Mock: mn, Kind: mk.Kind,
				APIVersion: mk.APIVersion, VerifiedAt: mk.VerifiedAt, Against: mk.VerifiedAgainst, Spec: mk.OpenAPI})
		}
	}
	for k, v := range p.Env() {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "password") || strings.Contains(lk, "secret") || strings.Contains(lk, "token") {
			continue
		}
		m.Env[k] = v
	}
	return m
}

func newReportCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "report <out/run-id>",
		Short: "Re-render report.html, junit.xml and traceability.csv of a run and re-seal its manifest",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := args[0]
			if err := requireIntact(root); err != nil {
				return err
			}
			run, err := report.LoadRun(root)
			if err != nil {
				return err
			}
			dir := &evidence.Dir{Root: root}
			var man evidence.Manifest
			if raw, err := os.ReadFile(filepath.Join(root, evidence.ManifestFile)); err == nil {
				_ = jsonUnmarshal(raw, &man)
			}
			if err := report.WriteAll(dir, run, &man); err != nil {
				return err
			}
			if err := dir.Seal(&man); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "report: %s\n", dir.Path("report.html"))
			return nil
		},
	}
}

func newVerifyCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "verify <out/run-id>",
		Short: "Check that no evidence file changed since the run (sha256 in manifest.json)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			problems, err := evidence.Verify(args[0])
			if err != nil {
				return err
			}
			for _, p := range problems {
				fmt.Fprintln(cmd.OutOrStdout(), p)
			}
			if len(problems) > 0 {
				return exitErr{5, fmt.Sprintf("verify: %d problem(s)", len(problems))}
			}
			fmt.Fprintln(cmd.OutOrStdout(), "verify: every file matches manifest.json")
			return nil
		},
	}
}

// baselineStore returns the baseline store and the fingerprint of this environment.
func baselineStore(ctx context.Context, p *config.Project, st *infra.Stack) (*perf.Store, perf.Fingerprint, string) {
	dir := p.BaselinesDir
	if dir == "" {
		dir = "testkit/baselines"
	}
	fp := perf.Fingerprint{OS: runtime.GOOS + "/" + runtime.GOARCH, Images: map[string]string{}}
	if out, err := st.D.Run(ctx, "info", "--format", "{{.NCPU}} {{.MemTotal}} {{.ServerVersion}}"); err == nil {
		f := strings.Fields(out)
		if len(f) == 3 {
			fp.CPUs, _ = strconv.Atoi(f[0])
			mem, _ := strconv.ParseInt(f[1], 10, 64)
			fp.MemoryGiB = int((mem + 1<<29) >> 30)
			fp.DockerVersion = f[2]
		}
	}
	for k, v := range p.PinnedImages() {
		switch k {
		case "POSTGRES_IMAGE", "KAFKA_IMAGE", "ELASTICSEARCH_IMAGE", "CLICKHOUSE_IMAGE", "MONGO_IMAGE", "REDIS_IMAGE":
			fp.Images[k] = v
		}
	}
	return &perf.Store{Dir: p.Abs(dir)}, fp, gitOut(p.Root, "rev-parse", "HEAD")
}

func newBaselineCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "baseline", Short: "Record and inspect performance baselines (per environment)"}
	var runs int
	record := &cobra.Command{
		Use:   "record <perf-case.yaml>...",
		Short: "Run perf cases N times and store the samples as the baseline of this environment",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			g.recordBaseline, g.baselineRuns = true, runs
			run := newRunCmd(g)
			run.SetArgs(args)
			run.SetOut(cmd.OutOrStdout())
			return run.ExecuteContext(cmd.Context())
		},
	}
	record.Flags().IntVar(&runs, "runs", 5, "repetitions to measure (5-10 recommended)")
	show := &cobra.Command{
		Use:   "show",
		Short: "List the baselines of this environment",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, st, err := g.stack(cmd.OutOrStdout())
			if err != nil {
				return err
			}
			store, fp, _ := baselineStore(cmd.Context(), p, st)
			dir := filepath.Join(store.Dir, fp.ID())
			files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
			fmt.Fprintf(cmd.OutOrStdout(), "environment %s (%d CPU, %d GiB, docker %s): %d baseline(s) in %s\n", fp.ID(), fp.CPUs, fp.MemoryGiB, fp.DockerVersion, len(files), dir)
			for _, f := range files {
				key := strings.TrimSuffix(filepath.Base(f), ".json")
				b, _, err := store.Load(fp, key)
				if err != nil || b == nil {
					continue
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %-28s recorded %s run %s (%s)\n", key, b.RecordedAt.Format(time.RFC3339), b.RunID, b.CaseID)
				for _, m := range config.SortedKeys(b.Median) {
					fmt.Fprintf(cmd.OutOrStdout(), "      %-12s median %-10.4g MAD %-8.3g samples %v\n", m, b.Median[m], b.MAD[m], b.Samples[m])
				}
			}
			return nil
		},
	}
	cmd.AddCommand(record, show)
	return cmd
}

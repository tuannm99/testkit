// Package playwright runs the UI tests of ui-tests/ (Playwright, TypeScript)
// inside the pinned Playwright image on the test network ("ui" connector).
// Screenshots, traces and the JSON report are written into the execution's
// evidence directory; pass/fail of the case comes from `ui.*` checks on the
// JSON report, like any other check.
package playwright

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tuannm99/testkit/testkit/core/infra"
	"github.com/tuannm99/testkit/testkit/core/kit"
)

type Connector struct {
	env  *kit.Env
	d    *infra.Docker
	runs []*Run
}

// Run is one ui.run step.
type Run struct {
	Name    string   `json:"name"`
	Spec    string   `json:"spec"`
	Dir     string   `json:"dir"` // relative to the run directory
	Tests   []Test   `json:"tests"`
	Errors  []string `json:"errors,omitempty"` // errors outside tests (config, compile)
	Exit    string   `json:"exit,omitempty"`
	Seconds float64  `json:"seconds"`
}

// Test is one Playwright test outcome.
type Test struct {
	Title       string   `json:"title"`
	File        string   `json:"file"`
	Status      string   `json:"status"` // expected | unexpected | flaky | skipped
	Error       string   `json:"error,omitempty"`
	Attachments []string `json:"attachments,omitempty"`
}

func New() kit.Connector { return &Connector{d: infra.NewDocker(nil)} }

func (c *Connector) Name() string                                    { return "ui" }
func (c *Connector) CheckPrefixes() []string                         { return []string{"ui"} }
func (c *Connector) Provision(_ context.Context, env *kit.Env) error { c.env = env; return nil }
func (c *Connector) Health(context.Context) error                    { return nil }
func (c *Connector) Teardown(context.Context) error                  { return nil }

// Collect lists the UI evidence of every run.
func (c *Connector) Collect(context.Context, kit.TimeWindow) ([]kit.Artifact, error) {
	var arts []kit.Artifact
	for _, r := range c.runs {
		arts = append(arts, kit.Artifact{Kind: "raw", Path: path.Join(r.Dir, "results.json"), Title: "Playwright report (JSON): " + r.Name, Source: "ui"})
		for _, t := range r.Tests {
			for _, a := range t.Attachments {
				kind := "raw"
				if strings.HasSuffix(a, ".png") {
					kind = "image"
				}
				arts = append(arts, kit.Artifact{Kind: kind, Path: a, Title: fmt.Sprintf("UI: %s — %s", t.Title, path.Base(a)), Source: "ui"})
			}
		}
	}
	return arts, nil
}

// baseURL is the service under test as seen on the network (port "http").
func (c *Connector) baseURL() string {
	port, ok := c.env.Service.Ports["http"]
	if !ok {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", c.env.NS.Container(c.env.Service.Name, 0, max(c.env.Replicas, 1)), port)
}

// Apply runs ui.run {spec, name?, base_url?, grep?, env?}.
func (c *Connector) Apply(ctx context.Context, s kit.Step) (kit.Result, error) {
	if s.Name != "ui.run" {
		return kit.Result{}, fmt.Errorf("ui: unknown step %s", s.Name)
	}
	spec := kit.Str(s.With, "spec")
	if spec == "" || strings.Contains(spec, "..") {
		return kit.Result{}, fmt.Errorf("ui.run: spec must be a path under ui-tests/tests")
	}
	name := kit.Str(s.With, "name")
	if name == "" {
		name = evidenceName(s.Label)
	}
	if name == "" {
		name = fmt.Sprintf("ui-%d", len(c.runs)+1)
	}
	base := kit.Str(s.With, "base_url")
	if base == "" {
		base = c.baseURL()
	}
	rel := path.Join(c.env.CaseDir, "output", "ui", name)
	dir := c.env.Evidence.Path(rel)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return kit.Result{}, err
	}
	_ = os.Chmod(dir, 0o777)
	args := []string{"run", "--rm", "--network", c.env.Project.Network, "--ipc", "host",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "-v", dir + ":/evidence",
		"--label", infra.LabelManaged + "=true", "--label", infra.LabelProject + "=" + c.env.Project.Name,
		"--label", infra.LabelRunID + "=" + c.env.RunID, "--label", "testkit.ns=" + string(c.env.NS),
		"--label", infra.LabelService + "=ui-runner",
		"-e", "BASE_URL=" + base, "-e", "TK_OUT=/evidence", "-e", "TESTKIT_RUN_ID=" + c.env.RunID, "-e", "TESTKIT_NS=" + string(c.env.NS)}
	if env, ok := s.With["env"].(map[string]any); ok {
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "-e", k+"="+fmt.Sprint(env[k]))
		}
	}
	args = append(args, c.env.Project.ImageTag("ui-runner"), path.Join("tests", spec))
	if g := kit.Str(s.With, "grep"); g != "" {
		args = append(args, "--grep", g)
	}
	started := time.Now()
	out, runErr := c.d.Run(ctx, args...)
	if ce, ok := runErr.(*infra.CmdError); ok {
		out = out + "\n" + ce.Stderr
	}
	_ = os.WriteFile(filepath.Join(dir, "playwright.log"), []byte(out+"\n"), 0o644)
	r := &Run{Name: name, Spec: spec, Dir: rel, Seconds: time.Since(started).Seconds()}
	if runErr != nil {
		r.Exit = runErr.Error()
	}
	if err := c.parse(r, filepath.Join(dir, "results.json")); err != nil {
		// No report: Playwright did not run (image, config, compile error).
		return kit.Result{Note: "playwright produced no report"}, fmt.Errorf("ui.run %s: %v (see %s/playwright.log)", spec, err, rel)
	}
	c.runs = append(c.runs, r)
	n := map[string]int{}
	for _, t := range r.Tests {
		n[t.Status]++
	}
	return kit.Result{Output: map[string]any{"tests": len(r.Tests), "passed": n["expected"], "failed": n["unexpected"], "flaky": n["flaky"]},
		Note: fmt.Sprintf("%s: %d test(s), %d passed, %d failed, %d flaky (base %s)", spec, len(r.Tests), n["expected"], n["unexpected"], n["flaky"], base)}, nil
}

// report is the part of Playwright's JSON reporter output we read.
type report struct {
	Suites []suite `json:"suites"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type suite struct {
	Title  string  `json:"title"`
	File   string  `json:"file"`
	Suites []suite `json:"suites"`
	Specs  []struct {
		Title string `json:"title"`
		File  string `json:"file"`
		Tests []struct {
			Status  string `json:"status"`
			Results []struct {
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
				Attachments []struct {
					Name string `json:"name"`
					Path string `json:"path"`
				} `json:"attachments"`
			} `json:"results"`
		} `json:"tests"`
	} `json:"specs"`
}

func (c *Connector) parse(r *Run, file string) error {
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var rep report
	if err := json.Unmarshal(raw, &rep); err != nil {
		return err
	}
	for _, e := range rep.Errors {
		r.Errors = append(r.Errors, stripANSI(e.Message))
	}
	var walk func(s suite, prefix string)
	walk = func(s suite, prefix string) {
		for _, sp := range s.Specs {
			for _, t := range sp.Tests {
				out := Test{Title: strings.TrimPrefix(prefix+" › "+sp.Title, " › "), File: sp.File, Status: t.Status}
				for _, res := range t.Results {
					if res.Error != nil && out.Error == "" {
						out.Error = stripANSI(res.Error.Message)
					}
					for _, a := range res.Attachments {
						// Paths are inside the container (/evidence/...): map them to the bundle.
						if p, ok := strings.CutPrefix(a.Path, "/evidence/"); ok {
							out.Attachments = append(out.Attachments, path.Join(r.Dir, p))
						}
					}
				}
				r.Tests = append(r.Tests, out)
			}
		}
		for _, sub := range s.Suites {
			p := prefix
			if sub.Title != "" && sub.File == "" {
				p = strings.TrimPrefix(prefix+" › "+sub.Title, " › ")
			}
			walk(sub, p)
		}
	}
	for _, s := range rep.Suites {
		walk(s, "")
	}
	return nil
}

// evidenceName turns a step label into a directory name.
func evidenceName(label string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(label) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < '@' || s[j] > '~') {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// Check resolves:
//
//	ui.tests              tests run by every ui.run of the execution
//	ui.passed | failed | flaky | skipped
//	ui.errors             errors outside tests (configuration, compilation)
//	ui.test(<title>).status   expected | unexpected | flaky | skipped
func (c *Connector) Check(_ context.Context, ref kit.CheckRef) (kit.Observation, error) {
	now := time.Now().UTC()
	var all []Test
	var errs []string
	for _, r := range c.runs {
		all = append(all, r.Tests...)
		errs = append(errs, r.Errors...)
	}
	src := fmt.Sprintf("Playwright JSON report (%d run(s))", len(c.runs))
	if len(c.runs) == 0 {
		return kit.Observation{At: now, Source: src}, fmt.Errorf("no ui.run step in this execution")
	}
	segs := ref.Segments
	if len(segs) == 2 {
		count := func(status string) int {
			n := 0
			for _, t := range all {
				if t.Status == status {
					n++
				}
			}
			return n
		}
		switch segs[1].Name {
		case "tests":
			return kit.Observation{Value: len(all), Raw: all, Source: src, At: now}, nil
		case "passed":
			return kit.Observation{Value: count("expected"), Raw: all, Source: src, At: now}, nil
		case "failed":
			return kit.Observation{Value: count("unexpected"), Raw: all, Source: src, At: now}, nil
		case "flaky":
			return kit.Observation{Value: count("flaky"), Raw: all, Source: src, At: now}, nil
		case "skipped":
			return kit.Observation{Value: count("skipped"), Raw: all, Source: src, At: now}, nil
		case "errors":
			return kit.Observation{Value: len(errs), Raw: errs, Source: src, At: now}, nil
		}
	}
	if len(segs) == 3 && segs[1].Name == "test" && len(segs[1].Args) == 1 && segs[2].Name == "status" {
		for _, t := range all {
			if t.Title == segs[1].Args[0] || strings.HasSuffix(t.Title, " › "+segs[1].Args[0]) {
				return kit.Observation{Value: t.Status, Raw: t, Source: src, At: now}, nil
			}
		}
		return kit.Observation{Value: nil, Source: src, At: now}, nil
	}
	return kit.Observation{At: now}, fmt.Errorf("expected ui.tests|passed|failed|flaky|skipped|errors or ui.test(<title>).status")
}

var (
	_ kit.Connector = (*Connector)(nil)
	_ kit.Checker   = (*Connector)(nil)
)

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	aianthropic "github.com/tuannm99/testkit/testkit/adapters/ai/anthropic"
	aicommand "github.com/tuannm99/testkit/testkit/adapters/ai/command"
	aiopenai "github.com/tuannm99/testkit/testkit/adapters/ai/openai"
	"github.com/tuannm99/testkit/testkit/core/ai"
	"github.com/tuannm99/testkit/testkit/core/config"
	"github.com/tuannm99/testkit/testkit/core/evidence"
	"github.com/tuannm99/testkit/testkit/core/kit"
	"github.com/tuannm99/testkit/testkit/core/report"
	"github.com/tuannm99/testkit/testkit/core/result"
	"github.com/tuannm99/testkit/testkit/core/scenario"
	"github.com/tuannm99/testkit/testkit/steps"
)

// aiProvider builds the configured provider (TK_AI_PROVIDER overrides).
func aiProvider(p *config.Project) (ai.Provider, error) {
	cfg := &config.AI{}
	if p.AI != nil {
		c := *p.AI
		cfg = &c
	}
	for env, dst := range map[string]*string{"TK_AI_PROVIDER": &cfg.Provider, "TK_AI_MODEL": &cfg.Model, "TK_AI_BASE_URL": &cfg.BaseURL} {
		if v := os.Getenv(env); v != "" {
			*dst = v
		}
	}
	if v := os.Getenv("TK_AI_COMMAND"); v != "" {
		cfg.Command = []string{"sh", "-c", v}
	}
	switch cfg.Provider {
	case "", "none":
		return ai.Manual{}, nil
	case "anthropic":
		return aianthropic.New(cfg)
	case "openai-compatible":
		return aiopenai.New(cfg)
	case "command":
		return aicommand.New(cfg)
	}
	return nil, fmt.Errorf("ai.provider %q is not supported (none, anthropic, openai-compatible, command)", cfg.Provider)
}

func aiClient(p *config.Project, auditDir, responseFile string) (*ai.Client, error) {
	prov, err := aiProvider(p)
	if err != nil {
		return nil, err
	}
	services, err := config.LoadServices(p.Abs(p.ServicesDir))
	if err != nil {
		return nil, err
	}
	c := &ai.Client{Provider: prov, Secrets: secretValues(p, services), AuditDir: auditDir}
	if responseFile != "" {
		raw, err := os.ReadFile(responseFile)
		if err != nil {
			return nil, err
		}
		c.Response = string(raw)
	}
	return c, nil
}

// loadBundle opens an intact run bundle for an AI task.
func loadBundle(root string) (*evidence.Dir, *evidence.Manifest, error) {
	if err := requireIntact(root); err != nil {
		return nil, nil, err
	}
	var man evidence.Manifest
	raw, err := os.ReadFile(filepath.Join(root, evidence.ManifestFile))
	if err != nil {
		return nil, nil, err
	}
	if err := jsonUnmarshal(raw, &man); err != nil {
		return nil, nil, err
	}
	return &evidence.Dir{Root: root}, &man, nil
}

func newAICmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "ai", Short: "Optional assistant: triage suggestions, run summaries, draft test cases (never decides a result; only redacted data is sent)"}
	var response, task string
	ctxCmd := &cobra.Command{
		Use:   "context <out/run-id>",
		Short: "Print exactly what would be sent for a task (redacted), without sending anything",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.project()
			if err != nil {
				return err
			}
			_, man, err := loadBundle(args[0])
			if err != nil {
				return err
			}
			run, err := report.LoadRun(args[0])
			if err != nil {
				return err
			}
			var req ai.Request
			switch task {
			case "triage":
				var ok bool
				if req, ok = ai.TriageRequest(run, args[0]); !ok {
					fmt.Fprintln(cmd.OutOrStdout(), "nothing to triage: no red, flaky or weak (mutation survived) execution")
					return nil
				}
			case "summary":
				req = ai.SummaryRequest(run, man)
			default:
				return exitErr{2, "--task triage|summary"}
			}
			c, err := aiClient(p, "", "")
			if err != nil {
				return err
			}
			prep, counts, err := c.Prepare(req)
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "=== system\n%s\n=== prompt\n%s\n=== redacted: %v\n", prep.System, prep.Prompt, counts)
			return err
		},
	}
	ctxCmd.Flags().StringVar(&task, "task", "triage", "triage | summary")

	triage := &cobra.Command{
		Use:   "triage <out/run-id>",
		Short: "Suggest a cause for each red / flaky / weak execution (advisory; written to ai/triage.{json,md})",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			p, err := g.project()
			if err != nil {
				return err
			}
			dir, man, err := loadBundle(args[0])
			if err != nil {
				return err
			}
			run, err := report.LoadRun(args[0])
			if err != nil {
				return err
			}
			req, ok := ai.TriageRequest(run, dir.Root)
			if !ok {
				fmt.Fprintln(out, "nothing to triage: no red, flaky or weak (mutation survived) execution")
				return nil
			}
			c, err := aiClient(p, dir.Path("ai", "requests"), response)
			if err != nil {
				return err
			}
			resp, err := c.Do(cmd.Context(), req)
			if err != nil {
				_ = reseal(dir) // the audit record of the attempt is part of the bundle
				return err
			}
			t, err := ai.ParseTriage(run, dir.Root, resp.Text)
			if err != nil {
				_ = reseal(dir)
				return err
			}
			t.Provider, t.Model = c.Provider.Name(), resp.Model
			if c.Response != "" {
				t.Provider, t.Model = "manual", "manual"
			}
			if _, err := dir.WriteJSON("ai/triage.json", t); err != nil {
				return err
			}
			if _, err := dir.WriteFile("ai/triage.md", []byte(t.Markdown())); err != nil {
				return err
			}
			if err := renderHTML(dir, run, man); err != nil {
				return err
			}
			if err := reseal(dir); err != nil {
				return err
			}
			fmt.Fprintf(out, "%s\n", ai.Disclaimer)
			for _, it := range t.Items {
				fmt.Fprintf(out, "  %-34s rule: %-12s AI: %-13s (%s) %s\n", it.Execution, it.RuleClass, it.Category, it.Confidence, it.Summary)
			}
			for _, pr := range t.Problems {
				fmt.Fprintf(out, "  check: %s\n", pr)
			}
			fmt.Fprintf(out, "triage: %s\n", dir.Path("ai", "triage.md"))
			return nil
		},
	}
	summary := &cobra.Command{
		Use:   "summary <out/run-id>",
		Short: "Write the sign-off summary of a run (facts by TestKit, text by the model) to ai/summary.md",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			p, err := g.project()
			if err != nil {
				return err
			}
			dir, man, err := loadBundle(args[0])
			if err != nil {
				return err
			}
			run, err := report.LoadRun(args[0])
			if err != nil {
				return err
			}
			c, err := aiClient(p, dir.Path("ai", "requests"), response)
			if err != nil {
				return err
			}
			resp, err := c.Do(cmd.Context(), ai.SummaryRequest(run, man))
			if err != nil {
				_ = reseal(dir)
				return err
			}
			prov, model := c.Provider.Name(), resp.Model
			if c.Response != "" {
				prov, model = "manual", "manual"
			}
			md, warn := ai.SummaryMarkdown(run, man, resp.Text, prov, model)
			if _, err := dir.WriteFile("ai/summary.md", []byte(md)); err != nil {
				return err
			}
			if err := renderHTML(dir, run, man); err != nil {
				return err
			}
			if err := reseal(dir); err != nil {
				return err
			}
			for _, w := range warn {
				fmt.Fprintf(out, "WARN %s\n", w)
			}
			fmt.Fprintf(out, "summary: %s\n", dir.Path("ai", "summary.md"))
			return nil
		},
	}
	for _, c := range []*cobra.Command{triage, summary} {
		c.Flags().StringVar(&response, "response", "", "use this file as the model's answer (obtained by hand from any model) instead of calling the provider")
	}
	cmd.AddCommand(ctxCmd, triage, summary, newAIDraftCmd(g))
	return cmd
}

func newAIDraftCmd(g *globals) *cobra.Command {
	var service, requirement, reqFile, reqID, id, outDir, response string
	var repairs int
	cmd := &cobra.Command{
		Use:   "draft --service <name> --requirement <text> | --requirement-file <file>",
		Short: "Draft a test case (status draft) from a requirement; it must pass lint and `testkit admit` before it counts",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			p, err := g.project()
			if err != nil {
				return err
			}
			services, err := config.LoadServices(p.Abs(p.ServicesDir))
			if err != nil {
				return err
			}
			svc, ok := services[service]
			if !ok {
				return exitErr{2, fmt.Sprintf("unknown service %q", service)}
			}
			source := "requirement (command line)"
			if reqFile != "" {
				raw, err := os.ReadFile(reqFile)
				if err != nil {
					return err
				}
				requirement, source = string(raw), "requirement file "+p.Rel(reqFile)
			}
			if strings.TrimSpace(requirement) == "" {
				return exitErr{2, "--requirement or --requirement-file is required"}
			}
			existing, err := loadCases(p, nil)
			if err != nil {
				return err
			}
			if id == "" {
				id = nextDraftID(existing)
			}
			for _, c := range existing {
				if c.ID == id {
					return exitErr{2, "case id " + id + " already exists (" + p.Rel(c.File) + ")"}
				}
			}
			descriptor, err := os.ReadFile(svc.File)
			if err != nil {
				return err
			}
			var vocab bytes.Buffer
			writeVocabulary(&vocab)
			in := ai.DraftInput{ID: id, Requirement: requirement, Service: service, Descriptor: string(descriptor),
				Examples: exampleCases(existing, service, 2), Vocabulary: vocab.String()}
			audit := p.Abs(filepath.Join(p.OutDir, "ai", "drafts", time.Now().UTC().Format("20060102-150405")+"-"+id))
			c, err := aiClient(p, audit, response)
			if err != nil {
				return err
			}
			if outDir == "" {
				outDir = filepath.Join(p.ScenariosDir, "drafts")
			}
			file := filepath.Join(p.Abs(outDir), id+".yaml")
			by := c.Provider.Name() + "/" + c.Provider.Model()
			if c.Response != "" {
				by = "manual"
			}
			reg := steps.Registry()
			for round := 0; ; round++ {
				resp, err := c.Do(cmd.Context(), ai.DraftRequest(in))
				if err != nil {
					return err
				}
				if resp.Model != "" && c.Response == "" {
					by = c.Provider.Name() + "/" + resp.Model
				}
				doc, err := ai.FinalizeDraft(resp.Text, id, reqID, by, []string{source}, time.Now())
				var problems []string
				if err != nil {
					problems = []string{err.Error()}
				} else {
					if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
						return err
					}
					if err := os.WriteFile(file, doc, 0o644); err != nil {
						return err
					}
					problems = lintProblems(file, services, reg)
				}
				if len(problems) == 0 {
					fmt.Fprintf(out, "draft: %s (status draft, lint OK)\naudit: %s\nnext: ./tk plan %s && ./tk admit --approve --by <name> %s\n",
						p.Rel(file), p.Rel(audit), p.Rel(file), p.Rel(file))
					return nil
				}
				if round >= repairs || c.Response != "" {
					for _, pr := range problems {
						fmt.Fprintf(out, "lint: %s\n", pr)
					}
					return exitErr{1, fmt.Sprintf("draft still has %d lint error(s) after %d repair round(s); kept for a person to fix: %s", len(problems), round, p.Rel(file))}
				}
				fmt.Fprintf(out, "draft round %d: %d lint error(s), asking for a fix\n", round+1, len(problems))
				in.Previous, in.LintErrors = string(doc), problems
			}
		},
	}
	cmd.Flags().StringVar(&service, "service", "", "service under test (testkit/services/<name>.yaml)")
	cmd.Flags().StringVar(&requirement, "requirement", "", "requirement text")
	cmd.Flags().StringVar(&reqFile, "requirement-file", "", "file holding the requirement")
	cmd.Flags().StringVar(&id, "id", "", "case id (default: next TC-DRAFT-NNN)")
	cmd.Flags().StringVar(&reqID, "req-id", "", "requirement id to trace the case to (default REQ-TBD)")
	cmd.Flags().StringVar(&outDir, "out", "", "directory for the draft (default: <scenarios_dir>/drafts)")
	cmd.Flags().StringVar(&response, "response", "", "use this file as the model's answer instead of calling the provider")
	cmd.Flags().IntVar(&repairs, "repairs", 2, "rounds in which lint errors are sent back to the model")
	return cmd
}

func lintProblems(file string, services map[string]*config.Service, reg *kit.Registry) []string {
	c, err := scenario.Load(file)
	if err != nil {
		return []string{err.Error()}
	}
	var out []string
	for _, i := range scenario.Lint(c, services, reg) {
		if i.Severity == "error" {
			out = append(out, fmt.Sprintf("line %d: %s", i.Line, i.Msg))
		}
	}
	return out
}

var draftID = regexp.MustCompile(`^TC-DRAFT-(\d+)$`)

func nextDraftID(cases []*scenario.Case) string {
	n := 0
	for _, c := range cases {
		if m := draftID.FindStringSubmatch(c.ID); m != nil {
			var v int
			fmt.Sscan(m[1], &v)
			n = max(n, v)
		}
	}
	return fmt.Sprintf("TC-DRAFT-%03d", n+1)
}

// exampleCases picks the shortest approved functional cases of a service as
// style examples (perf and chaos cases are not good templates).
func exampleCases(cases []*scenario.Case, service string, n int) []string {
	type ex struct {
		size int
		text string
	}
	var all []ex
	for _, c := range cases {
		if c.Service != service || c.Status != "approved" || c.Perf != nil || len(c.Chaos.Proxies) > 0 || len(c.Mutations) == 0 {
			continue
		}
		raw, err := os.ReadFile(c.File)
		if err != nil {
			continue
		}
		all = append(all, ex{len(raw), string(raw)})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].size < all[j].size })
	var out []string
	for i := 0; i < len(all) && i < n; i++ {
		out = append(out, all[i].text)
	}
	return out
}

// renderHTML re-renders report.html only: AI output never rewrites run.json,
// junit.xml or any other result file.
func renderHTML(dir *evidence.Dir, run *result.Run, man *evidence.Manifest) error {
	var b bytes.Buffer
	if err := report.HTML(&b, dir, run, man); err != nil {
		return err
	}
	_, err := dir.WriteFile("report.html", b.Bytes())
	return err
}

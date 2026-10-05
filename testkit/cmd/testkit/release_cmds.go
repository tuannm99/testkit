package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	qcfiles "github.com/tuannm99/testkit/testkit/adapters/qc/files"
	"github.com/tuannm99/testkit/testkit/adapters/qc/zephyr"
	"github.com/tuannm99/testkit/testkit/core/config"
	"github.com/tuannm99/testkit/testkit/core/evidence"
	"github.com/tuannm99/testkit/testkit/core/report"
	"github.com/tuannm99/testkit/testkit/core/result"
	"github.com/tuannm99/testkit/testkit/core/scenario"
)

var secretName = regexp.MustCompile(`(?i)(PASSWORD|PASSWD|SECRET|TOKEN|API_KEY|PRIVATE_KEY|CREDENTIAL)`)

// secretValues gathers every value declared as a secret: env files, the QC
// token, mock signing secrets and service env entries named like secrets.
func secretValues(p *config.Project, services map[string]*config.Service) map[string]string {
	out := map[string]string{}
	for k, v := range p.Env() {
		if secretName.MatchString(k) && !strings.HasPrefix(k, "TK_PORT_") {
			out[k] = v
		}
	}
	if p.QC != nil && p.QC.TokenEnv != "" {
		if v := os.Getenv(p.QC.TokenEnv); v != "" {
			out[p.QC.TokenEnv] = v
		}
	}
	for name, s := range services {
		for m, spec := range s.Mocks {
			if spec.Secret != "" {
				out[name+".mocks."+m+".secret"] = spec.Secret
			}
		}
		for k, v := range s.Env {
			if secretName.MatchString(k) && !strings.Contains(v, "{{") {
				out[name+".env."+k] = v
			}
		}
	}
	return out
}

// packBundle refuses bundles containing secrets, then zips the run.
func packBundle(w io.Writer, p *config.Project, root string) (string, error) {
	services, err := config.LoadServices(p.Abs(p.ServicesDir))
	if err != nil {
		return "", err
	}
	secrets := secretValues(p, services)
	for _, k := range config.SortedKeys(secrets) {
		if len(secrets[k]) < evidence.MinSecretLen {
			fmt.Fprintf(w, "WARN    %s is shorter than %d characters: it cannot be told apart from ordinary text, so the scan does not cover it; use a distinctive value\n", k, evidence.MinSecretLen)
		}
	}
	findings, err := evidence.ScanSecrets(root, secrets)
	if err != nil {
		return "", err
	}
	if len(findings) > 0 {
		for _, f := range findings {
			fmt.Fprintf(w, "SECRET  %s:%d  %s\n", f.Path, f.Line, f.What)
		}
		return "", exitErr{1, fmt.Sprintf("pack: %d secret(s) in the evidence; fix the collector/redaction, the bundle was not packed", len(findings))}
	}
	zipPath := strings.TrimRight(root, "/") + ".zip"
	sum, err := evidence.Pack(root, zipPath)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(w, "bundle: %s\nsha256: %s\n", zipPath, sum)
	return zipPath, nil
}

func newPackCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "pack <out/run-id>",
		Short: "Check the bundle (manifest, no secrets) and zip it to out/<run-id>.zip",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.project()
			if err != nil {
				return err
			}
			_, err = packBundle(cmd.OutOrStdout(), p, args[0])
			return err
		},
	}
}

// exportQC writes the QC import files of every configured tool into a
// bundle before it is sealed.
func exportQC(w io.Writer, p *config.Project, dir *evidence.Dir, run *result.Run, cases []*scenario.Case) error {
	for _, tool := range p.QCTools() {
		var files []string
		var err error
		switch tool {
		case "files":
			files, err = qcfiles.Export(dir, run, cases, p.Rel)
		case "zephyr-scale":
			files, err = zephyr.Export(dir, run, cases, p.QC)
		default:
			return fmt.Errorf("qc.tool %q is not supported (files, zephyr-scale)", tool)
		}
		if err != nil {
			return fmt.Errorf("qc %s: %w", tool, err)
		}
		fmt.Fprintf(w, "qc: %s → %d file(s) in %s\n", tool, len(files), dir.Path(qcfiles.Dir))
	}
	return nil
}

func newQCCmd(g *globals) *cobra.Command {
	qc := &cobra.Command{Use: "qc", Short: "QC handover: test cases and results as CSV/Markdown (files) or Zephyr Scale import files and upload"}
	qc.AddCommand(&cobra.Command{
		Use:   "cases [case.yaml|dir]...",
		Short: "Write the test case list for the QC tool(s) to out/qc/ (files: testcases.csv + testcases.md)",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.project()
			if err != nil {
				return err
			}
			cases, err := loadCases(p, args)
			if err != nil {
				return err
			}
			outDir := p.Abs(filepath.Join(p.OutDir, "qc"))
			if err := os.MkdirAll(outDir, 0o755); err != nil {
				return err
			}
			write := func(name string, data []byte) error {
				if err := os.WriteFile(filepath.Join(outDir, name), data, 0o644); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "qc: %d case(s) → %s\n", len(cases), filepath.Join(outDir, name))
				return nil
			}
			for _, tool := range p.QCTools() {
				switch tool {
				case "files":
					csvRaw, md, err := qcfiles.TestCases(cases, p.Rel)
					if err != nil {
						return err
					}
					if err := write("testcases.csv", csvRaw); err != nil {
						return err
					}
					if err := write("testcases.md", md); err != nil {
						return err
					}
				case "zephyr-scale":
					raw, err := zephyr.TestCasesCSV(cases, p.QC)
					if err != nil {
						return err
					}
					if err := write("zephyr-scale-testcases.csv", raw); err != nil {
						return err
					}
				default:
					return fmt.Errorf("qc.tool %q is not supported (files, zephyr-scale)", tool)
				}
			}
			return nil
		},
	}, &cobra.Command{
		Use:   "export <out/run-id>",
		Short: "(Re)write the QC files of an intact run (e.g. after adding qc_key or a tool) and re-seal it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.project()
			if err != nil {
				return err
			}
			root := args[0]
			if err := requireIntact(root); err != nil {
				return err
			}
			run, err := report.LoadRun(root)
			if err != nil {
				return err
			}
			cases, err := loadCases(p, nil)
			if err != nil {
				return err
			}
			dir := &evidence.Dir{Root: root}
			if err := exportQC(cmd.OutOrStdout(), p, dir, run, cases); err != nil {
				return err
			}
			return reseal(dir)
		},
	}, &cobra.Command{
		Use:   "push <out/run-id>",
		Short: "Upload the run's results to Zephyr Scale as a new test cycle (token from qc.token_env)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.project()
			if err != nil {
				return err
			}
			if err := requireIntact(args[0]); err != nil {
				return err
			}
			if !contains(p.QCTools(), "zephyr-scale") {
				return exitErr{2, "qc push uploads to Zephyr Scale only; with qc.tool: files, import " + args[0] + "/qc/results.csv or attach the zip"}
			}
			var tokenEnv string
			if p.QC != nil {
				tokenEnv = p.QC.TokenEnv
				if api := os.Getenv("TK_QC_API"); api != "" {
					p.QC.API = api // e.g. a Zephyr Scale Data Center URL, or a test double
				}
			}
			body, err := zephyr.Push(cmd.Context(), args[0], p.QC, os.Getenv(tokenEnv), nil)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "zephyr: %s\n", body)
			return nil
		},
	})
	return qc
}

// requireIntact refuses to touch a bundle that changed since it was sealed:
// re-sealing it would hide the change.
func requireIntact(root string) error {
	problems, err := evidence.Verify(root)
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		return exitErr{1, "bundle changed since it was sealed (" + strings.Join(problems, "; ") + "); not touching it"}
	}
	return nil
}

func reseal(dir *evidence.Dir) error {
	var man evidence.Manifest
	raw, err := os.ReadFile(dir.Path(evidence.ManifestFile))
	if err != nil {
		return err
	}
	if err := jsonUnmarshal(raw, &man); err != nil {
		return err
	}
	return dir.Seal(&man)
}

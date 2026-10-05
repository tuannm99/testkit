package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

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

// exportQC writes the QC tool's import files into a bundle before it is sealed.
func exportQC(w io.Writer, p *config.Project, dir *evidence.Dir, run *result.Run, cases []*scenario.Case) error {
	if p.QC == nil || p.QC.Tool == "" {
		return nil
	}
	if p.QC.Tool != "zephyr-scale" {
		return fmt.Errorf("qc.tool %q is not supported (zephyr-scale)", p.QC.Tool)
	}
	files, err := zephyr.Export(dir, run, cases, p.QC)
	if err == nil {
		fmt.Fprintf(w, "qc: Zephyr Scale import files in %s (%d)\n", dir.Path(zephyr.Dir), len(files))
	}
	return err
}

func newQCCmd(g *globals) *cobra.Command {
	qc := &cobra.Command{Use: "qc", Short: "Jira / Zephyr Scale: test case CSV, result import files, upload"}
	qc.AddCommand(&cobra.Command{
		Use:   "cases [case.yaml|dir]...",
		Short: "Write the Zephyr Scale test case import CSV (out/qc/testcases.csv)",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.project()
			if err != nil {
				return err
			}
			cases, err := loadCases(p, args)
			if err != nil {
				return err
			}
			raw, err := zephyr.TestCasesCSV(cases, p.QC)
			if err != nil {
				return err
			}
			out := p.Abs(filepath.Join(p.OutDir, "qc", "testcases.csv"))
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(out, raw, 0o644); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "qc: %d case(s) → %s (Zephyr Scale → Tests → Import → CSV)\n", len(cases), out)
			return nil
		},
	}, &cobra.Command{
		Use:   "export <out/run-id>",
		Short: "(Re)write the Zephyr Scale import files of a run and re-seal it",
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
			var tokenEnv string
			if p.QC != nil {
				tokenEnv = p.QC.TokenEnv
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

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tuannm99/testkit/testkit/adapters/collect/grafana"
	"github.com/tuannm99/testkit/testkit/adapters/collect/loki"
	"github.com/tuannm99/testkit/testkit/adapters/collect/prometheus"
	"github.com/tuannm99/testkit/testkit/core/config"
	"github.com/tuannm99/testkit/testkit/core/evidence"
)

// parseTime accepts RFC3339, "now", or a negative offset such as -10m.
func parseTime(s string, now time.Time) (time.Time, error) {
	switch {
	case s == "" || s == "now":
		return now, nil
	case strings.HasPrefix(s, "-"):
		d, err := time.ParseDuration(s)
		return now.Add(d), err
	}
	return time.Parse(time.RFC3339Nano, s)
}

func collector(p *config.Project) *grafana.Collector {
	ep := p.Endpoints(config.RunnerInNetwork())
	host := p.Endpoints(false)
	return &grafana.Collector{
		Grafana: grafana.New(ep.Grafana, host.Grafana, ep.GrafanaUser, ep.GrafanaPass),
		Prom:    prometheus.New(ep.Prometheus),
		Loki:    loki.New(ep.Loki),
		Scrape:  time.Second, // scrape interval of services under test
	}
}

func newAnnotateCmd(g *globals) *cobra.Command {
	var runID, text, at, end string
	var tags []string
	cmd := &cobra.Command{
		Use:   "annotate",
		Short: "Write a Grafana annotation for a run (step boundaries, fault injection)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := config.LoadProject(g.root)
			if err != nil {
				return err
			}
			now := time.Now()
			t0, err := parseTime(at, now)
			if err != nil {
				return err
			}
			a := grafana.Annotation{Time: t0, Tags: append([]string{"testkit", runID}, tags...), Text: text}
			if end != "" {
				if a.TimeEnd, err = parseTime(end, now); err != nil {
					return err
				}
			}
			id, err := collector(p).Grafana.Annotate(cmd.Context(), a)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "annotation %d: %s\n", id, text)
			return nil
		},
	}
	cmd.Flags().StringVar(&runID, "run-id", "", "run id (required)")
	cmd.Flags().StringVar(&text, "text", "", "annotation text")
	cmd.Flags().StringVar(&at, "time", "now", "time (RFC3339, now, -30s)")
	cmd.Flags().StringVar(&end, "end", "", "end time for a region annotation")
	cmd.Flags().StringSliceVar(&tags, "tags", nil, "extra tags")
	_ = cmd.MarkFlagRequired("run-id")
	return cmd
}

func newCollectCmd(g *globals) *cobra.Command {
	var runID, service, from, to, ns, outDir string
	var panels []string
	cmd := &cobra.Command{
		Use:   "collect",
		Short: "Export Grafana panels (PNG + raw query_range JSON/CSV + locked link) and Loki logs for a time window",
		Example: `  testkit collect --run-id r20261004-180501-ab12 --service order-worker --from -10m --to now
  testkit collect --run-id r1 --service order-worker --panels p95_latency,error_rate --from 2026-10-04T18:00:00Z --to 2026-10-04T18:05:00Z`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := config.LoadProject(g.root)
			if err != nil {
				return err
			}
			svcs, err := selectServices(p, []string{service})
			if err != nil {
				return err
			}
			svc := svcs[0]
			now := time.Now()
			t0, err := parseTime(from, now)
			if err != nil {
				return err
			}
			t1, err := parseTime(to, now)
			if err != nil {
				return err
			}
			if !t1.After(t0) {
				return fmt.Errorf("--to must be after --from")
			}
			if outDir == "" {
				outDir = filepath.Join(p.Abs(p.OutDir), runID, "collect")
			}
			if len(panels) == 0 {
				panels = config.SortedKeys(svc.Panels)
			}
			col := collector(p)
			vars := map[string]string{"run_id": runID, "ns": ns}
			dir := filepath.Join(outDir, "grafana")
			var captured []*grafana.Captured
			failed := 0
			for _, name := range panels {
				ps, ok := svc.Panels[name]
				if !ok {
					return fmt.Errorf("service %s declares no panel %q", svc.Name, name)
				}
				c, err := col.Capture(cmd.Context(), dir, grafana.Capture{Name: name, Dashboard: ps.Dashboard, PanelID: ps.PanelID,
					Query: ps.Query, From: t0, To: t1, Vars: vars})
				if err != nil {
					return fmt.Errorf("panel %s: %w", name, err)
				}
				if c.ImageError != "" {
					failed++
				}
				captured = append(captured, c)
				points := 0
				for _, q := range c.Queries {
					for _, s := range q.Stats {
						points += s.Points
					}
				}
				fmt.Fprintf(cmd.OutOrStdout(), "panel %-16s image=%-22s raw_queries=%d points=%d %s\n", name, orDash(c.Image), len(c.Queries), points, c.ImageError)
			}
			if _, err := writeJSON(filepath.Join(dir, "panels.json"), captured); err != nil {
				return err
			}
			lines, err := col.Loki.QueryRange(cmd.Context(), fmt.Sprintf(`{run_id=%q}`, runID), t0, t1, 20000)
			if err != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "WARN loki: %v\n", err)
			} else {
				if _, err := writeJSONL(filepath.Join(outDir, "logs", "loki.jsonl"), lines); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "logs: %d lines from Loki\n", len(lines))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "evidence written to %s\n", outDir)
			if failed > 0 {
				return exitErr{4, fmt.Sprintf("%d panel(s) could not be rendered (raw data was exported)", failed)}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&runID, "run-id", "", "run id (required)")
	cmd.Flags().StringVar(&service, "service", "", "service descriptor declaring the panels (required)")
	cmd.Flags().StringVar(&from, "from", "-15m", "window start (RFC3339, -10m)")
	cmd.Flags().StringVar(&to, "to", "now", "window end")
	cmd.Flags().StringVar(&ns, "ns", "All", "namespace filter")
	cmd.Flags().StringSliceVar(&panels, "panels", nil, "panel names (default: all declared)")
	cmd.Flags().StringVar(&outDir, "out", "", "output directory (default out/<run-id>/collect)")
	_ = cmd.MarkFlagRequired("run-id")
	_ = cmd.MarkFlagRequired("service")
	return cmd
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func writeJSON(path string, v any) (string, error) {
	d := &evidence.Dir{Root: filepath.Dir(path)}
	return d.WriteJSON(filepath.Base(path), v)
}

func writeJSONL(path string, lines []loki.Line) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	for _, l := range lines {
		b, _ := jsonMarshal(l)
		f.Write(append(b, '\n'))
	}
	return path, nil
}

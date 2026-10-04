package grafana

import (
	"context"
	"fmt"
	"time"

	"github.com/tuannm99/testkit/testkit/core/config"
	"github.com/tuannm99/testkit/testkit/core/result"
)

// Observer adapts the collector to the orchestrator's Observability port.
type Observer struct {
	C *Collector
}

func (o *Observer) Annotate(ctx context.Context, from, to time.Time, tags []string, text string) error {
	_, err := o.C.Grafana.Annotate(ctx, Annotation{Time: from, TimeEnd: to, Tags: tags, Text: text})
	return err
}

// Capture waits for one more scrape after `to` (so the window is complete),
// then renders the panel and exports its raw data.
func (o *Observer) Capture(ctx context.Context, dir, name string, p config.PanelSpec, from, to time.Time, vars map[string]string) result.Panel {
	out := result.Panel{Name: name, Window: fmt.Sprintf("%s → %s UTC", from.UTC().Format("15:04:05"), to.UTC().Format("15:04:05"))}
	c, err := o.C.Capture(ctx, dir, Capture{Name: name, Dashboard: p.Dashboard, PanelID: p.PanelID, Query: p.Query, From: from, To: to, Vars: vars})
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Title, out.Image, out.Link, out.Error = c.Title, c.Image, c.Link, c.ImageError
	for _, q := range c.Queries {
		if q.Raw != "" {
			out.Raw = append(out.Raw, q.Raw)
		}
		if q.CSV != "" {
			out.CSV = append(out.CSV, q.CSV)
		}
		if q.Error != "" && out.Error == "" {
			out.Error = q.Error
		}
		for _, s := range q.Stats {
			out.Stats = append(out.Stats, fmt.Sprintf("%s %s: min %.4g · max %.4g · cuối %.4g (%d điểm)", q.RefID, s.Series, s.Min, s.Max, s.Last, s.Points))
		}
	}
	return out
}

// WaitScraped waits until `targets` instances of the namespace are up in
// Prometheus with a sample taken at or after t (max 4 scrape intervals + 4s).
func (o *Observer) WaitScraped(ctx context.Context, ns string, targets int, t time.Time) error {
	deadline := time.Now().Add(4*o.C.Scrape + 4*time.Second)
	q := fmt.Sprintf(`count(timestamp(up{ns=%q} == 1) >= %d)`, ns, t.Unix())
	var last float64
	for time.Now().Before(deadline) {
		v, ok, err := o.C.Prom.Scalar(ctx, q, time.Now())
		if err == nil && ok && int(v) >= targets {
			return nil
		}
		last = v
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("prometheus scraped %d/%d instance(s) of %s after %s", int(last), targets, ns, t.Format(time.RFC3339))
}

// Available reports whether Grafana and Prometheus answer.
func (o *Observer) Available(ctx context.Context) error {
	if _, _, err := o.C.Prom.Scalar(ctx, "vector(1)", time.Now()); err != nil {
		return err
	}
	_, code, err := o.C.Grafana.do(ctx, "GET", "/api/health", nil)
	if err == nil && code != 200 {
		err = fmt.Errorf("grafana health HTTP %d", code)
	}
	return err
}

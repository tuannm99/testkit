// Package grafana writes annotations (step boundaries, fault injection) and
// captures panels as evidence: a PNG rendered for the exact time window of a
// step, the raw data behind every query of the panel (fetched from the
// datasource with the same expression), and a dashboard link locked to that
// window. Raw data is the primary evidence; the image is for a quick look.
package grafana

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tuannm99/testkit/testkit/adapters/collect/loki"
	"github.com/tuannm99/testkit/testkit/adapters/collect/prometheus"
)

type Client struct {
	Base      string // reachable from the runner
	PublicURL string // what a human opens (links in the report)
	User      string
	Password  string
	HTTP      *http.Client
}

func New(base, public, user, pass string) *Client {
	return &Client{Base: base, PublicURL: public, User: user, Password: pass, HTTP: &http.Client{Timeout: 90 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, path string, in any) ([]byte, int, error) {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return nil, 0, err
	}
	req.SetBasicAuth(c.User, c.Password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return raw, resp.StatusCode, err
}

// Annotation marks a point or a region on every TestKit dashboard. Tags always
// include "testkit" and the run id so dashboards filter per run.
type Annotation struct {
	Time    time.Time
	TimeEnd time.Time
	Tags    []string
	Text    string
}

// Annotate creates an organisation-wide annotation and returns its id.
func (c *Client) Annotate(ctx context.Context, a Annotation) (int64, error) {
	body := map[string]any{"time": a.Time.UnixMilli(), "tags": a.Tags, "text": a.Text}
	if !a.TimeEnd.IsZero() {
		body["timeEnd"] = a.TimeEnd.UnixMilli()
	}
	raw, code, err := c.do(ctx, http.MethodPost, "/api/annotations", body)
	if err != nil {
		return 0, err
	}
	if code != 200 {
		return 0, fmt.Errorf("grafana annotate: HTTP %d: %s", code, raw)
	}
	var r struct {
		ID int64 `json:"id"`
	}
	return r.ID, json.Unmarshal(raw, &r)
}

// Target is one query of a panel.
type Target struct {
	RefID        string `json:"refId"`
	Expr         string `json:"expr"`
	LegendFormat string `json:"legendFormat"`
}

// Panel is the subset of a dashboard panel used for evidence.
type Panel struct {
	ID         int    `json:"id"`
	Title      string `json:"title"`
	Datasource struct {
		Type string `json:"type"`
		UID  string `json:"uid"`
	} `json:"datasource"`
	Targets []Target `json:"targets"`
}

// Dashboard is the subset of a dashboard used for evidence.
type Dashboard struct {
	UID    string  `json:"uid"`
	Title  string  `json:"title"`
	Panels []Panel `json:"panels"`
	Slug   string  `json:"-"`
}

func (c *Client) Dashboard(ctx context.Context, uid string) (*Dashboard, error) {
	raw, code, err := c.do(ctx, http.MethodGet, "/api/dashboards/uid/"+uid, nil)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("grafana dashboard %s: HTTP %d: %.200s", uid, code, raw)
	}
	var r struct {
		Dashboard Dashboard `json:"dashboard"`
		Meta      struct {
			Slug string `json:"slug"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	r.Dashboard.Slug = r.Meta.Slug
	return &r.Dashboard, nil
}

func (d *Dashboard) Panel(id int) (*Panel, error) {
	for i := range d.Panels {
		if d.Panels[i].ID == id {
			return &d.Panels[i], nil
		}
	}
	return nil, fmt.Errorf("dashboard %s has no panel %d", d.UID, id)
}

func varsQuery(vars map[string]string) string {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString("&var-" + url.QueryEscape(k) + "=" + url.QueryEscape(vars[k]))
	}
	return b.String()
}

// DashboardURL is a link to the dashboard locked to [from, to].
func (c *Client) DashboardURL(d *Dashboard, panelID int, from, to time.Time, vars map[string]string) string {
	u := fmt.Sprintf("%s/d/%s/%s?orgId=1&from=%d&to=%d&timezone=utc%s", c.PublicURL, d.UID, d.Slug,
		from.UnixMilli(), to.UnixMilli(), varsQuery(vars))
	if panelID > 0 {
		u += fmt.Sprintf("&viewPanel=%d", panelID)
	}
	return u
}

// RenderPanel renders one panel to PNG through the image renderer.
func (c *Client) RenderPanel(ctx context.Context, d *Dashboard, panelID int, from, to time.Time, vars map[string]string, w, h int) ([]byte, error) {
	path := fmt.Sprintf("/render/d-solo/%s/%s?orgId=1&panelId=%d&from=%d&to=%d&width=%d&height=%d&tz=UTC&timeout=60%s",
		d.UID, d.Slug, panelID, from.UnixMilli(), to.UnixMilli(), w, h, varsQuery(vars))
	raw, code, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if code != 200 || !bytes.HasPrefix(raw, []byte("\x89PNG")) {
		return nil, fmt.Errorf("grafana render panel %d: HTTP %d: %.200s", panelID, code, raw)
	}
	return raw, nil
}

// Capture describes one evidence panel to capture.
type Capture struct {
	Name      string            // evidence name (file stem), e.g. p95_latency
	Dashboard string            // dashboard uid
	PanelID   int               // panel id
	Query     string            // optional PromQL override for the raw data
	From, To  time.Time         // exact window of the step/case
	Vars      map[string]string // dashboard variables (run_id, ns, ...)
}

// Captured is what Capture produced (paths relative to the evidence dir).
type Captured struct {
	Name        string            `json:"name"`
	Title       string            `json:"title"`
	Dashboard   string            `json:"dashboard"`
	PanelID     int               `json:"panel_id"`
	From        time.Time         `json:"from"`
	To          time.Time         `json:"to"`
	Link        string            `json:"link"`
	Image       string            `json:"image,omitempty"`
	ImageError  string            `json:"image_error,omitempty"`
	Queries     []CapturedQuery   `json:"queries"`
	Vars        map[string]string `json:"vars"`
	Annotations int               `json:"annotations"`
}

type CapturedQuery struct {
	RefID      string             `json:"ref_id"`
	Datasource string             `json:"datasource"`
	Expr       string             `json:"expr"`
	Raw        string             `json:"raw"`
	CSV        string             `json:"csv,omitempty"`
	Stats      []prometheus.Stats `json:"stats,omitempty"`
	Error      string             `json:"error,omitempty"`
}

// Collector captures panels using Grafana for images and the datasources
// directly for raw data.
type Collector struct {
	Grafana *Client
	Prom    *prometheus.Client
	Loki    *loki.Client
	Scrape  time.Duration // Prometheus scrape interval (for $__rate_interval)
}

var varRe = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?`)

// Interpolate replaces dashboard variables like Grafana does for the raw query.
func Interpolate(expr string, vars map[string]string, step, scrape time.Duration) string {
	rate := max(step+scrape, 4*scrape)
	return varRe.ReplaceAllStringFunc(expr, func(m string) string {
		name := varRe.FindStringSubmatch(m)[1]
		switch name {
		case "__rate_interval":
			return promDuration(rate)
		case "__interval":
			return promDuration(step)
		}
		if v, ok := vars[name]; ok {
			if v == "All" || v == "$__all" {
				return ".*"
			}
			return v
		}
		return m
	})
}

func promDuration(d time.Duration) string {
	if d%time.Second == 0 {
		return strconv.Itoa(int(d/time.Second)) + "s"
	}
	return strconv.Itoa(int(d/time.Millisecond)) + "ms"
}

// Step chooses a query resolution: the scrape interval, or coarser so that a
// series has at most ~600 points.
func Step(from, to time.Time, scrape time.Duration) time.Duration {
	s := scrape
	for to.Sub(from)/s > 600 {
		s *= 2
	}
	return s
}

// Capture renders the panel and exports its raw data into dir.
func (c *Collector) Capture(ctx context.Context, dir string, cp Capture) (*Captured, error) {
	d, err := c.Grafana.Dashboard(ctx, cp.Dashboard)
	if err != nil {
		return nil, err
	}
	p, err := d.Panel(cp.PanelID)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	out := &Captured{Name: cp.Name, Title: p.Title, Dashboard: d.UID, PanelID: p.ID, From: cp.From.UTC(), To: cp.To.UTC(),
		Vars: cp.Vars, Link: c.Grafana.DashboardURL(d, p.ID, cp.From, cp.To, cp.Vars)}

	png, err := c.Grafana.RenderPanel(ctx, d, p.ID, cp.From, cp.To, cp.Vars, 1000, 420)
	if err != nil {
		out.ImageError = err.Error()
	} else {
		out.Image = cp.Name + ".png"
		if err := os.WriteFile(filepath.Join(dir, out.Image), png, 0o644); err != nil {
			return nil, err
		}
	}
	step := Step(cp.From, cp.To, c.Scrape)
	targets := p.Targets
	if cp.Query != "" {
		targets = []Target{{RefID: "A", Expr: cp.Query}}
	}
	for _, t := range targets {
		q := CapturedQuery{RefID: t.RefID, Datasource: p.Datasource.Type, Expr: Interpolate(t.Expr, cp.Vars, step, c.Scrape)}
		stem := fmt.Sprintf("%s.%s", cp.Name, strings.ToLower(t.RefID))
		switch p.Datasource.Type {
		case "loki":
			// Loki aligns metric samples down to a step boundary; start on the
			// next boundary so every sample lies inside the step window.
			lokiFrom := cp.From.Truncate(step)
			if lokiFrom.Before(cp.From) {
				lokiFrom = lokiFrom.Add(step)
			}
			raw, typ, err := c.Loki.RangeRaw(ctx, q.Expr, lokiFrom, cp.To, step, 5000)
			if len(raw) > 0 {
				q.Raw = stem + ".loki.json"
				if werr := os.WriteFile(filepath.Join(dir, q.Raw), raw, 0o644); werr != nil {
					return nil, werr
				}
			}
			if err != nil {
				q.Error = err.Error()
				break
			}
			if typ == "matrix" { // metric query: same shape as a Prometheus matrix
				var res prometheus.Result
				var m struct {
					Data struct {
						Result []prometheus.Series `json:"result"`
					} `json:"data"`
				}
				if json.Unmarshal(raw, &m) == nil {
					res.Series = m.Data.Result
					var buf bytes.Buffer
					_ = res.WriteCSV(&buf)
					q.CSV = stem + ".csv"
					if err := os.WriteFile(filepath.Join(dir, q.CSV), buf.Bytes(), 0o644); err != nil {
						return nil, err
					}
					q.Stats = res.Stats()
				}
			}
		default:
			res, err := c.Prom.QueryRange(ctx, q.Expr, cp.From, cp.To, step)
			if res != nil && len(res.Raw) > 0 {
				q.Raw = stem + ".prom.json"
				if werr := os.WriteFile(filepath.Join(dir, q.Raw), res.Raw, 0o644); werr != nil {
					return nil, werr
				}
			}
			if err != nil {
				q.Error = err.Error()
				break
			}
			var buf bytes.Buffer
			_ = res.WriteCSV(&buf)
			q.CSV = stem + ".csv"
			if err := os.WriteFile(filepath.Join(dir, q.CSV), buf.Bytes(), 0o644); err != nil {
				return nil, err
			}
			q.Stats = res.Stats()
		}
		out.Queries = append(out.Queries, q)
	}
	return out, nil
}

// CountAnnotations returns how many annotations carry all tags in [from, to].
func (c *Client) CountAnnotations(ctx context.Context, from, to time.Time, tags ...string) (int, error) {
	q := url.Values{}
	q.Set("from", strconv.FormatInt(from.UnixMilli(), 10))
	q.Set("to", strconv.FormatInt(to.UnixMilli(), 10))
	for _, t := range tags {
		q.Add("tags", t)
	}
	q.Set("limit", "1000")
	raw, code, err := c.do(ctx, http.MethodGet, "/api/annotations?"+q.Encode(), nil)
	if err != nil {
		return 0, err
	}
	if code != 200 {
		return 0, fmt.Errorf("grafana annotations: HTTP %d", code)
	}
	var list []json.RawMessage
	return len(list), json.Unmarshal(raw, &list)
}

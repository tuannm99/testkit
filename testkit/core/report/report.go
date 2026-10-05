// Package report renders a finished run for humans and tools:
//
//	report.html       offline, one file, written for a QC reader
//	junit.xml         for CI and test-management imports
//	traceability.csv  REQ -> TC -> result -> evidence paths
package report

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tuannm99/testkit/testkit/core/ai"
	"github.com/tuannm99/testkit/testkit/core/assert"
	"github.com/tuannm99/testkit/testkit/core/evidence"
	"github.com/tuannm99/testkit/testkit/core/result"
)

// WriteAll writes report.html, junit.xml, traceability.csv and run.json.
func WriteAll(dir *evidence.Dir, run *result.Run, man *evidence.Manifest) error {
	if _, err := dir.WriteJSON("run.json", run); err != nil {
		return err
	}
	if err := writeFile(dir.Path("junit.xml"), func(w io.Writer) error { return JUnit(w, run) }); err != nil {
		return err
	}
	if err := writeFile(dir.Path("traceability.csv"), func(w io.Writer) error { return Traceability(w, run) }); err != nil {
		return err
	}
	return writeFile(dir.Path("report.html"), func(w io.Writer) error { return HTML(w, dir, run, man) })
}

func writeFile(p string, fn func(io.Writer) error) error {
	f, err := os.Create(p)
	if err != nil {
		return err
	}
	if err := fn(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// LoadRun reads run.json of a run directory.
func LoadRun(root string) (*result.Run, error) {
	raw, err := os.ReadFile(filepath.Join(root, "run.json"))
	if err != nil {
		return nil, err
	}
	var r result.Run
	return &r, json.Unmarshal(raw, &r)
}

// ---------------------------------------------------------------- JUnit

type junitSuites struct {
	XMLName  xml.Name     `xml:"testsuites"`
	Name     string       `xml:"name,attr"`
	Tests    int          `xml:"tests,attr"`
	Failures int          `xml:"failures,attr"`
	Errors   int          `xml:"errors,attr"`
	Skipped  int          `xml:"skipped,attr"`
	Time     float64      `xml:"time,attr"`
	Suites   []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name      string      `xml:"name,attr"`
	Tests     int         `xml:"tests,attr"`
	Failures  int         `xml:"failures,attr"`
	Errors    int         `xml:"errors,attr"`
	Skipped   int         `xml:"skipped,attr"`
	Time      float64     `xml:"time,attr"`
	Timestamp string      `xml:"timestamp,attr"`
	Props     []junitProp `xml:"properties>property"`
	Cases     []junitCase `xml:"testcase"`
}

type junitProp struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type junitCase struct {
	Name      string       `xml:"name,attr"`
	Classname string       `xml:"classname,attr"`
	Time      float64      `xml:"time,attr"`
	Props     []junitProp  `xml:"properties>property,omitempty"`
	Failure   *junitDetail `xml:"failure,omitempty"`
	Error     *junitDetail `xml:"error,omitempty"`
	Skipped   *junitDetail `xml:"skipped,omitempty"`
	SystemOut string       `xml:"system-out,omitempty"`
}

type junitDetail struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr,omitempty"`
	Body    string `xml:",chardata"`
}

// JUnit writes one testsuite per service; mutation runs are not test cases.
func JUnit(w io.Writer, run *result.Run) error {
	bySvc := map[string]*junitSuite{}
	var order []string
	out := junitSuites{Name: "testkit " + run.RunID}
	for _, ex := range run.Executions {
		if ex.Mutation != "" {
			continue
		}
		s, ok := bySvc[ex.Service]
		if !ok {
			s = &junitSuite{Name: ex.Service, Timestamp: run.StartedAt.Format(time.RFC3339),
				Props: []junitProp{{"run_id", run.RunID}}}
			bySvc[ex.Service] = s
			order = append(order, ex.Service)
		}
		d := ex.FinishedAt.Sub(ex.StartedAt).Seconds()
		jc := junitCase{Name: ex.ID + " " + ex.Title, Classname: ex.Service + "." + ex.CaseID, Time: d,
			Props: []junitProp{{"requirement", strings.Join(ex.Requirement, ",")}, {"risk", ex.Risk},
				{"trigger", ex.Trigger}, {"evidence", ex.Dir + "/case.json"}}}
		var body strings.Builder
		for _, a := range ex.Assertions {
			fmt.Fprintf(&body, "%s %s: %s %s %s -> actual %s (%s)\n", a.ID, strings.ToUpper(a.Result), a.Check,
				a.Operator, assert.Show(a.Expected), assert.Show(a.Actual), a.Why)
		}
		switch ex.Result {
		case result.Fail:
			jc.Failure = &junitDetail{Message: ex.Reason, Type: ex.Class, Body: body.String() + strings.Join(ex.LogTail, "\n")}
			s.Failures++
			out.Failures++
		case result.Error:
			jc.Error = &junitDetail{Message: ex.Reason, Type: ex.Class, Body: body.String() + strings.Join(ex.LogTail, "\n")}
			s.Errors++
			out.Errors++
		case result.Skipped:
			jc.Skipped = &junitDetail{Message: ex.Reason}
			s.Skipped++
			out.Skipped++
		default:
			jc.SystemOut = body.String()
		}
		s.Cases = append(s.Cases, jc)
		s.Tests++
		s.Time += d
		out.Tests++
		out.Time += d
	}
	for _, k := range order {
		out.Suites = append(out.Suites, *bySvc[k])
	}
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	return enc.Encode(out)
}

// ---------------------------------------------------------- traceability

// Traceability writes REQ -> TC -> result -> evidence.
func Traceability(w io.Writer, run *result.Run) error {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"requirement", "case_id", "execution", "title", "risk", "status", "trigger", "result",
		"classification", "mutations_killed", "evidence_dir", "evidence_files"})
	type row struct {
		req string
		ex  *result.Execution
	}
	var rows []row
	for _, ex := range run.Executions {
		if ex.Mutation != "" {
			continue
		}
		for _, r := range ex.Requirement {
			rows = append(rows, row{r, ex})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].req != rows[j].req {
			return rows[i].req < rows[j].req
		}
		return rows[i].ex.ID < rows[j].ex.ID
	})
	for _, r := range rows {
		ex := r.ex
		files := []string{ex.Dir + "/case.json", ex.Dir + "/timeline.json"}
		for _, a := range ex.Assertions {
			files = append(files, a.Evidence...)
		}
		files = dedupe(files)
		killed := ""
		if len(ex.Mutations) > 0 {
			k := 0
			for _, m := range ex.Mutations {
				if m.Killed {
					k++
				}
			}
			killed = fmt.Sprintf("%d/%d", k, len(ex.Mutations))
		}
		_ = cw.Write([]string{r.req, ex.CaseID, ex.ID, ex.Title, ex.Risk, ex.Status, ex.Trigger, ex.Result, ex.Class,
			killed, ex.Dir, strings.Join(files, ";")})
	}
	cw.Flush()
	return cw.Error()
}

func dedupe(l []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range l {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ------------------------------------------------------------------ HTML

type htmlData struct {
	Run       *result.Run
	Manifest  *evidence.Manifest
	Counts    map[string]int
	Execs     []*execView
	Matrix    []matrixRow
	Generated string
	AI        *ai.Triage // advisory, from ai/triage.json when present
	Summary   bool       // ai/summary.md present
}

type execView struct {
	*result.Execution
	Anchor    string
	Duration  string
	InputJSON string
	VarsJSON  string
	Timeline  []result.Event
	Outputs   []outputView
}

type outputView struct {
	Title   string
	Path    string
	Kind    string
	Preview string
}

type matrixRow struct {
	Req   string
	Execs []*result.Execution
}

// HTML renders the report.
func HTML(w io.Writer, dir *evidence.Dir, run *result.Run, man *evidence.Manifest) error {
	d := htmlData{Run: run, Manifest: man, Counts: run.Counts(), Generated: time.Now().UTC().Format(time.RFC3339)}
	if raw, err := os.ReadFile(dir.Path("ai", "triage.json")); err == nil {
		var t ai.Triage
		if json.Unmarshal(raw, &t) == nil {
			d.AI = &t
		}
	}
	if _, err := os.Stat(dir.Path("ai", "summary.md")); err == nil {
		d.Summary = true
	}
	reqs := map[string][]*result.Execution{}
	for _, ex := range run.Executions {
		if ex.Mutation != "" {
			continue
		}
		v := &execView{Execution: ex, Anchor: anchor(ex.ID), Duration: ex.FinishedAt.Sub(ex.StartedAt).Round(10 * time.Millisecond).String()}
		v.InputJSON = pretty(ex.Input)
		if len(ex.Vars) > 0 {
			v.VarsJSON = pretty(ex.Vars)
		}
		if raw, err := os.ReadFile(dir.Path(ex.Dir, "timeline.json")); err == nil {
			_ = json.Unmarshal(raw, &v.Timeline)
		}
		for _, a := range ex.Artifacts {
			ov := outputView{Title: a.Title, Path: a.Path, Kind: a.Kind}
			if raw, err := os.ReadFile(dir.Path(a.Path)); err == nil && a.Kind != "image" && !binary(raw) {
				ov.Preview = preview(raw, a.Kind)
			}
			v.Outputs = append(v.Outputs, ov)
		}
		d.Execs = append(d.Execs, v)
		for _, r := range ex.Requirement {
			reqs[r] = append(reqs[r], ex)
		}
	}
	keys := make([]string, 0, len(reqs))
	for k := range reqs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		d.Matrix = append(d.Matrix, matrixRow{Req: k, Execs: reqs[k]})
	}
	t, err := template.New("report").Funcs(funcs).Parse(reportTemplate)
	if err != nil {
		return err
	}
	return t.Execute(w, d)
}

func anchor(id string) string {
	r := strings.NewReplacer("[", "-", "]", "", "{", "-", "}", "", " ", "-")
	return strings.ToLower(r.Replace(id))
}

func pretty(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// preview keeps the first lines of a text artifact (logs) or a compact JSON.
func preview(raw []byte, kind string) string {
	const max = 3000
	s := string(raw)
	if kind == "log" {
		lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
		if len(lines) > 40 {
			lines = append([]string{fmt.Sprintf("… %d earlier lines in the file …", len(lines)-40)}, lines[len(lines)-40:]...)
		}
		s = strings.Join(lines, "\n")
	}
	if len(s) > max {
		s = s[:max] + "\n… (truncated, open the file for the full content)"
	}
	return s
}

var funcs = template.FuncMap{
	"upper": strings.ToUpper,
	"show":  assert.Show,
	"op": func(op string) string {
		m := map[string]string{"eq": "=", "ne": "≠", "gt": ">", "gte": "≥", "lt": "<", "lte": "≤", "in": "∈", "not_in": "∉",
			"contains": "chứa", "not_contains": "không chứa", "matches": "khớp", "exists": "tồn tại", "not_exists": "không tồn tại",
			"between": "trong khoảng", "approx": "≈", "empty": "rỗng", "not_empty": "không rỗng", "len_eq": "số phần tử ="}
		if v, ok := m[op]; ok {
			return v
		}
		return op
	},
	"needsExp": assert.NeedsExpected,
	"compact": func(v any) string {
		if v == nil {
			return ""
		}
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(b)
	},
	"join": strings.Join,
	"t": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format("15:04:05.000")
	},
	"date": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 UTC") },
	"dur": func(a, b time.Time) string {
		if a.IsZero() || b.IsZero() {
			return ""
		}
		return b.Sub(a).Round(time.Millisecond).String()
	},
	"since": func(start, t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return "+" + t.Sub(start).Round(time.Millisecond).String()
	},
	"cls": func(r string) string {
		switch r {
		case "pass", "ok", "GO":
			return "ok"
		case "fail", "NO-GO":
			return "bad"
		case "error":
			return "err"
		}
		return "muted"
	},
	"classVN": func(c string) string {
		switch c {
		case result.ClassProduct:
			return "nghi lỗi thật của sản phẩm"
		case result.ClassEnvironment:
			return "lỗi môi trường / hạ tầng test"
		case result.ClassFlaky:
			return "flaky (cách ly, không tính là pass)"
		case result.ClassCapability:
			return "bỏ qua: máy chạy thiếu quyền/khả năng được khai báo (không tính là fail)"
		case result.ClassTest:
			return "lỗi kịch bản test"
		}
		return c
	},
	"base": filepath.Base,
	"pct":  func(v float64) float64 { return v * 100 },
	"num":  fmtNum,
	"admitVN": func(s string) string {
		return map[string]string{"admitted": "đạt — chờ người duyệt", "rejected": "không đạt", "incomplete": "chưa đủ điều kiện đánh giá"}[s]
	},
	"ruleVN": func(s string) string {
		return map[string]string{"has-mutations": "có mutation", "green-and-stable": "xanh ổn định",
			"mutations-killed": "đỏ khi có lỗi", "baseline-compared": "so với baseline"}[s]
	},
	"worse": func(v float64) string {
		switch {
		case v > 0:
			return fmt.Sprintf("tệ đi %.1f%%", v)
		case v < 0:
			return fmt.Sprintf("tốt lên %.1f%%", -v)
		}
		return "không đổi"
	},
	"sloFor": func(t map[string]float64, metric string) string {
		names := map[string]string{"p50_ms": "p50_ms", "p95_ms": "p95_ms", "p99_ms": "p99_ms", "error_rate": "error_rate", "throughput": "min_throughput", "late": "max_dropped"}
		k, ok := names[metric]
		if !ok {
			return ""
		}
		v, ok := t[k]
		if !ok {
			return ""
		}
		if metric == "throughput" {
			return fmt.Sprintf("≥ %g", v)
		}
		return fmt.Sprintf("≤ %g", v)
	},
	"anchor": anchor,
	"isLast": func(i, n int) bool { return i == n-1 },
}

// fmtNum prints a metric without exponent notation: integers as is, large
// values rounded to units, small ones with up to 3 decimals.
func fmtNum(v float64) string {
	a := math.Abs(v)
	switch {
	case v == math.Trunc(v) || a >= 100:
		return strconv.FormatFloat(v, 'f', 0, 64)
	case a >= 1:
		return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(v, 'f', 2, 64), "0"), ".")
	}
	return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(v, 'f', 4, 64), "0"), ".")
}

// binary reports whether a file is not text (no quick view in the report).
func binary(b []byte) bool { return bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 }

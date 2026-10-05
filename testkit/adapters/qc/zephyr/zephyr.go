// Package zephyr exports a run to Zephyr Scale (Jira test management with
// test cycles): result files to import into a new test cycle, a CSV to create
// the test cases, and an optional upload through the Zephyr Scale API.
//
// Results use Zephyr Scale's "custom format" (version 1): one execution per
// test case, mapped by key (qc_key in the case file) or by name. The verdict
// is TestKit's — Zephyr only stores it.
package zephyr

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/tuannm99/testkit/testkit/core/config"
	"github.com/tuannm99/testkit/testkit/core/evidence"
	"github.com/tuannm99/testkit/testkit/core/result"
	"github.com/tuannm99/testkit/testkit/core/scenario"
)

// Dir is where the export lives inside the evidence bundle.
const Dir = "qc/zephyr-scale"

// Execution is one entry of the custom format.
type Execution struct {
	Source   string    `json:"source"`
	Result   string    `json:"result"` // Passed | Failed
	TestCase *TestCase `json:"testCase,omitempty"`
}

// TestCase maps an execution to a Zephyr test case by key or by name.
type TestCase struct {
	Key  string `json:"key,omitempty"`
	Name string `json:"name,omitempty"`
}

// Results is the custom format file.
type Results struct {
	Version    int         `json:"version"`
	Executions []Execution `json:"executions"`
}

// Cycle is the testCycle part of the upload.
type Cycle struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// TestCaseName is how cases without qc_key are named in Zephyr (and in the CSV).
func TestCaseName(c *scenario.Case) string { return c.ID + " — " + c.Title }

// Build maps a run to Zephyr executions. Every trigger of a case must pass
// for the test case to pass; skipped cases are not exported (they were not
// executed) and are listed in the cycle description instead.
func Build(run *result.Run, cases []*scenario.Case, cfg *config.QC) (Results, Cycle, error) {
	byID := map[string]*scenario.Case{}
	for _, c := range cases {
		byID[c.ID] = c
	}
	type agg struct {
		results []string
		lines   []string
	}
	per := map[string]*agg{}
	var order []string
	for _, ex := range run.Executions {
		if ex.Mutation != "" {
			continue
		}
		a, ok := per[ex.CaseID]
		if !ok {
			a = &agg{}
			per[ex.CaseID] = a
			order = append(order, ex.CaseID)
		}
		a.results = append(a.results, ex.Result)
		line := fmt.Sprintf("%s: %s", ex.ID, strings.ToUpper(ex.Result))
		if ex.Class != "" {
			line += " (" + ex.Class + ")"
		}
		a.lines = append(a.lines, line+" — "+ex.Dir+"/case.json")
	}
	sort.Strings(order)
	res := Results{Version: 1}
	var notRun, lines []string
	for _, id := range order {
		a := per[id]
		verdict := "Passed"
		skippedAll := true
		for _, r := range a.results {
			if r != result.Skipped {
				skippedAll = false
			}
			if r != result.Pass && r != result.Skipped {
				verdict = "Failed"
			}
		}
		lines = append(lines, a.lines...)
		if skippedAll {
			notRun = append(notRun, id)
			continue
		}
		e := Execution{Source: id, Result: verdict}
		if c := byID[id]; c != nil {
			e.Source = id + " (" + path.Base(c.File) + ")"
			if c.QCKey != "" {
				e.TestCase = &TestCase{Key: c.QCKey}
			} else {
				e.TestCase = &TestCase{Name: TestCaseName(c)}
			}
		}
		res.Executions = append(res.Executions, e)
	}
	name, err := cycleName(cfg, run)
	if err != nil {
		return res, Cycle{}, err
	}
	desc := []string{
		fmt.Sprintf("TestKit run %s%s, %s → %s.", run.RunID, suffix(" · suite ", run.Suite), run.StartedAt.Format(time.RFC3339), run.FinishedAt.Format(time.RFC3339)),
		"Kết quả do TestKit quyết định bằng luật cứng; bằng chứng: gói " + run.RunID + ".zip (report.html, manifest.json).",
	}
	if run.Gate != nil {
		desc = append(desc, "Cổng release: "+run.Gate.Decision)
	}
	if len(notRun) > 0 {
		desc = append(desc, "Không chạy (bỏ qua): "+strings.Join(notRun, ", "))
	}
	desc = append(desc, lines...)
	return res, Cycle{Name: name, Description: strings.Join(desc, "\n")}, nil
}

func suffix(p, s string) string {
	if s == "" {
		return ""
	}
	return p + s
}

func cycleName(cfg *config.QC, run *result.Run) (string, error) {
	tpl := "{{ .suite }} {{ .run_id }}"
	if cfg != nil && cfg.CycleName != "" {
		tpl = cfg.CycleName
	}
	t, err := template.New("cycle").Option("missingkey=error").Parse(tpl)
	if err != nil {
		return "", fmt.Errorf("qc.cycle_name: %w", err)
	}
	suite := run.Suite
	if suite == "" {
		suite = "TestKit"
	}
	var b bytes.Buffer
	err = t.Execute(&b, map[string]string{"suite": suite, "run_id": run.RunID, "date": run.StartedAt.Format("2006-01-02")})
	return strings.TrimSpace(b.String()), err
}

// Export writes the import files into the bundle (before it is sealed).
func Export(dir *evidence.Dir, run *result.Run, cases []*scenario.Case, cfg *config.QC) ([]string, error) {
	res, cyc, err := Build(run, cases, cfg)
	if err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return nil, err
	}
	var files []string
	write := func(name string, data []byte) error {
		p, err := dir.WriteFile(path.Join(Dir, name), data)
		files = append(files, p)
		return err
	}
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "executions.json", Method: zip.Deflate, Modified: run.FinishedAt})
	if err == nil {
		_, err = w.Write(raw)
	}
	if err == nil {
		err = zw.Close()
	}
	if err != nil {
		return nil, err
	}
	cycRaw, _ := json.MarshalIndent(cyc, "", "  ")
	csvRaw, err := TestCasesCSV(cases, cfg)
	if err != nil {
		return nil, err
	}
	for _, f := range []struct {
		name string
		data []byte
	}{
		{"executions.json", raw}, {"executions.zip", zb.Bytes()}, {"test-cycle.json", cycRaw},
		{"testcases.csv", csvRaw}, {"HUONG-DAN-IMPORT.md", []byte(guide(cfg, run))},
	} {
		if err := write(f.name, f.data); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// TestCasesCSV describes the cases for Zephyr Scale's CSV test case import:
// one row per step; the first row of a case carries its fields.
func TestCasesCSV(cases []*scenario.Case, cfg *config.QC) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("\xef\xbb\xbf") // Excel and Zephyr's importer read UTF-8 with BOM correctly
	w := csv.NewWriter(&b)
	_ = w.Write([]string{"Key", "Name", "Objective", "Precondition", "Priority", "Status", "Labels", "Folder", "Owner",
		"Coverage (Issues)", "Step", "Test Data", "Expected Result"})
	folder := "/TestKit"
	if cfg != nil && cfg.Folder != "" {
		folder = cfg.Folder
	}
	sorted := append([]*scenario.Case(nil), cases...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for _, c := range sorted {
		type step struct{ action, data, expected string }
		var steps []step
		given, err := c.GivenEntries()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.ID, err)
		}
		for _, g := range given {
			steps = append(steps, step{"Chuẩn bị: " + g.Key, compact(g.Value), ""})
		}
		for _, s := range c.Steps {
			action := s.Step
			if s.Name != "" {
				action += " — " + s.Name
			}
			steps = append(steps, step{action, compact(s.With), ""})
		}
		for _, e := range c.Expect {
			exp := fmt.Sprintf("%s %s", e.Op, compact(e.Expected))
			if e.Why != "" {
				exp += " — " + e.Why
			}
			steps = append(steps, step{"Kiểm tra " + e.ID + ": " + e.Check, "", exp})
		}
		if len(steps) == 0 {
			steps = append(steps, step{"Chạy kịch bản " + c.ID, "", "pass"})
		}
		priority := map[string]string{"P0": "High", "P1": "High", "P2": "Normal", "P3": "Low"}[c.Risk]
		if priority == "" {
			priority = "Normal"
		}
		status := map[string]string{"approved": "Approved", "draft": "Draft"}[c.Status]
		labels := append([]string{"testkit", c.Service}, c.Tags...)
		for i, s := range steps {
			row := make([]string, 13)
			if i == 0 {
				name := TestCaseName(c)
				row = []string{c.QCKey, name, strings.TrimSpace(c.Purpose), strings.Join(c.Preconditions, "\n"), priority, status,
					strings.Join(labels, " "), folder, c.Owner, strings.Join(c.Requirement, ", "), "", "", ""}
			}
			row[10], row[11], row[12] = s.action, s.data, s.expected
			if err := w.Write(row); err != nil {
				return nil, err
			}
		}
	}
	w.Flush()
	return b.Bytes(), w.Error()
}

func compact(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func guide(cfg *config.QC, run *result.Run) string {
	project, api := "<PROJECT>", "https://api.zephyrscale.smartbear.com/v2"
	if cfg != nil {
		if cfg.ProjectKey != "" {
			project = cfg.ProjectKey
		}
		if cfg.API != "" {
			api = cfg.API
		}
	}
	return fmt.Sprintf(`# Đưa kết quả run %[1]s vào Jira (Zephyr Scale)

Tệp trong thư mục này:

| Tệp | Dùng để |
|-----|---------|
| testcases.csv | Tạo/cập nhật test case (lần đầu, hoặc khi có case mới): Zephyr Scale → Tests → Import → CSV. Mỗi bước một dòng; dòng đầu của case chứa các trường của case. Cột Key trống = case mới. Sau khi import, ghi key Zephyr (vd. %[2]s-T12) vào `+"`qc_key`"+` trong tệp YAML của case. |
| executions.zip | Kết quả thực thi (định dạng custom version 1 của Zephyr Scale, bên trong là executions.json). |
| test-cycle.json | Tên và mô tả test cycle sẽ được tạo. |

Đẩy kết quả (tạo một test cycle mới):

    ZEPHYR_TOKEN=... ./tk qc push out/%[1]s

hoặc tự gọi API:

    curl -H "Authorization: Bearer $ZEPHYR_TOKEN" \
      -F "file=@executions.zip" -F "testCycle=@test-cycle.json;type=application/json" \
      "%[3]s/automations/executions/custom?projectKey=%[2]s&autoCreateTestCases=false"

Kết quả pass/fail do TestKit quyết định bằng luật cứng; Zephyr chỉ lưu lại. Bằng chứng đầy đủ nằm trong gói %[1]s.zip.
`, run.RunID, project, api)
}

// Push uploads executions.zip and test-cycle.json of an exported bundle.
// The token comes from the environment and is never written anywhere.
func Push(ctx context.Context, bundle string, cfg *config.QC, token string, client *http.Client) (string, error) {
	if cfg == nil || cfg.ProjectKey == "" || cfg.API == "" {
		return "", fmt.Errorf("qc: project_key and api are required in testkit.yaml")
	}
	if token == "" {
		return "", fmt.Errorf("qc: no token in $%s", cfg.TokenEnv)
	}
	zipRaw, err := os.ReadFile(path.Join(bundle, Dir, "executions.zip"))
	if err != nil {
		return "", fmt.Errorf("qc: %w (run `testkit qc export` first?)", err)
	}
	cycRaw, err := os.ReadFile(path.Join(bundle, Dir, "test-cycle.json"))
	if err != nil {
		return "", err
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "executions.zip")
	_, _ = fw.Write(zipRaw)
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{`form-data; name="testCycle"; filename="test-cycle.json"`}
	h["Content-Type"] = []string{"application/json"}
	cw, _ := mw.CreatePart(h)
	_, _ = cw.Write(cycRaw)
	if err := mw.Close(); err != nil {
		return "", err
	}
	q := url.Values{"projectKey": {cfg.ProjectKey}, "autoCreateTestCases": {fmt.Sprint(cfg.AutoCreate)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.API, "/")+"/automations/executions/custom?"+q.Encode(), &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		return string(out), fmt.Errorf("zephyr: HTTP %d: %.500s", resp.StatusCode, out)
	}
	return string(out), nil
}

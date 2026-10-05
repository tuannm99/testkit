package zephyr

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tuannm99/testkit/testkit/core/config"
	"github.com/tuannm99/testkit/testkit/core/evidence"
	"github.com/tuannm99/testkit/testkit/core/result"
	"github.com/tuannm99/testkit/testkit/core/scenario"
)

func fixture() (*result.Run, []*scenario.Case) {
	t0 := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	run := &result.Run{RunID: "r20261005-080000-abcd", Suite: "release", StartedAt: t0, FinishedAt: t0.Add(time.Minute),
		Executions: []*result.Execution{
			{ID: "TC-A[kafka]", CaseID: "TC-A", Result: "pass", Dir: "TC-A/kafka"},
			{ID: "TC-A[db-poll]", CaseID: "TC-A", Result: "fail", Class: "product", Dir: "TC-A/db-poll"},
			{ID: "TC-B", CaseID: "TC-B", Result: "pass", Dir: "TC-B"},
			{ID: "TC-B{mutation M1}", CaseID: "TC-B", Mutation: "M1", Result: "fail"},
			{ID: "TC-C", CaseID: "TC-C", Result: "skipped", Class: "capability", Dir: "TC-C"},
		}}
	cases := []*scenario.Case{
		{ID: "TC-A", Title: "Retry", QCKey: "ORD-T1", File: "/x/TC-A.yaml", Risk: "P0", Status: "approved", Service: "svc",
			Requirement: scenario.Strings{"ORD-10"}, Expect: []scenario.ExpectSpec{{ID: "A1", Check: "postgres.order.o1.status", Op: "eq", Expected: "paid", Why: "paid"}}},
		{ID: "TC-B", Title: "Mail, \"quoted\"", File: "/x/TC-B.yaml", Steps: []scenario.StepSpec{{Step: "mock.script", With: map[string]any{"mock": "p"}}}},
		{ID: "TC-C", Title: "Netem", File: "/x/TC-C.yaml"},
	}
	return run, cases
}

func TestBuild(t *testing.T) {
	run, cases := fixture()
	res, cyc, err := Build(run, cases, &config.QC{CycleName: "{{ .suite }} {{ .date }}"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != 1 || len(res.Executions) != 2 {
		t.Fatalf("%+v", res)
	}
	a, b := res.Executions[0], res.Executions[1]
	if a.Result != "Failed" || a.TestCase.Key != "ORD-T1" || b.Result != "Passed" || b.TestCase.Name != `TC-B — Mail, "quoted"` {
		t.Fatalf("%+v %+v", a, b)
	}
	if cyc.Name != "release 2026-10-05" || !strings.Contains(cyc.Description, "Không chạy (bỏ qua): TC-C") {
		t.Fatalf("%+v", cyc)
	}
}

func TestExportAndPush(t *testing.T) {
	run, cases := fixture()
	dir := &evidence.Dir{Root: t.TempDir()}
	cfg := &config.QC{ProjectKey: "ORD", TokenEnv: "ZEPHYR_TOKEN"}
	if _, err := Export(dir, run, cases, cfg); err != nil {
		t.Fatal(err)
	}
	var got struct {
		auth, query, cycle string
		results            Results
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.auth, got.query = r.Header.Get("Authorization"), r.URL.Path+"?"+r.URL.RawQuery
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(f)
		zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
		if err != nil || len(zr.File) != 1 {
			t.Fatalf("zip: %v", err)
		}
		rc, _ := zr.File[0].Open()
		_ = json.NewDecoder(rc).Decode(&got.results)
		c, _, _ := r.FormFile("testCycle")
		cb, _ := io.ReadAll(c)
		got.cycle = string(cb)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"testCycle":{"key":"ORD-R1"}}`))
	}))
	defer srv.Close()
	cfg.API = srv.URL + "/v2"
	out, err := Push(context.Background(), dir.Root, cfg, "secret-token", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if got.auth != "Bearer secret-token" || got.query != "/v2/automations/executions/custom?autoCreateTestCases=false&projectKey=ORD" ||
		len(got.results.Executions) != 2 || !strings.Contains(got.cycle, run.RunID) || !strings.Contains(out, "ORD-R1") {
		t.Fatalf("%+v %s", got, out)
	}
	if _, err := Push(context.Background(), dir.Root, cfg, "", nil); err == nil {
		t.Fatal("push without token accepted")
	}
}

func TestCSV(t *testing.T) {
	_, cases := fixture()
	raw, err := TestCasesCSV(cases, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	// header + TC-A (1 check) + TC-B (1 step) + TC-C (placeholder)
	if len(rows) != 4 || rows[1][0] != "ORD-T1" || rows[1][9] != "ORD-10" || rows[1][4] != "High" ||
		!strings.HasPrefix(rows[1][10], "Kiểm tra A1") || rows[1][12] != "eq paid — paid" || rows[2][11] != `{"mock":"p"}` {
		t.Fatalf("%q", rows)
	}
}

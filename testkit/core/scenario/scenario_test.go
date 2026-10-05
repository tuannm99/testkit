package scenario

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/tuannm99/testkit/testkit/core/config"
	"github.com/tuannm99/testkit/testkit/core/kit"
)

func root(t *testing.T) string {
	_, f, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(f), "..", "..")
}

func registry() *kit.Registry {
	r := kit.NewRegistry()
	for _, n := range []string{"trigger.enqueue", "mock.script", "postgres.insert", "sut.restart", "wait.until"} {
		d := kit.StepDef{Name: n, Open: true}
		switch {
		case strings.HasPrefix(n, "mock"):
			d.Connector = "mock"
		case strings.HasPrefix(n, "postgres"):
			d.Connector = "postgres"
		}
		r.AddStep(d)
	}
	for _, p := range []string{"postgres", "mock", "mail", "es", "kafka", "sut"} {
		conn := p
		if p == "es" {
			conn = "elasticsearch"
		}
		r.AddCheck(kit.CheckDef{Prefix: p, Connector: conn})
	}
	return r
}

func services(t *testing.T) map[string]*config.Service {
	s, err := config.LoadServices(filepath.Join(root(t), "services"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestReferenceCaseLintsClean(t *testing.T) {
	c, err := Load(filepath.Join(root(t), "scenarios", "order", "TC-ORDER-017.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if issues := Lint(c, services(t), registry()); len(issues) > 0 {
		t.Fatalf("issues: %v", issues)
	}
	steps, err := c.Expand()
	if err != nil {
		t.Fatal(err)
	}
	// fixtures, then mocks, then the job — whatever the YAML order
	got := []string{steps[0].Step, steps[1].Step, steps[2].Step}
	if strings.Join(got, ",") != "postgres.insert,mock.script,trigger.enqueue" {
		t.Fatalf("expansion order %v", got)
	}
	resp := steps[1].With["responses"].([]any)
	if len(resp) != 3 || resp[1].(map[string]any)["retry_after"] != "2" {
		t.Fatalf("responses %v", resp)
	}
}

func TestLintCatchesMissingFieldsAndUnknowns(t *testing.T) {
	yaml := `id: tc-1
title: x
requirement: REQ-1
risk: P9
status: approved
service: order-worker
purpose: p
preconditions: [a]
input: {}
evidence: { grafana: [nope] }
trigger: [kafka, sqs]
given:
  mock.unknown: [500]
steps:
  - step: does.not.exist
expect:
  - { id: A1, check: postgres.order.o1.status, eq: paid }
  - { id: A1, check: redis.x.y, eq: 1, why: dup }
  - { id: A3, check: mock.payment.calls, why: no op }
mutations:
  - { id: M1, failpoint: not_a_failpoint, expect_red: [A9] }
`
	p := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	var msgs []string
	for _, i := range Lint(c, services(t), registry()) {
		msgs = append(msgs, i.Msg)
	}
	all := strings.Join(msgs, "\n")
	for _, want := range []string{
		`id "tc-1" must look like TC-AREA-123`, "risk must be P0, P1 or P2", `trigger "sqs" is not declared`,
		`panel "nope" is not declared`, `unknown step "does.not.exist"`, `mock "unknown" is not declared`,
		"assertion A1: why is required", "duplicate assertion id A1", `unknown check source "redis"`,
		"assertion A3: operator missing", `failpoint "not_a_failpoint" is not declared`, "unknown assertion A9",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing lint error %q in:\n%s", want, all)
		}
	}
}

func TestUnknownKeysRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.yaml")
	_ = os.WriteFile(p, []byte("id: TC-A-1\ntitel: typo\n"), 0o644)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "titel") {
		t.Fatalf("typo not rejected: %v", err)
	}
}

func TestParseResponses(t *testing.T) {
	out, err := ParseResponses([]any{500, "429(retry-after=2)", "503*2", "reset", "200(delay=300ms)"})
	if err != nil || len(out) != 6 {
		t.Fatalf("%v %v", out, err)
	}
	if out[4].(map[string]any)["fault"] != "reset" || out[5].(map[string]any)["delay"].(map[string]any)["fixed"] != "300ms" {
		t.Fatalf("%v", out)
	}
	if _, err := ParseResponses([]any{"42x"}); err == nil {
		t.Fatal("bad shorthand accepted")
	}
}

func TestRenderKeepsTypesForWholeReferences(t *testing.T) {
	data := map[string]any{"input": map[string]any{"order": map[string]any{"id": "o1"}}, "ns": "tk_1"}
	v, err := RenderString("{{ .input.order }}", data)
	if err != nil || v.(map[string]any)["id"] != "o1" {
		t.Fatalf("%v %v", v, err)
	}
	v, _ = RenderString("cust+{{ .ns }}@x", data)
	if v != "cust+tk_1@x" {
		t.Fatal(v)
	}
	if _, err := RenderString("{{ .missing }}", data); err == nil {
		t.Fatal("missing key accepted")
	}
}

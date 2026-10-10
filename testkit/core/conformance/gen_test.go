package conformance

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuannm99/testkit/testkit/core/config"
	"github.com/tuannm99/testkit/testkit/core/scenario"
)

const descriptor = `
apiVersion: testkit/v1
kind: Service
name: demo
owner: team-demo
image: { name: demo, tag: "1.0", test_tag: "1.0-fp" }
ports: { http: 8080 }
health: { port: http, path: /healthz, timeout: 10s }
stores:
  postgres: { migrations: ., snapshot: [orders] }
  kafka: { topics: [{ name: jobs, partitions: 1 }, { name: jobs.dlq, partitions: 1 }], groups: [demo] }
mocks: {}
triggers:
  kafka: { topic: jobs, group: demo, dlq: jobs.dlq, key: "{{ .job.id }}", value: '{"id":"{{ .job.id }}"}' }
  db-poll:
    table: jobs
    sql: "INSERT INTO jobs (job_key) VALUES ('{{ .job.id }}')"
    drained: "SELECT count(*) FROM jobs WHERE status <> 'done'"
    dead: "SELECT count(*) FROM jobs WHERE status = 'dead'"
entities:
  order: { postgres: { table: orders, key: id } }
failpoints:
  skip_check: charges twice
  dead_drop: drops dead letters
  die: exits
env: {}
perf: { fixture: "INSERT INTO orders SELECT '{{ .prefix }}' || g FROM generate_series({{ .from }}, {{ .to }}) g", completion: "SELECT 1" }
chaos: { proxies: { postgres: { upstream: "postgres:5432", env: { DATABASE_URL: "x" } } } }
conformance:
  given:
    postgres.order: [ { id: "{{ .key }}", status: pending } ]
  done:
    - { check: "postgres.order.{{ .key }}.status", eq: paid }
    - { check: "postgres.order.count(status=paid)", per_job: 1, why: "all {{ .jobs }} paid" }
  effects:
    - { check: mock.pay.succeeded, per_job: 1 }
  patterns:
    duplicate-delivery:
      copies: 4
      jobs: 2
      mutations: [ { failpoint: skip_check, title: charges twice, expect_red: [mock.pay.succeeded] } ]
    poison-message:
      mutations: [ { failpoint: dead_drop, title: dropped, expect_red: [poison] } ]
    crash-mid-job: { failpoint: die, log: "exiting now" }
    dependency-fault:
      faults: [ { proxy: postgres, fault: down, for: 3s } ]
    out-of-order: { jobs: 3 }
`

func load(t *testing.T, yml string) *config.Service {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "demo.yaml")
	if err := os.WriteFile(p, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "001.sql"), []byte("select 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := config.LoadService(p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func byPath(p *Plan) map[string]File {
	m := map[string]File{}
	for _, f := range p.Files {
		m[f.Path] = f
	}
	return m
}

func TestGenerateIsDeterministicAndParses(t *testing.T) {
	svc := load(t, descriptor)
	a, err := Generate(svc, Options{Source: "testkit/services/demo.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Generate(svc, Options{Source: "testkit/services/demo.yaml"})
	if len(a.Files) != len(b.Files) {
		t.Fatal("different number of files")
	}
	for i := range a.Files {
		if !bytes.Equal(a.Files[i].Body, b.Files[i].Body) {
			t.Fatalf("%s is not deterministic", a.Files[i].Path)
		}
		dir := t.TempDir()
		p := filepath.Join(dir, "case.yaml")
		_ = os.WriteFile(p, a.Files[i].Body, 0o644)
		c, err := scenario.Load(p)
		if err != nil {
			t.Fatalf("%s: %v\n%s", a.Files[i].Path, err, a.Files[i].Body)
		}
		if c.Status != "draft" || c.Admission != nil {
			t.Fatalf("%s must be a draft, got %q", c.ID, c.Status)
		}
	}
	// duplicate + poison×2 (kafka, db-poll: db-poll has no SQL → skipped) + crash + fault + order
	got := byPath(a)
	for _, want := range []string{"demo/duplicate-delivery.yaml", "demo/poison-message-kafka.yaml", "demo/crash-mid-job.yaml",
		"demo/dependency-fault-postgres-down.yaml", "demo/out-of-order.yaml"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %s (have %v)", want, keysOf(got))
		}
	}
	if len(a.Skips) != 1 || a.Skips[0].Pattern != config.PatPoison || a.Skips[0].Trigger != "db-poll" {
		t.Fatalf("skips = %+v, want the db-poll poison message", a.Skips)
	}
}

func keysOf(m map[string]File) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestPerKeyAndTotalExpectations(t *testing.T) {
	svc := load(t, descriptor)
	plan, err := Generate(svc, Options{Patterns: []string{config.PatDuplicate}})
	if err != nil {
		t.Fatal(err)
	}
	body := string(plan.Files[0].Body)
	for _, want := range []string{"postgres.order.k1.status", "postgres.order.k2.status", "duplicate: 4",
		"eq: 2, why: all 2 paid", "check: mock.pay.succeeded, eq: 2", "check: trigger.dlq, eq: 0"} {
		if !strings.Contains(body, want) {
			t.Errorf("duplicate-delivery lacks %q:\n%s", want, body)
		}
	}
	// A big case uses totals only.
	plan, _ = Generate(svc, Options{Patterns: []string{config.PatFault}})
	body = string(plan.Files[0].Body)
	if strings.Contains(body, "order.k1.status") {
		t.Errorf("a bulk case must not assert per-job rows:\n%s", body)
	}
	if !strings.Contains(body, "eq: 100, why: all 100 paid") {
		t.Errorf("bulk total missing:\n%s", body)
	}
}

func TestExpectRedResolvesGroupsAndCheckPrefixes(t *testing.T) {
	svc := load(t, descriptor)
	plan, err := Generate(svc, Options{Patterns: []string{config.PatDuplicate, config.PatPoison}})
	if err != nil {
		t.Fatal(err)
	}
	by := byPath(plan)
	dup := string(by["demo/duplicate-delivery.yaml"].Body)
	// A1,A2 per-key done; A3 total; A4 effects total (mock.pay.succeeded)
	if !strings.Contains(dup, "expect_red: [A4]") {
		t.Errorf("check prefix should select only the mock.pay.succeeded assertion:\n%s", dup)
	}
	poison := string(by["demo/poison-message-kafka.yaml"].Body)
	if !strings.Contains(poison, "expect_red: [A1]") {
		t.Errorf("group poison should select the dlq assertion:\n%s", poison)
	}
}

func TestDescriptorProblemsAreReported(t *testing.T) {
	// a trigger without a dead-letter destination
	svc := load(t, strings.Replace(descriptor, "dlq: jobs.dlq, ", "", 1))
	if _, err := Generate(svc, Options{}); err == nil || !strings.Contains(err.Error(), "triggers.kafka.dlq") {
		t.Fatalf("want a message about triggers.kafka.dlq, got %v", err)
	}
	// a mutation naming something that is not in the case
	svc = load(t, strings.Replace(descriptor, "expect_red: [poison]", "expect_red: [nonsense]", 1))
	if _, err := Generate(svc, Options{}); err == nil || !strings.Contains(err.Error(), "nonsense") {
		t.Fatalf("want a message about expect_red nonsense, got %v", err)
	}
	// unknown failpoint in a mutation is rejected when the descriptor loads
	dir := t.TempDir()
	p := filepath.Join(dir, "demo.yaml")
	_ = os.WriteFile(p, []byte(strings.Replace(descriptor, "failpoint: dead_drop", "failpoint: nope", 1)), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "001.sql"), []byte("select 1;"), 0o644)
	if _, err := config.LoadService(p); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("want the descriptor to be rejected for failpoint nope, got %v", err)
	}
}

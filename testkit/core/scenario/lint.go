package scenario

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tuannm99/testkit/testkit/core/assert"
	"github.com/tuannm99/testkit/testkit/core/config"
	"github.com/tuannm99/testkit/testkit/core/kit"
)

// Issue is one lint finding.
type Issue struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Severity string `json:"severity"` // error | warning
	Msg      string `json:"msg"`
}

func (i Issue) String() string {
	return fmt.Sprintf("%s:%d: %s: %s", i.File, i.Line, i.Severity, i.Msg)
}

var (
	idRe    = regexp.MustCompile(`^TC-[A-Z0-9]+(-[A-Z0-9]+)*$`)
	reqRe   = regexp.MustCompile(`^[A-Z][A-Z0-9]*-[A-Z0-9-]+$`)
	assIDRe = regexp.MustCompile(`^[A-Z][A-Z0-9_-]*$`)
)

// Lint validates a case against the schema, the step/check registry and the
// service descriptor. It never touches infrastructure.
func Lint(c *Case, services map[string]*config.Service, reg *kit.Registry) []Issue {
	file := filepath.Base(c.File)
	var out []Issue
	errf := func(line int, f string, a ...any) {
		out = append(out, Issue{File: file, Line: line, Severity: "error", Msg: fmt.Sprintf(f, a...)})
	}
	warnf := func(line int, f string, a ...any) {
		out = append(out, Issue{File: file, Line: line, Severity: "warning", Msg: fmt.Sprintf(f, a...)})
	}
	line := func(k string) int {
		if l, ok := c.Line[k]; ok {
			return l
		}
		return 1
	}

	// --- mandatory fields -------------------------------------------------------
	if !idRe.MatchString(c.ID) {
		errf(line("id"), "id %q must look like TC-AREA-123", c.ID)
	}
	if strings.TrimSpace(c.Title) == "" {
		errf(line("title"), "title is required")
	}
	if len(c.Requirement) == 0 {
		errf(line("requirement"), "requirement is required (traceability REQ -> TC)")
	}
	for _, r := range c.Requirement {
		if !reqRe.MatchString(r) {
			errf(line("requirement"), "requirement %q must look like REQ-231", r)
		}
	}
	switch c.Risk {
	case "P0", "P1", "P2":
	default:
		errf(line("risk"), "risk must be P0, P1 or P2 (got %q)", c.Risk)
	}
	switch c.Status {
	case "draft", "approved":
	case "":
		errf(1, "status is required: draft | approved")
	default:
		errf(line("status"), "status must be draft or approved (got %q)", c.Status)
	}
	if strings.TrimSpace(c.Purpose) == "" {
		errf(line("purpose"), "purpose is required (what the case proves, for the QC reader)")
	}
	if _, ok := c.Line["preconditions"]; !ok || len(c.Preconditions) == 0 {
		errf(line("preconditions"), "preconditions are required (state assumed before the steps)")
	}
	if _, ok := c.Line["input"]; !ok {
		errf(1, "input is required (use input: {} when the case has none)")
	}
	if _, ok := c.Line["evidence"]; !ok {
		errf(1, "evidence is required (grafana panels, snapshots; evidence: {} for defaults)")
	}
	if len(c.Expect) == 0 {
		errf(line("expect"), "expect must contain at least one assertion")
	}
	if c.Within != "" {
		if _, err := time.ParseDuration(c.Within); err != nil {
			errf(line("within"), "within %q is not a duration (e.g. 30s)", c.Within)
		}
	}

	// --- service -------------------------------------------------------------
	svc, ok := services[c.Service]
	if !ok {
		errf(line("service"), "service %q has no descriptor in services/ (known: %s)", c.Service,
			strings.Join(config.SortedKeys(services), ", "))
		return out
	}
	for _, t := range c.Trigger {
		if _, ok := svc.Triggers[t]; !ok {
			errf(line("trigger"), "trigger %q is not declared by service %s (declared: %s)", t, svc.Name,
				strings.Join(config.SortedKeys(svc.Triggers), ", "))
		}
	}
	for _, fp := range c.Failpoints {
		name, _, _ := strings.Cut(fp, "=")
		if _, ok := svc.Failpoints[name]; !ok {
			errf(line("failpoints"), "failpoint %q is not declared by service %s", name, svc.Name)
		}
	}
	if len(c.Failpoints) > 0 && svc.Image.TestTag == "" {
		errf(line("failpoints"), "failpoints need a test image (image.test_tag) in the service descriptor")
	}
	for _, m := range c.Mutations {
		if _, ok := svc.Failpoints[m.Failpoint]; !ok {
			errf(line("mutations"), "mutation %s: failpoint %q is not declared by service %s", m.ID, m.Failpoint, svc.Name)
		}
	}
	for _, p := range c.Evidence.Grafana {
		if _, ok := svc.Panels[p]; !ok {
			errf(line("evidence"), "evidence.grafana: panel %q is not declared by service %s (declared: %s)", p, svc.Name,
				strings.Join(config.SortedKeys(svc.Panels), ", "))
		}
	}

	// --- steps -----------------------------------------------------------------
	steps, err := c.Expand()
	if err != nil {
		errf(line("given"), "%v", err)
	}
	if len(steps) == 0 {
		errf(line("steps"), "the case has no steps (given or steps)")
	}
	for _, s := range steps {
		def, ok := reg.Lookup(s.Step)
		if !ok {
			errf(s.Line, "unknown step %q (see `testkit steps`)", s.Step)
			continue
		}
		for _, e := range def.Validate(s.With) {
			errf(s.Line, "%s", e)
		}
		if def.Connector != "" && !serviceHas(svc, def.Connector) {
			errf(s.Line, "step %s needs connector %s, not declared by service %s", s.Step, def.Connector, svc.Name)
		}
		if s.Step == "trigger.enqueue" && len(c.Trigger) == 0 {
			errf(s.Line, "trigger.enqueue (given.job) needs `trigger:` (kafka, db-poll)")
		}
		if s.Step == "mock.script" {
			if m := kit.Str(s.With, "mock"); m != "" {
				if _, ok := svc.Mocks[m]; !ok {
					errf(s.Line, "mock %q is not declared by service %s", m, svc.Name)
				}
			}
		}
		if strings.HasSuffix(s.Step, ".insert") {
			store := strings.TrimSuffix(s.Step, ".insert")
			ent := kit.Str(s.With, "entity")
			if e, ok := svc.Entities[ent]; !ok || entityTable(e, store) == nil {
				errf(s.Line, "entity %q has no %s mapping in service %s", ent, store, svc.Name)
			}
		}
	}

	// --- assertions --------------------------------------------------------------
	seen := map[string]bool{}
	for _, e := range c.Expect {
		if !assIDRe.MatchString(e.ID) {
			errf(e.Line, "assertion id %q must look like A1", e.ID)
		}
		if seen[e.ID] {
			errf(e.Line, "duplicate assertion id %s", e.ID)
		}
		seen[e.ID] = true
		if strings.TrimSpace(e.Why) == "" {
			errf(e.Line, "assertion %s: why is required (the reason shown to QC)", e.ID)
		}
		if e.Op == "" {
			errf(e.Line, "assertion %s: operator missing (one of %s)", e.ID, strings.Join(assert.Operators, ", "))
		} else if !assert.IsOperator(e.Op) {
			errf(e.Line, "assertion %s: unknown operator %q", e.ID, e.Op)
		} else if assert.NeedsExpected(e.Op) && !e.hasExp {
			errf(e.Line, "assertion %s: operator %s needs an expected value", e.ID, e.Op)
		}
		ref, err := assert.ParseCheck(e.Check)
		if err != nil {
			errf(e.Line, "assertion %s: %v", e.ID, err)
			continue
		}
		if msg := checkSupported(ref, svc, reg); msg != "" {
			errf(e.Line, "assertion %s: %s", e.ID, msg)
		}
	}
	for _, m := range c.Mutations {
		for _, id := range m.ExpectRed {
			if !seen[id] {
				errf(line("mutations"), "mutation %s expects red on unknown assertion %s", m.ID, id)
			}
		}
	}

	// --- templates render with placeholder values ----------------------------------
	for _, trig := range c.Triggers() {
		if _, err := c.Data("r00000000-000000-lint", "tk_lint", trig); err != nil {
			errf(line("vars"), "%v", err)
			break
		}
	}
	if c.Generated != nil && c.Status == "approved" && c.Owner == "" {
		warnf(line("generated"), "generated case approved without an owner (who reviewed it?)")
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

// serviceHas reports whether the service declares what a connector needs.
func serviceHas(svc *config.Service, connector string) bool {
	switch connector {
	case "postgres":
		return svc.Stores.Postgres != nil
	case "kafka":
		return svc.Stores.Kafka != nil
	case "elasticsearch":
		return svc.Stores.Elasticsearch != nil
	case "clickhouse":
		return svc.Stores.ClickHouse != nil
	case "mongo":
		return svc.Stores.Mongo != nil
	case "redis":
		return svc.Stores.Redis != nil
	case "mock", "webhook":
		return len(svc.Mocks) > 0
	case "mail":
		for _, m := range svc.Mocks {
			if m.Kind == "smtp" {
				return true
			}
		}
		return false
	case "socket":
		for _, m := range svc.Mocks {
			if m.Kind == "socket" {
				return true
			}
		}
		return false
	}
	return true // sut, chaos, trigger, prom, ...: always available (capabilities checked at run time)
}

func entityTable(e config.Entity, store string) *config.EntityTable {
	switch store {
	case "postgres":
		return e.Postgres
	case "es", "elasticsearch":
		return e.Elasticsearch
	case "clickhouse", "ch":
		return e.ClickHouse
	case "mongo":
		return e.Mongo
	}
	return nil
}

// checkSupported validates the source of a check against registry + service.
func checkSupported(ref kit.CheckRef, svc *config.Service, reg *kit.Registry) string {
	def, ok := reg.Checks[ref.Prefix()]
	if !ok {
		known := config.SortedKeys(reg.Checks)
		return fmt.Sprintf("unknown check source %q (known: %s)", ref.Prefix(), strings.Join(known, ", "))
	}
	if !serviceHas(svc, def.Connector) {
		return fmt.Sprintf("check %s needs %s, not declared by service %s", ref.Raw, def.Connector, svc.Name)
	}
	switch ref.Prefix() {
	case "mock":
		if _, ok := svc.Mocks[ref.Segments[1].Name]; !ok {
			return fmt.Sprintf("mock %q is not declared by service %s", ref.Segments[1].Name, svc.Name)
		}
	case "postgres", "es", "clickhouse", "mongo":
		name := ref.Segments[1].Name
		if name == "sql" || name == "query" || name == "index" || name == "table" || name == "collection" {
			return ""
		}
		e, ok := svc.Entities[name]
		if !ok || entityTable(e, ref.Prefix()) == nil {
			return fmt.Sprintf("entity %q has no %s mapping in service %s", name, ref.Prefix(), svc.Name)
		}
	}
	return ""
}

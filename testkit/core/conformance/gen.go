// Package conformance generates the standard case pack of a service from its
// descriptor (`conformance:` section): duplicate delivery, poison message,
// crash mid-job, dependency fault, out-of-order delivery and steady load.
//
// The generator is deterministic (no model, no clock): the same descriptor
// gives the same files byte for byte, so `testkit gen --check` can tell when
// the descriptor changed. Generated cases are always `status: draft`; only a
// person approves them (`testkit admit --approve`).
package conformance

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/tuannm99/testkit/testkit/core/assert"
	"github.com/tuannm99/testkit/testkit/core/config"
)

// Options of one generation.
type Options struct {
	Patterns []string // restrict to these patterns (default: every configured pattern)
	Source   string   // descriptor path written into the provenance (relative to the project)
}

// File is one generated case.
type File struct {
	Path    string // relative to the output directory: <service>/<name>.yaml
	ID      string
	Pattern string
	Trigger string // single trigger when the case is specific to one ("" = all covered triggers)
	Body    []byte
}

// Skip explains why a pattern was not generated for a trigger.
type Skip struct {
	Pattern string
	Trigger string
	Reason  string
}

// Plan is the result of a generation.
type Plan struct {
	Files []File
	Skips []Skip
}

var (
	keyRe  = regexp.MustCompile(`\{\{\s*\.key\s*\}\}`)
	jobsRe = regexp.MustCompile(`\{\{\s*\.jobs\s*\}\}`)
	nlRe   = regexp.MustCompile(`\s*\n\s*`)
)

// Generate builds the pack of one service.
func Generate(svc *config.Service, opt Options) (*Plan, error) {
	c := svc.Conformance
	if c == nil {
		return nil, fmt.Errorf("service %s has no conformance section", svc.Name)
	}
	g := &gen{svc: svc, c: c, opt: opt, plan: &Plan{}}
	if err := g.checkDescriptor(); err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, p := range opt.Patterns {
		ok := false
		for _, n := range config.PatternNames {
			ok = ok || n == p
		}
		if !ok {
			return nil, fmt.Errorf("unknown pattern %q (%s)", p, strings.Join(config.PatternNames, ", "))
		}
		want[p] = true
	}
	for _, name := range config.PatternNames {
		p, ok := c.Patterns[name]
		if !ok || (len(want) > 0 && !want[name]) {
			continue
		}
		if p == nil {
			p = &config.ConformancePattern{}
		}
		var err error
		switch name {
		case config.PatDuplicate:
			err = g.duplicate(p)
		case config.PatPoison:
			err = g.poison(p)
		case config.PatCrash:
			err = g.crash(p)
		case config.PatFault:
			err = g.fault(p)
		case config.PatOrder:
			err = g.order(p)
		case config.PatLoad:
			err = g.load(p)
		}
		if err != nil {
			return nil, fmt.Errorf("conformance.patterns.%s: %w", name, err)
		}
	}
	return g.plan, nil
}

type gen struct {
	svc  *config.Service
	c    *config.Conformance
	opt  Options
	plan *Plan
}

// triggers covered by a pattern, in the service's stable order.
func (g *gen) triggers(p *config.ConformancePattern) []string {
	list := p.Triggers
	if len(list) == 0 {
		list = g.c.Triggers
	}
	if len(list) == 0 {
		for t := range g.svc.Triggers {
			list = append(list, t)
		}
	}
	order := map[string]int{"kafka": 0, "db-poll": 1, "rabbitmq": 2, "redis": 3}
	out := append([]string(nil), list...)
	sort.Slice(out, func(i, j int) bool { return order[out[i]] < order[out[j]] })
	return out
}

// checkDescriptor verifies what every pattern relies on: each covered trigger
// declares where dead letters go (trigger.dlq) and what "drained" means (trigger.backlog).
func (g *gen) checkDescriptor() error {
	var problems []string
	covered := map[string]bool{}
	for _, p := range g.c.Patterns {
		if p == nil {
			p = &config.ConformancePattern{}
		}
		for _, t := range g.triggers(p) {
			covered[t] = true
		}
	}
	for t := range covered {
		spec := g.svc.Triggers[t]
		switch t {
		case "kafka":
			if spec.DLQ == "" {
				problems = append(problems, "triggers.kafka.dlq: the logical name of the dead-letter topic is required by the standard pack")
			}
		case "db-poll":
			if spec.Drained == "" {
				problems = append(problems, "triggers.db-poll.drained: required by the standard pack (trigger.backlog)")
			}
			if spec.Dead == "" {
				problems = append(problems, "triggers.db-poll.dead: SQL counting the jobs given up on is required by the standard pack (trigger.dlq)")
			}
		case "rabbitmq":
			for _, q := range g.svc.Stores.RabbitMQ.Queues {
				if q.Name == spec.Queue && q.DLQ == "" {
					problems = append(problems, fmt.Sprintf("stores.rabbitmq.queues.%s.dlq: required by the standard pack", q.Name))
				}
			}
		case "redis":
			if q, ok := g.svc.RedisQueueOf(spec.Queue); ok && q.DLQ == "" {
				problems = append(problems, fmt.Sprintf("stores.redis.queues.%s.dlq: required by the standard pack", q.Name))
			}
		}
	}
	if len(g.c.Done) == 0 {
		problems = append(problems, "conformance.done: say what holds once a job is complete (at least one check)")
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("the descriptor is not ready for the standard pack:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// ---------------------------------------------------------------------------------------
// YAML building blocks

func str(v string) *yaml.Node {
	v = strings.TrimSpace(v)
	n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: nlRe.ReplaceAllString(v, " ")}
	return n
}

func val(v any) *yaml.Node {
	n := &yaml.Node{}
	if err := n.Encode(v); err != nil {
		panic(err)
	}
	return n
}

func mapping(kv ...any) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for i := 0; i+1 < len(kv); i += 2 {
		k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: kv[i].(string)}
		var v *yaml.Node
		switch x := kv[i+1].(type) {
		case *yaml.Node:
			v = x
		case string:
			v = str(x)
		default:
			v = val(x)
		}
		n.Content = append(n.Content, k, v)
	}
	return n
}

func seq(items ...*yaml.Node) *yaml.Node {
	return &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: items}
}

func flow(n *yaml.Node) *yaml.Node {
	if n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode {
		n.Style |= yaml.FlowStyle
		for _, c := range n.Content {
			flow(c)
		}
	}
	return n
}

func strs(list ...string) *yaml.Node {
	var items []*yaml.Node
	for _, s := range list {
		items = append(items, str(s))
	}
	return flow(seq(items...))
}

func clone(n *yaml.Node) *yaml.Node {
	c := *n
	c.Content = nil
	for _, ch := range n.Content {
		c.Content = append(c.Content, clone(ch))
	}
	return &c
}

func substitute(n *yaml.Node, key string) {
	if n.Kind == yaml.ScalarNode {
		n.Value = keyRe.ReplaceAllString(n.Value, key)
	}
	for _, c := range n.Content {
		substitute(c, key)
	}
}

func subStr(s, key string, jobs int) string {
	s = keyRe.ReplaceAllString(s, key)
	return jobsRe.ReplaceAllString(s, fmt.Sprint(jobs))
}

func keys(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("k%d", i+1)
	}
	return out
}

// ---------------------------------------------------------------------------------------
// Case assembly

type assertion struct {
	group string
	check string
	node  *yaml.Node // flow mapping without id; the id is added at the end
}

type builder struct {
	g          *gen
	pattern    string
	idSuffix   string
	file       string
	trigger    string // specific trigger, "" = every covered
	triggers   []string
	title      string
	risk       string
	purpose    string
	pre        []string
	given      *yaml.Node
	vars       *yaml.Node
	steps      []*yaml.Node
	asserts    []assertion
	within     string
	mutations  []config.ConformanceMutation
	failpoints []string
	sut        *yaml.Node
	chaos      *yaml.Node
	perf       *yaml.Node
	jobs       int
}

func (g *gen) newBuilder(pattern, suffix, file string, triggers []string, p *config.ConformancePattern) *builder {
	return &builder{g: g, pattern: pattern, idSuffix: suffix, file: file, triggers: triggers, mutations: p.Mutations}
}

func (b *builder) id() string {
	s := strings.ToUpper(b.g.svc.Name) + "-" + b.idSuffix
	return "TC-STD-" + strings.ReplaceAll(s, "_", "-")
}

// givenFor merges the per-job rows of conformance.given for n jobs, then conformance.shared.
func (b *builder) givenFor(n int) error {
	out := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	index := map[string]*yaml.Node{}
	for _, k := range keys(n) {
		for i := 0; i+1 < len(b.g.c.Given.Content); i += 2 {
			step, rows := b.g.c.Given.Content[i].Value, b.g.c.Given.Content[i+1]
			if rows.Kind != yaml.SequenceNode {
				return fmt.Errorf("conformance.given.%s must be a list of rows", step)
			}
			dst, ok := index[step]
			if !ok {
				dst = seq()
				index[step] = dst
				out.Content = append(out.Content, str(step), dst)
			}
			for _, r := range rows.Content {
				c := clone(r)
				substitute(c, k)
				dst.Content = append(dst.Content, flow(c))
			}
		}
	}
	if sh := b.g.c.Shared; sh.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(sh.Content); i += 2 {
			step := sh.Content[i].Value
			if _, dup := index[step]; dup {
				return fmt.Errorf("%s is in both conformance.given and conformance.shared", step)
			}
			v := clone(sh.Content[i+1])
			out.Content = append(out.Content, str(step), flow(v))
		}
	}
	b.given = out
	if len(b.g.c.Vars) > 0 {
		names := make([]string, 0, len(b.g.c.Vars))
		for k := range b.g.c.Vars {
			names = append(names, k)
		}
		sort.Strings(names)
		vars := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for _, k := range keys(n) {
			for _, name := range names {
				vars.Content = append(vars.Content, str(subStr(name, k, n)), str(subStr(b.g.c.Vars[name], k, n)))
			}
		}
		b.vars = vars
	}
	return nil
}

func (b *builder) addStep(step string, with *yaml.Node, name string) {
	kv := []any{"step", step}
	if name != "" {
		kv = append(kv, "name", name)
	}
	if with != nil {
		kv = append(kv, "with", flow(with))
	}
	b.steps = append(b.steps, mapping(kv...))
}

// itemExpect converts one descriptor item (done/effects) into an expectation for the given key.
func (g *gen) itemExpect(it map[string]any, key string, jobs int) (check string, opKey string, expected any, why string, err error) {
	check, _ = it["check"].(string)
	check = subStr(check, key, jobs)
	if w, ok := it["why"].(string); ok {
		why = subStr(w, key, jobs)
	}
	for k, v := range it {
		switch {
		case k == "check" || k == "why":
		case k == "per_job":
			n, ok := toInt(v)
			if !ok {
				return "", "", nil, "", fmt.Errorf("%s: per_job must be a number", check)
			}
			opKey, expected = "eq", n*jobs
		case assert.IsOperator(k):
			if opKey != "" {
				return "", "", nil, "", fmt.Errorf("%s: two operators", check)
			}
			opKey, expected = k, v
		default:
			return "", "", nil, "", fmt.Errorf("%s: unknown key %q (check, why, per_job or an operator)", check, k)
		}
	}
	if opKey == "" {
		return "", "", nil, "", fmt.Errorf("%s: needs an operator (eq, gte...) or per_job", check)
	}
	return check, opKey, expected, why, nil
}

func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	}
	return 0, false
}

func isPerKey(it map[string]any) bool {
	c, _ := it["check"].(string)
	return keyRe.MatchString(c)
}

func isTotal(it map[string]any) bool { return !isPerKey(it) }

// addItems appends the done/effects expectations: per-key items for each key (when perKey),
// totals once.
func (b *builder) addItems(group string, items []map[string]any, jobs int, perKey bool) error {
	for _, it := range items {
		if isPerKey(it) {
			if !perKey {
				continue
			}
			for _, k := range keys(jobs) {
				if err := b.addItem(group, it, k, jobs); err != nil {
					return err
				}
			}
			continue
		}
		if err := b.addItem(group, it, "", jobs); err != nil {
			return err
		}
	}
	return nil
}

func (b *builder) addItem(group string, it map[string]any, key string, jobs int) error {
	check, op, exp, why, err := b.g.itemExpect(it, key, jobs)
	if err != nil {
		return fmt.Errorf("conformance.%s: %w", group, err)
	}
	kv := []any{"check", check, op, exp}
	if why != "" {
		kv = append(kv, "why", why)
	}
	b.asserts = append(b.asserts, assertion{group: group, check: check, node: mapping(kv...)})
	return nil
}

func (b *builder) addCheck(group, check, op string, exp any, why string) {
	b.asserts = append(b.asserts, assertion{group: group, check: check, node: mapping("check", check, op, exp, "why", why)})
}

func (b *builder) reconcile() {
	names := make([]string, 0, len(b.g.svc.Reconcile))
	for n := range b.g.svc.Reconcile {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		b.addCheck("reconcile", "reconcile."+n+".mismatches", "eq", 0, "Các kho dữ liệu khớp nhau sau thí nghiệm (reconcile."+n+")")
	}
}

// build finalises the case into a File and appends it to the plan.
func (b *builder) build() error {
	g := b.g
	c := g.c
	req := c.Requirement
	if req == "" {
		req = "REQ-STD"
	}
	owner := c.Owner
	if owner == "" {
		owner = g.svc.Owner
	}
	within := b.within
	if within == "" {
		within = c.Within
	}
	if within == "" {
		within = "45s"
	}
	// Assertion ids and groups.
	groups := map[string][]string{}
	var expect []*yaml.Node
	for i, a := range b.asserts {
		id := fmt.Sprintf("A%d", i+1)
		groups[a.group] = append(groups[a.group], id)
		n := mapping("id", id)
		n.Content = append(n.Content, a.node.Content...)
		expect = append(expect, flow(n))
	}
	var muts []*yaml.Node
	for i, m := range b.mutations {
		var red []string
		for _, ref := range m.ExpectRed {
			// A group name selects every assertion of the group; anything else is a check
			// prefix (mock.payment.succeeded) selecting the assertions whose check starts with it.
			ids, ok := groups[ref]
			if !ok {
				for j, a := range b.asserts {
					if strings.HasPrefix(a.check, ref) {
						ids = append(ids, fmt.Sprintf("A%d", j+1))
					}
				}
			}
			if len(ids) == 0 {
				avail := make([]string, 0, len(groups))
				for k := range groups {
					avail = append(avail, k)
				}
				sort.Strings(avail)
				return fmt.Errorf("mutation %s: expect_red %q matches neither an assertion group (%s) nor the check of an assertion of this case", m.Failpoint, ref, strings.Join(avail, ", "))
			}
			red = append(red, ids...)
		}
		kv := []any{"id", fmt.Sprintf("M%d", i+1), "failpoint", m.Failpoint, "title", m.Title}
		if len(red) > 0 {
			kv = append(kv, "expect_red", strs(red...))
		}
		if len(m.Triggers) > 0 {
			kv = append(kv, "triggers", strs(m.Triggers...))
		}
		muts = append(muts, flow(mapping(kv...)))
	}
	pre := make([]*yaml.Node, len(b.pre))
	for i, p := range b.pre {
		pre[i] = str(p)
	}
	purpose := str(b.purpose)
	purpose.Style = yaml.FoldedStyle
	doc := mapping(
		"id", b.id(),
		"title", b.title,
		"requirement", req,
		"risk", b.risk,
		"status", "draft",
		"owner", owner,
		"service", g.svc.Name,
		"purpose", purpose,
		"preconditions", seq(pre...),
		"trigger", strs(b.triggers...),
		"input", flow(mapping()),
	)
	add := func(k string, v *yaml.Node) {
		if v != nil {
			doc.Content = append(doc.Content, str(k), v)
		}
	}
	add("vars", b.vars)
	add("failpoints", func() *yaml.Node {
		if len(b.failpoints) == 0 {
			return nil
		}
		return strs(b.failpoints...)
	}())
	add("sut", b.sut)
	add("chaos", b.chaos)
	add("given", b.given)
	if len(b.steps) > 0 {
		add("steps", seq(b.steps...))
	}
	add("perf", b.perf)
	if len(expect) > 0 {
		add("expect", seq(expect...))
	}
	if len(c.Evidence) > 0 {
		add("evidence", flow(mapping("grafana", strs(c.Evidence...))))
	}
	add("within", str(within))
	if len(muts) > 0 {
		add("mutations", seq(muts...))
	}
	add("generated", flow(mapping("by", "testkit gen", "sources", strs(b.g.opt.Source+"#conformance.patterns."+b.pattern))))

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# Sinh tự động bởi `testkit gen` từ %s (mục conformance, mẫu %s).\n", g.opt.Source, b.pattern)
	buf.WriteString("# Đừng sửa tay: sửa mô tả service rồi chạy lại `testkit gen`. Case ở trạng thái draft;\n")
	buf.WriteString("# `testkit admit` kiểm bằng đột biến, người duyệt mới chạy `testkit admit --approve --by <tên>`.\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	g.plan.Files = append(g.plan.Files, File{Path: g.svc.Name + "/" + b.file + ".yaml", ID: b.id(), Pattern: b.pattern, Trigger: b.trigger, Body: buf.Bytes()})
	return nil
}

func (g *gen) skip(pattern, trigger, reason string) {
	g.plan.Skips = append(g.plan.Skips, Skip{Pattern: pattern, Trigger: trigger, Reason: reason})
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func orDefaultS(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// ---------------------------------------------------------------------------------------
// Patterns

func (g *gen) duplicate(p *config.ConformancePattern) error {
	jobs, copies := orDefault(p.Jobs, 2), orDefault(p.Copies, 3)
	b := g.newBuilder(config.PatDuplicate, "DUP", "duplicate-delivery", g.triggers(p), p)
	b.title = fmt.Sprintf("Job giao trùng (%d lần) — hiệu ứng nghiệp vụ chỉ xảy ra đúng một lần", copies)
	b.risk = "P0"
	b.purpose = fmt.Sprintf("Hàng đợi chỉ đảm bảo at-least-once: cùng một job có thể được giao %d lần (producer gửi lại, consumer chưa kịp ack). "+
		"Kết quả phải như khi giao một lần: trạng thái hoàn tất đúng, các hiệu ứng phụ (trừ tiền, mail, sự kiện...) xảy ra đúng một lần, "+
		"không còn tồn đọng và không job nào bị đẩy vào DLQ.", copies)
	b.pre = []string{fmt.Sprintf("%d job (%s) có dữ liệu nghiệp vụ theo conformance.given", jobs, strings.Join(keys(jobs), ", ")),
		fmt.Sprintf("Mỗi job được giao %d lần liên tiếp qua trigger", copies)}
	if err := b.givenFor(jobs); err != nil {
		return err
	}
	for _, k := range keys(jobs) {
		b.addStep("trigger.enqueue", g.enqueueWith(k, copies), "")
	}
	if err := g.completion(b, jobs, true); err != nil {
		return err
	}
	b.addCheck("backlog", "trigger.backlog", "eq", 0, "Không còn job nào chờ hoặc đang xử lý")
	b.addCheck("dlq", "trigger.dlq", "eq", 0, "Giao trùng không phải lỗi: không job nào vào DLQ")
	return b.build()
}

// enqueueWith builds the with-map of trigger.enqueue for a key.
func (g *gen) enqueueWith(key string, copies int) *yaml.Node {
	kv := []any{"id", key}
	if copies > 1 {
		kv = append(kv, "duplicate", copies)
	}
	fields := make([]string, 0, len(g.c.Job))
	for k := range g.c.Job {
		fields = append(fields, k)
	}
	sort.Strings(fields)
	for _, k := range fields {
		v := g.c.Job[k]
		if s, ok := v.(string); ok {
			v = subStr(s, key, 0)
		}
		kv = append(kv, k, v)
	}
	return mapping(kv...)
}

// completion adds the done + effects expectations of jobs jobs.
func (g *gen) completion(b *builder, jobs int, perKey bool) error {
	if err := b.addItems("done", g.c.Done, jobs, perKey); err != nil {
		return err
	}
	return b.addItems("effects", g.c.Effects, jobs, perKey)
}

func (g *gen) poison(p *config.ConformancePattern) error {
	body := orDefaultS(p.Body, "{not json")
	for _, t := range g.triggers(p) {
		spec := g.svc.Triggers[t]
		var step string
		var with *yaml.Node
		switch t {
		case "kafka":
			step, with = "kafka.produce", mapping("topic", spec.Topic, "key", "k1", "value", body)
		case "rabbitmq":
			step, with = "rabbitmq.publish", mapping("queue", spec.Queue, "body", body)
		case "redis":
			step, with = "redis.enqueue", mapping("queue", spec.Queue, "value", body)
		case "db-poll":
			if p.DBPoll == nil || p.DBPoll.SQL == "" {
				g.skip(config.PatPoison, t, "db-poll không có thân message: khai báo patterns.poison-message.db-poll.sql (câu INSERT một job không dùng được)")
				continue
			}
			step, with = "postgres.exec", mapping("sql", p.DBPoll.SQL)
		}
		b := g.newBuilder(config.PatPoison, "POISON-"+strings.ToUpper(t), "poison-message-"+t, []string{t}, p)
		b.trigger = t
		b.title = fmt.Sprintf("Message độc qua %s vào DLQ, không chặn job hợp lệ phía sau", t)
		b.risk = "P1"
		b.purpose = "Một message không đọc được (hỏng định dạng) đứng trước một job hợp lệ. Message độc không bao giờ xử lý được nên phải bị chuyển sang " +
			"nơi dead-letter đã khai báo (không được mất lặng lẽ, không được thử lại vô tận, không được chặn hàng đợi), còn job hợp lệ phía sau vẫn hoàn tất đúng một lần."
		b.pre = []string{"Một job hợp lệ k1 có dữ liệu nghiệp vụ theo conformance.given", "Message độc được đưa vào hàng đợi trước job hợp lệ"}
		if err := b.givenFor(1); err != nil {
			return err
		}
		b.addStep(step, with, "đưa message độc vào hàng đợi")
		b.addStep("trigger.enqueue", g.enqueueWith("k1", 1), "")
		b.addCheck("poison", "trigger.dlq", "eq", 1, "Message độc nằm ở nơi dead-letter (đúng một)")
		if err := g.completion(b, 1, true); err != nil {
			return err
		}
		b.addCheck("backlog", "trigger.backlog", "eq", 0, "Không còn gì chờ hoặc đang xử lý: message độc không bị thử lại mãi")
		if err := b.build(); err != nil {
			return err
		}
	}
	return nil
}

func (g *gen) crash(p *config.ConformancePattern) error {
	jobs := orDefault(p.Jobs, 1)
	if strings.Contains(p.Log, ")") {
		return fmt.Errorf("log must not contain ')' (it is used inside sut.log(...))")
	}
	b := g.newBuilder(config.PatCrash, "CRASH", "crash-mid-job", g.triggers(p), p)
	b.title = fmt.Sprintf("Tiến trình chết giữa chừng (%s) — job được giao lại, hiệu ứng vẫn đúng một lần", p.Failpoint)
	b.risk = "P0"
	b.purpose = fmt.Sprintf("Tiến trình chết đột ngột ở điểm %q (failpoint, chỉ có trong image test) khi đang xử lý job. Sau khi được khởi động lại, job "+
		"phải được giao lại (offset/ack/lease chưa hoàn tất) và phần việc còn thiếu được hoàn tất; những gì đã xảy ra trước khi chết không được lặp lại.", p.Failpoint)
	b.pre = []string{"Image test (build tag failpoint) chạy với TK_FAILPOINTS=" + p.Failpoint + "=once",
		"Container có restart policy " + orDefaultS(p.Restart, "on-failure:3") + " (mô phỏng orchestrator khởi động lại)",
		fmt.Sprintf("%d job có dữ liệu nghiệp vụ theo conformance.given", jobs)}
	b.failpoints = []string{p.Failpoint + "=once"}
	b.sut = flow(mapping("restart", orDefaultS(p.Restart, "on-failure:3")))
	if err := b.givenFor(jobs); err != nil {
		return err
	}
	for _, k := range keys(jobs) {
		b.addStep("trigger.enqueue", g.enqueueWith(k, 1), "")
	}
	b.addCheck("crash", "sut.log("+p.Log+").count", "eq", 1, "Tiến trình thực sự đã chết đúng chỗ (bằng chứng trong log)")
	b.addCheck("restart", "sut.restarts", "eq", 1, "Container được khởi động lại đúng một lần")
	if err := g.completion(b, jobs, true); err != nil {
		return err
	}
	b.addCheck("backlog", "trigger.backlog", "eq", 0, "Job được giao lại và xử lý xong, không còn tồn đọng")
	b.addCheck("dlq", "trigger.dlq", "eq", 0, "Chết giữa chừng không được biến thành dead-letter")
	b.within = "60s"
	return b.build()
}

// firstProgress returns the first total done item usable as a progress gauge.
func (g *gen) firstProgress() (string, bool) {
	for _, it := range g.c.Done {
		if isTotal(it) {
			if _, ok := it["per_job"]; ok {
				c, _ := it["check"].(string)
				return c, true
			}
		}
	}
	return "", false
}

func (g *gen) fault(p *config.ConformancePattern) error {
	jobs, rate := orDefault(p.Jobs, 100), orDefault(p.Rate, 10)
	progress, ok := g.firstProgress()
	if !ok {
		return fmt.Errorf("needs a total in conformance.done with per_job (e.g. { check: \"postgres.order.count(status=paid)\", per_job: 1 }) to know the load is flowing")
	}
	maxRec := orDefaultS(p.MaxRecovery, "30s")
	for _, f := range p.Faults {
		hold := orDefaultS(f.For, "5s")
		suffix := strings.ToUpper(f.Proxy) + "-" + strings.ToUpper(strings.ReplaceAll(f.Fault, "_", "-"))
		b := g.newBuilder(config.PatFault, "FAULT-"+suffix, "dependency-fault-"+f.Proxy+"-"+strings.ReplaceAll(f.Fault, "_", "-"), g.triggers(p), p)
		b.title = fmt.Sprintf("Sự cố %s của %s trong %s dưới tải %d job/s — không mất job, không vào DLQ, phục hồi ≤ %s", f.Fault, f.Proxy, hold, rate, maxRec)
		b.risk = "P0"
		b.purpose = fmt.Sprintf("Thí nghiệm chaos theo quy trình: tải nền cố định %d job/s (open model, %d job) → trạng thái ổn định → gây lỗi %q lên phụ thuộc %q "+
			"(chỉ namespace này, qua Toxiproxy) trong %s → gỡ lỗi → đo thời gian về trạng thái ổn định. "+
			"Một sự cố hạ tầng ngắn không được biến thành mất job, xử lý trùng hay dead-letter.", rate, jobs, f.Fault, f.Proxy, hold)
		b.pre = []string{fmt.Sprintf("%d job; dữ liệu nghiệp vụ tạo bằng perf.fixture của service", jobs),
			"Service kết nối tới " + f.Proxy + " qua proxy Toxiproxy riêng của execution (chaos.proxies)",
			"Trạng thái ổn định = trigger.backlog ≤ 5"}
		if len(f.Mutations) > 0 {
			b.mutations = f.Mutations
		}
		b.chaos = flow(mapping("proxies", strs(f.Proxy), "max_recovery", maxRec))
		if err := b.givenShared(); err != nil {
			return err
		}
		fixture := g.svc.Perf.Fixture
		fixture = strings.NewReplacer("{{ .prefix }}", "S", "{{.prefix}}", "S", "{{ .from }}", "1", "{{.from}}", "1", "{{ .to }}", fmt.Sprint(jobs), "{{.to}}", fmt.Sprint(jobs)).Replace(fixture)
		b.addStep("postgres.exec", mapping("sql", fixture), "tạo dữ liệu của các job")
		b.addStep("load.start", mapping("rate", rate, "from", 1, "to", jobs, "id_prefix", "S"), "")
		b.addStep("wait.until", mapping("check", progress, "gte", max(1, min(20, jobs/5)), "within", "60s"), "trạng thái ổn định trước khi gây lỗi")
		with := mapping("proxy", f.Proxy)
		keysSorted := make([]string, 0, len(f.With))
		for k := range f.With {
			keysSorted = append(keysSorted, k)
		}
		sort.Strings(keysSorted)
		for _, k := range keysSorted {
			with.Content = append(with.Content, str(k), val(f.With[k]))
		}
		b.addStep("chaos."+f.Fault, with, "")
		// Only a blast-radius condition stops the experiment. A dead-lettered job is the property
		// under test (assertion `dlq`): stopping on it would turn a product failure into an
		// aborted, "environment" result and hide the verdict.
		holdWith := mapping("for", hold)
		if p.AbortBacklog > 0 {
			holdWith.Content = append(holdWith.Content, str("abort_if"), seq(flow(mapping("check", "trigger.backlog", "gt", p.AbortBacklog,
				"why", fmt.Sprintf("blast radius: dừng nếu tồn đọng vượt %d job", p.AbortBacklog)))))
		}
		b.addStep("chaos.hold", holdWith, "")
		b.addStep("chaos.clear", nil, "")
		b.addStep("chaos.recover", mapping("within", "60s", "expect", seq(flow(mapping("check", "trigger.backlog", "lte", 5, "why", "tồn đọng về lại mức ổn định")))), "")
		b.addStep("load.wait", mapping("timeout", "120s"), "")
		if err := g.completion(b, jobs, false); err != nil {
			return err
		}
		b.addCheck("dlq", "trigger.dlq", "eq", 0, "Sự cố hạ tầng tạm thời không đẩy job vào DLQ")
		b.addCheck("backlog", "trigger.backlog", "eq", 0, "Không còn job nào chờ hoặc đang xử lý")
		b.addCheck("recovery", "experiment.recovery_seconds", "lte", durSeconds(maxRec), "Phục hồi trong thời gian cho phép")
		if p.AbortBacklog > 0 {
			b.addCheck("abort", "experiment.aborted", "eq", false, "Lỗi nằm trong giới hạn cho phép (không chạm điều kiện dừng khẩn cấp)")
		}
		b.addCheck("load", "experiment.load.late", "eq", 0, "Máy tạo tải không bị nghẽn (mọi job được phát đúng nhịp)")
		b.reconcile()
		b.within = fmt.Sprintf("%ds", jobs/rate+durSeconds(hold)+150)
		if err := b.build(); err != nil {
			return err
		}
	}
	return nil
}

// givenShared sets the given of a case that has no per-job rows (big cases create their data with perf.fixture).
func (b *builder) givenShared() error {
	out := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if sh := b.g.c.Shared; sh.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(sh.Content); i += 2 {
			out.Content = append(out.Content, str(sh.Content[i].Value), flow(clone(sh.Content[i+1])))
		}
	}
	if len(out.Content) > 0 {
		b.given = out
	}
	return nil
}

func durSeconds(s string) int {
	var n int
	var unit string
	if _, err := fmt.Sscanf(s, "%d%s", &n, &unit); err != nil {
		return 30
	}
	switch unit {
	case "m":
		return n * 60
	case "ms":
		return max(1, n/1000)
	}
	return n
}

func (g *gen) order(p *config.ConformancePattern) error {
	jobs := orDefault(p.Jobs, 3)
	b := g.newBuilder(config.PatOrder, "ORDER", "out-of-order", g.triggers(p), p)
	b.title = "Job tới ngược thứ tự và bị phát lại muộn — kết quả không đổi, hiệu ứng đúng một lần"
	b.risk = "P1"
	b.purpose = fmt.Sprintf("%d job được giao theo thứ tự ngược với lúc tạo (%s), rồi job đầu tiên bị phát lại muộn sau khi mọi job đã hoàn tất. "+
		"Kết quả không được phụ thuộc thứ tự tới, và job tới muộn cho việc đã xong không được lặp lại hiệu ứng nào.", jobs, strings.Join(reversed(keys(jobs)), " → "))
	b.pre = []string{fmt.Sprintf("%d job (%s) có dữ liệu nghiệp vụ theo conformance.given", jobs, strings.Join(keys(jobs), ", ")),
		"Giao theo thứ tự ngược, đợi xong hết, rồi giao lại k1"}
	if err := b.givenFor(jobs); err != nil {
		return err
	}
	for _, k := range reversed(keys(jobs)) {
		b.addStep("trigger.enqueue", g.enqueueWith(k, 1), "")
	}
	b.addStep("trigger.drain", nil, "đợi mọi job hoàn tất")
	b.addStep("trigger.enqueue", g.enqueueWith("k1", 1), "phát lại muộn job k1 (đã xong)")
	if err := g.completion(b, jobs, true); err != nil {
		return err
	}
	b.addCheck("backlog", "trigger.backlog", "eq", 0, "Không còn job nào chờ hoặc đang xử lý")
	b.addCheck("dlq", "trigger.dlq", "eq", 0, "Không job nào vào DLQ")
	return b.build()
}

func reversed(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[len(in)-1-i] = s
	}
	return out
}

func (g *gen) load(p *config.ConformancePattern) error {
	rate := orDefault(p.Rate, 20)
	dur, warm, repeat := orDefaultS(p.Duration, "20s"), orDefaultS(p.Warmup, "5s"), orDefault(p.Repeat, 3)
	base := orDefaultS(p.Baseline, fmt.Sprintf("std-%s-%drps", g.svc.Name, rate))
	for _, t := range g.triggers(p) {
		b := g.newBuilder(config.PatLoad, "LOAD-"+strings.ToUpper(t), "steady-load-"+t, []string{t}, p)
		b.trigger = t
		b.title = fmt.Sprintf("Tải ổn định %d job/s qua %s — SLO, thông lượng và so với baseline", rate, t)
		b.risk = "P1"
		b.purpose = fmt.Sprintf("Đo hiệu năng ở tải cố định %d job/s qua %s (open model: job tới đúng nhịp bất kể service nhanh hay chậm), sau %s warm-up, "+
			"%d lần lặp để so sánh thống kê với baseline cùng môi trường. Độ trễ đầu-cuối = lúc job hoàn tất − lúc máy tạo tải phát job (perf.completion). "+
			"Ngưỡng SLO do đội service đặt trong mô tả; baseline do người ghi trên máy rảnh (`testkit baseline record`).", rate, t, warm, repeat)
		b.pre = []string{"Máy tạo tải (TestKit runner) không bị nghẽn (đo bằng số job phát trễ)", "Chạy một mình trên stack (case hiệu năng không chạy song song)",
			"Baseline của khoá " + base + "-" + t + " đã được ghi (nếu chưa: `testkit baseline record <case>`)"}
		if err := b.givenShared(); err != nil {
			return err
		}
		b.perf = mapping(
			"kind", "load",
			"executor", "trigger",
			"warmup", warm,
			"stages", flow(seq(mapping("rate", rate, "duration", dur))),
			"repeat", repeat,
			"id_prefix", "SL",
			"thresholds", flow(val(p.Thresholds)),
			"baseline", flow(mapping("key", base+"-"+t, "max_regression", "10%")),
		)
		b.addCheck("dlq", "trigger.dlq", "eq", 0, "Không job nào bị đẩy vào DLQ dưới tải")
		b.reconcile()
		b.within = "90s"
		if err := b.build(); err != nil {
			return err
		}
	}
	return nil
}

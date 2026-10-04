// Package toxiproxy is the chaos connector ("chaos"). Network faults go
// through Toxiproxy: every dependency a case routes through a proxy gets its
// own proxy per execution namespace, on a port Toxiproxy allocates, so
// parallel executions never share a fault. Process/resource faults act on
// the containers of the service under test through the Docker CLI
// (pause, kill, network disconnect, CPU/memory/disk stress, tc netem).
package toxiproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tuannm99/testkit/testkit/core/infra"
	"github.com/tuannm99/testkit/testkit/core/kit"
)

type Connector struct {
	env     *kit.Env
	base    string
	http    *http.Client
	d       *infra.Docker
	proxies map[string]string // logical name -> toxiproxy proxy name
	netem   []string          // containers with a netem qdisc to remove
	log     []map[string]any  // every fault applied (evidence)
}

func New() kit.Connector {
	return &Connector{http: &http.Client{Timeout: 10 * time.Second}, d: infra.NewDocker(nil), proxies: map[string]string{}}
}

func (c *Connector) Name() string { return "chaos" }

func (c *Connector) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("toxiproxy %s %s: %w (is the chaos profile up? testkit up --profile ...,chaos)", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("toxiproxy %s %s: HTTP %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(raw))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// Provision creates the proxies requested by the case and publishes their
// addresses to the env, so the service under test is started through them.
func (c *Connector) Provision(ctx context.Context, env *kit.Env) error {
	c.env = env
	c.base = env.Runner.Toxiproxy
	if env.ProxyAddr == nil {
		env.ProxyAddr = map[string]string{}
	}
	for _, name := range env.Proxies {
		spec, ok := env.Service.Chaos.Proxies[name]
		if !ok {
			return fmt.Errorf("proxy %q is not declared in chaos.proxies of service %s", name, env.Service.Name)
		}
		pname := string(env.NS) + "_" + name
		var p struct {
			Listen string `json:"listen"`
		}
		if err := c.do(ctx, http.MethodPost, "/proxies", map[string]any{"name": pname, "listen": "0.0.0.0:0",
			"upstream": spec.Upstream, "enabled": true}, &p); err != nil {
			return err
		}
		port := p.Listen[strings.LastIndex(p.Listen, ":")+1:]
		c.proxies[name] = pname
		env.ProxyAddr[name] = "toxiproxy:" + port
	}
	return nil
}

func (c *Connector) Health(ctx context.Context) error {
	if len(c.proxies) == 0 {
		return nil
	}
	return c.do(ctx, http.MethodGet, "/version", nil, nil)
}

func (c *Connector) proxy(with map[string]any) (string, error) {
	name := kit.Str(with, "proxy")
	p, ok := c.proxies[name]
	if !ok {
		return "", fmt.Errorf("proxy %q is not routed for this case (add it to chaos.proxies of the case)", name)
	}
	return p, nil
}

func (c *Connector) toxic(ctx context.Context, s kit.Step, typ string, attrs map[string]any) (kit.Result, error) {
	p, err := c.proxy(s.With)
	if err != nil {
		return kit.Result{}, err
	}
	stream := kit.Str(s.With, "stream")
	if stream == "" {
		stream = "downstream"
	}
	name := typ + "_" + stream
	toxicity := 1.0
	if t := kit.Str(s.With, "toxicity"); t != "" {
		toxicity, _ = strconv.ParseFloat(t, 64)
	}
	body := map[string]any{"name": name, "type": typ, "stream": stream, "toxicity": toxicity, "attributes": attrs}
	err = c.do(ctx, http.MethodPost, "/proxies/"+p+"/toxics", body, nil)
	c.note(s.Name, map[string]any{"proxy": kit.Str(s.With, "proxy"), "toxic": body})
	return kit.Result{Output: body, Note: fmt.Sprintf("%s on %s: %v", typ, kit.Str(s.With, "proxy"), attrs)}, err
}

func (c *Connector) note(step string, detail map[string]any) {
	detail["step"], detail["at"] = step, time.Now().UTC()
	c.log = append(c.log, detail)
}

func ms(with map[string]any, k string, def int) int { return kit.Int(with, k, def) }

// Apply runs one chaos step.
func (c *Connector) Apply(ctx context.Context, s kit.Step) (kit.Result, error) {
	switch s.Name {
	case "chaos.latency":
		return c.toxic(ctx, s, "latency", map[string]any{"latency": ms(s.With, "latency", 1000), "jitter": ms(s.With, "jitter", 0)})
	case "chaos.timeout":
		// Data stops flowing; the connection is closed after `timeout` ms (0 = never).
		return c.toxic(ctx, s, "timeout", map[string]any{"timeout": ms(s.With, "timeout", 0)})
	case "chaos.reset_peer":
		return c.toxic(ctx, s, "reset_peer", map[string]any{"timeout": ms(s.With, "timeout", 0)})
	case "chaos.bandwidth":
		return c.toxic(ctx, s, "bandwidth", map[string]any{"rate": ms(s.With, "rate", 10)})
	case "chaos.slicer":
		return c.toxic(ctx, s, "slicer", map[string]any{"average_size": ms(s.With, "size", 8), "size_variation": 4, "delay": ms(s.With, "delay", 10)})
	case "chaos.down", "chaos.up":
		p, err := c.proxy(s.With)
		if err != nil {
			return kit.Result{}, err
		}
		enabled := s.Name == "chaos.up"
		c.note(s.Name, map[string]any{"proxy": kit.Str(s.With, "proxy"), "enabled": enabled})
		return kit.Result{Note: fmt.Sprintf("proxy %s enabled=%v", kit.Str(s.With, "proxy"), enabled)},
			c.do(ctx, http.MethodPost, "/proxies/"+p, map[string]any{"enabled": enabled}, nil)
	case "chaos.clear":
		return kit.Result{Note: "all faults removed"}, c.ClearAll(ctx)
	case "chaos.container":
		return c.container(ctx, s)
	case "chaos.network":
		return c.network(ctx, s)
	case "chaos.stress":
		return c.stress(ctx, s)
	case "chaos.netem":
		return c.netemApply(ctx, s)
	}
	return kit.Result{}, fmt.Errorf("chaos: unknown step %s", s.Name)
}

// ClearAll removes every toxic, re-enables every proxy and removes netem.
func (c *Connector) ClearAll(ctx context.Context) error {
	var first error
	names := make([]string, 0, len(c.proxies))
	for n := range c.proxies {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := c.proxies[n]
		var px struct {
			Toxics []struct {
				Name string `json:"name"`
			} `json:"toxics"`
		}
		if err := c.do(ctx, http.MethodGet, "/proxies/"+p, nil, &px); err != nil {
			first = errOr(first, err)
			continue
		}
		for _, t := range px.Toxics {
			first = errOr(first, c.do(ctx, http.MethodDelete, "/proxies/"+p+"/toxics/"+t.Name, nil, nil))
		}
		first = errOr(first, c.do(ctx, http.MethodPost, "/proxies/"+p, map[string]any{"enabled": true}, nil))
	}
	for _, ct := range c.netem {
		_, err := c.d.Run(ctx, "run", "--rm", "--net", "container:"+ct, "--cap-add", "NET_ADMIN",
			"--label", infra.LabelManaged+"=true", "--label", infra.LabelProject+"="+c.env.Project.Name,
			c.env.Project.Get("NETTOOLS_IMAGE"), "tc", "qdisc", "del", "dev", "eth0", "root")
		first = errOr(first, err)
	}
	c.netem = nil
	c.note("chaos.clear", map[string]any{})
	return first
}

func errOr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// sutContainers lists the containers of the service under test (or one replica).
func (c *Connector) sutContainers(with map[string]any) []string {
	n := max(c.env.Replicas, 1)
	if r := kit.Int(with, "replica", 0); r > 0 {
		return []string{c.env.NS.Container(c.env.Service.Name, r-1, n)}
	}
	var out []string
	for i := 0; i < n; i++ {
		out = append(out, c.env.NS.Container(c.env.Service.Name, i, n))
	}
	return out
}

// container: {target: sut | <compose service>, action: pause|unpause|stop|start|restart|kill}.
// Shared infrastructure (postgres, kafka, ...) affects every parallel
// execution: such cases are run alone (exclusive).
func (c *Connector) container(ctx context.Context, s kit.Step) (kit.Result, error) {
	action := kit.Str(s.With, "action")
	switch action {
	case "pause", "unpause", "stop", "start", "restart", "kill":
	default:
		return kit.Result{}, fmt.Errorf("chaos.container: action %q (pause|unpause|stop|start|restart|kill)", action)
	}
	target := kit.Str(s.With, "target")
	var names []string
	if target == "" || target == "sut" {
		names = c.sutContainers(s.With)
	} else {
		out, err := c.d.Run(ctx, "ps", "-aq", "--filter", "label=com.docker.compose.project="+c.env.Project.Name,
			"--filter", "label=com.docker.compose.service="+target)
		if err != nil || out == "" {
			return kit.Result{}, fmt.Errorf("chaos.container: no running %s container (%v)", target, err)
		}
		names = infra.Lines(out)
	}
	args := append([]string{action}, names...)
	if action == "stop" || action == "restart" {
		args = append([]string{action, "-t", strconv.Itoa(kit.Int(s.With, "timeout", 10))}, names...)
	}
	_, err := c.d.Run(ctx, args...)
	c.note(s.Name, map[string]any{"target": target, "action": action, "containers": names})
	return kit.Result{Note: fmt.Sprintf("%s %s", action, strings.Join(names, ", "))}, err
}

// network: {action: disconnect|connect} the service under test from the
// TestKit network (every dependency and DNS become unreachable).
func (c *Connector) network(ctx context.Context, s kit.Step) (kit.Result, error) {
	action := kit.Str(s.With, "action")
	if action != "disconnect" && action != "connect" {
		return kit.Result{}, fmt.Errorf("chaos.network: action must be disconnect|connect")
	}
	names := c.sutContainers(s.With)
	for _, n := range names {
		args := []string{"network", action, c.env.Project.Network, n}
		if action == "connect" {
			args = []string{"network", "connect", "--alias", n, c.env.Project.Network, n}
		}
		if _, err := c.d.Run(ctx, args...); err != nil {
			return kit.Result{}, err
		}
	}
	c.note(s.Name, map[string]any{"action": action, "containers": names})
	return kit.Result{Note: fmt.Sprintf("network %s %s", action, strings.Join(names, ", "))}, nil
}

// stress: {resource: cpu|memory|disk, workers, mb, duration} inside the
// containers of the service under test (busybox tools of the image).
func (c *Connector) stress(ctx context.Context, s kit.Step) (kit.Result, error) {
	d := kit.Dur(s.With, "duration", 10*time.Second)
	secs := int(d.Seconds())
	var script string
	switch kit.Str(s.With, "resource") {
	case "cpu", "":
		workers := kit.Int(s.With, "workers", 1)
		script = fmt.Sprintf(`for i in $(seq %d); do (end=$(($(date +%%s)+%d)); while [ $(date +%%s) -lt $end ]; do :; done) & done; wait`, workers, secs)
	case "memory":
		script = fmt.Sprintf(`head -c %dM /dev/zero | (timeout %d tail; true)`, kit.Int(s.With, "mb", 256), secs)
	case "disk":
		script = fmt.Sprintf(`dd if=/dev/zero of=/tmp/testkit-fill bs=1M count=%d 2>/dev/null; sleep %d; rm -f /tmp/testkit-fill`, kit.Int(s.With, "mb", 512), secs)
	default:
		return kit.Result{}, fmt.Errorf("chaos.stress: resource cpu|memory|disk")
	}
	names := c.sutContainers(s.With)
	for _, n := range names {
		if _, err := c.d.Run(ctx, "exec", "-d", n, "sh", "-c", script); err != nil {
			return kit.Result{}, err
		}
	}
	c.note(s.Name, map[string]any{"resource": kit.Str(s.With, "resource"), "duration": d.String(), "containers": names})
	return kit.Result{Note: fmt.Sprintf("%s stress for %s in %s", kit.Str(s.With, "resource"), d, strings.Join(names, ", "))}, nil
}

// netem: {delay: 200ms, jitter, loss: 10} with tc in the network namespace
// of the service under test. Needs CAP_NET_ADMIN (step declares it).
func (c *Connector) netemApply(ctx context.Context, s kit.Step) (kit.Result, error) {
	args := []string{"qdisc", "add", "dev", "eth0", "root", "netem"}
	if d := kit.Str(s.With, "delay"); d != "" {
		args = append(args, "delay", d)
		if j := kit.Str(s.With, "jitter"); j != "" {
			args = append(args, j)
		}
	}
	if l := kit.Str(s.With, "loss"); l != "" {
		args = append(args, "loss", l+"%")
	}
	names := c.sutContainers(s.With)
	for _, n := range names {
		run := append([]string{"run", "--rm", "--net", "container:" + n, "--cap-add", "NET_ADMIN",
			"--label", infra.LabelManaged + "=true", "--label", infra.LabelProject + "=" + c.env.Project.Name,
			c.env.Project.Get("NETTOOLS_IMAGE"), "tc"}, args...)
		if _, err := c.d.Run(ctx, run...); err != nil {
			return kit.Result{}, err
		}
		c.netem = append(c.netem, n)
	}
	c.note(s.Name, map[string]any{"tc": strings.Join(args, " "), "containers": names})
	return kit.Result{Note: "tc " + strings.Join(args, " ")}, nil
}

// Collect writes the fault log and the final proxy state.
func (c *Connector) Collect(ctx context.Context, _ kit.TimeWindow) ([]kit.Artifact, error) {
	if c.env == nil || (len(c.log) == 0 && len(c.proxies) == 0) {
		return nil, nil
	}
	state := map[string]any{}
	for n, p := range c.proxies {
		var px any
		if err := c.do(ctx, http.MethodGet, "/proxies/"+p, nil, &px); err == nil {
			state[n] = px
		}
	}
	rel := c.env.CaseDir + "/output/chaos/faults.json"
	if _, err := c.env.Evidence.WriteJSON(rel, map[string]any{"faults": c.log, "proxies": state, "addresses": c.env.ProxyAddr}); err != nil {
		return nil, err
	}
	return []kit.Artifact{{Kind: "chaos", Path: rel, Title: fmt.Sprintf("Faults injected (%d) and proxy state", len(c.log)), Source: "chaos"}}, nil
}

// Teardown clears faults and deletes the proxies of the execution.
func (c *Connector) Teardown(ctx context.Context) error {
	if c.env == nil {
		return nil
	}
	err := c.ClearAll(ctx)
	for _, p := range c.proxies {
		err = errOr(err, c.do(ctx, http.MethodDelete, "/proxies/"+p, nil, nil))
	}
	return err
}

var _ kit.Connector = (*Connector)(nil)

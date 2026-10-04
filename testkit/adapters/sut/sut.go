// Package sut runs the service under test as labelled containers on the
// TestKit network ("sut" connector). The env of the service is rendered from
// its descriptor for the execution namespace, so every execution gets its
// own database, topics, consumer group, index and mock base URL.
package sut

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	httpmock "github.com/tuannm99/testkit/testkit/adapters/mock/http"
	"github.com/tuannm99/testkit/testkit/core/infra"
	"github.com/tuannm99/testkit/testkit/core/kit"
)

type Connector struct {
	env      *kit.Env
	d        *infra.Docker
	names    []string
	image    string
	imageID  string
	healthOf map[string]string // container -> health URL
	http     *http.Client
}

func New() kit.Connector {
	return &Connector{d: infra.NewDocker(nil), healthOf: map[string]string{}, http: &http.Client{Timeout: 3 * time.Second}}
}

func (c *Connector) Name() string            { return "sut" }
func (c *Connector) CheckPrefixes() []string { return []string{"sut"} }
func (c *Connector) Image() (string, string) { return c.image, c.imageID }

// Funcs are the template functions available in the descriptor's env.
func Funcs(env *kit.Env) template.FuncMap {
	in := env.Internal
	ns := env.NS
	return template.FuncMap{
		"pgdsn":     func() string { return in.Postgres.DSN(ns.Database()) },
		"brokers":   func() string { return strings.Join(in.KafkaBrokers, ",") },
		"topic":     ns.Topic,
		"group":     ns.Group,
		"index":     ns.Index,
		"keyprefix": ns.KeyPrefix,
		"mock":      func(name string) string { return httpmock.DataURL(in.Mockhub, string(ns), name) },
		"smtp":      func() string { return in.MailpitSMTP },
		"es":        func() string { return in.Elasticsearch },
		"ch":        func() string { return in.ClickHouse },
		"chuser":    func() string { return in.CHUser },
		"chpass":    func() string { return in.CHPassword },
		"mongo":     func() string { return in.Mongo },
		"redis":     func() string { return in.Redis },
		"otlp":      func() string { return in.OTLP },
		"database":  ns.Database,
		"mocksecret": func(name string) (string, error) {
			m, ok := env.Service.Mocks[name]
			if !ok {
				return "", fmt.Errorf("no mock %q", name)
			}
			return m.Secret, nil
		},
		"mocksocket": func(name string) string {
			return fmt.Sprintf("ws://%s/ns/%s/%s", in.MockhubSocket, ns, name)
		},
	}
}

// RenderEnv renders the descriptor env (+ extra env) for the execution.
func RenderEnv(env *kit.Env) (map[string]string, error) {
	data := map[string]any{"NS": string(env.NS), "RunID": env.RunID, "Vars": env.Vars, "Case": env.CaseID, "Trigger": env.Trigger}
	out := map[string]string{}
	for k, v := range env.Service.Env {
		t, err := template.New(k).Option("missingkey=error").Funcs(Funcs(env)).Parse(v)
		if err != nil {
			return nil, fmt.Errorf("env %s: %w", k, err)
		}
		var b strings.Builder
		if err := t.Execute(&b, data); err != nil {
			return nil, fmt.Errorf("env %s: %w", k, err)
		}
		out[k] = b.String()
	}
	for k, v := range env.ExtraEnv {
		out[k] = v
	}
	if len(env.Failpoint) > 0 {
		out["TK_FAILPOINTS"] = strings.Join(env.Failpoint, ";")
	}
	if _, ok := out["OTEL_RESOURCE_ATTRIBUTES"]; !ok {
		out["OTEL_RESOURCE_ATTRIBUTES"] = fmt.Sprintf("testkit.run_id=%s,testkit.ns=%s,service.name=%s", env.RunID, env.NS, env.Service.Name)
	}
	return out, nil
}

func (c *Connector) containerName(i int) string {
	return c.env.NS.Container(c.env.Service.Name, i, max(c.env.Replicas, 1))
}

func (c *Connector) Provision(ctx context.Context, env *kit.Env) error {
	c.env = env
	svc := env.Service
	c.image = svc.ImageRef(env.TestImage)
	if env.TestImage && svc.Image.TestTag == "" {
		return fmt.Errorf("failpoints requested but service %s declares no test image (image.test_tag)", svc.Name)
	}
	id, err := c.d.Run(ctx, "image", "inspect", "--format", "{{.Id}}", c.image)
	if err != nil {
		return fmt.Errorf("image %s not found: run `testkit up --services %s` (builds it) or pull it", c.image, svc.Name)
	}
	c.imageID = id
	vars, err := RenderEnv(env)
	if err != nil {
		return err
	}
	replicas := max(env.Replicas, 1)
	for i := 0; i < replicas; i++ {
		name := c.containerName(i)
		args := []string{"run", "-d", "--name", name, "--network", env.Project.Network, "--hostname", name,
			"--label", infra.LabelManaged + "=true", "--label", infra.LabelProject + "=" + env.Project.Name,
			"--label", infra.LabelRunID + "=" + env.RunID, "--label", "testkit.ns=" + string(env.NS),
			"--label", infra.LabelService + "=" + svc.Name, "--label", infra.LabelCase + "=" + env.CaseID}
		if env.Trigger != "" {
			args = append(args, "--label", "testkit.trigger="+env.Trigger)
		}
		if svc.Metrics.Path != "" {
			args = append(args, "--label", "testkit.metrics=true",
				"--label", "testkit.metrics_port="+strconv.Itoa(svc.Ports[svc.Metrics.Port]),
				"--label", "testkit.metrics_path="+svc.Metrics.Path)
		}
		if env.Restart != "" {
			args = append(args, "--restart", env.Restart)
		}
		hp := 0
		if svc.Health.Path != "" {
			hp = svc.Ports[svc.Health.Port]
			if !env.Runner.InNetwork {
				args = append(args, "-p", fmt.Sprintf("127.0.0.1::%d", hp))
			}
		}
		keys := make([]string, 0, len(vars))
		for k := range vars {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "-e", k+"="+vars[k])
		}
		args = append(args, c.image)
		if _, err := c.d.Run(ctx, args...); err != nil {
			return err
		}
		c.names = append(c.names, name)
		if hp > 0 {
			c.healthOf[name] = svc.Health.Path
		}
	}
	return c.waitHealthy(ctx, c.names)
}

// baseURL is the address of a container port as seen by the runner. From the
// host it is the published port, which docker reassigns on every start.
func (c *Connector) baseURL(ctx context.Context, name string, port int) (string, error) {
	if c.env.Runner.InNetwork {
		return fmt.Sprintf("http://%s:%d", name, port), nil
	}
	out, err := c.d.Run(ctx, "port", name, strconv.Itoa(port))
	if err != nil {
		return "", err
	}
	return "http://" + strings.TrimSpace(strings.Split(out, "\n")[0]), nil
}

// waitHealthy polls the health endpoint of each container (no sleep-based wait).
func (c *Connector) waitHealthy(ctx context.Context, names []string) error {
	timeout := c.env.Service.Health.Timeout.D()
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	deadline := time.Now().Add(timeout)
	svc := c.env.Service
	for _, n := range names {
		hpath, ok := c.healthOf[n]
		if !ok {
			continue
		}
		url := ""
		for {
			state, _ := c.d.Run(ctx, "inspect", "-f", "{{.State.Status}} {{.State.ExitCode}}", n)
			if strings.HasPrefix(state, "exited") || strings.HasPrefix(state, "dead") {
				logs, _ := c.d.Run(ctx, "logs", "--tail", "20", n)
				return fmt.Errorf("%s exited before becoming healthy (%s): %s", n, state, lastLine(logs))
			}
			if base, err := c.baseURL(ctx, n, svc.Ports[svc.Health.Port]); err == nil && strings.HasPrefix(state, "running") {
				url = base + hpath
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
				resp, err := c.http.Do(req)
				if err == nil {
					resp.Body.Close()
					if resp.StatusCode == 200 {
						break
					}
				}
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s not healthy within %s (%s)", n, timeout, url)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
	return nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

func (c *Connector) Health(ctx context.Context) error { return c.waitHealthy(ctx, c.names) }

func (c *Connector) targets(with map[string]any) []string {
	if r := kit.Int(with, "replica", 0); r > 0 && r <= len(c.names) {
		return []string{c.names[r-1]}
	}
	return c.names
}

func (c *Connector) Apply(ctx context.Context, s kit.Step) (kit.Result, error) {
	ts := c.targets(s.With)
	switch s.Name {
	case "sut.restart":
		for _, n := range ts {
			if _, err := c.d.Run(ctx, "restart", "-t", strconv.Itoa(kit.Int(s.With, "timeout", 10)), n); err != nil {
				return kit.Result{}, err
			}
		}
		return kit.Result{Note: "restarted " + strings.Join(ts, ", ")}, c.waitHealthy(ctx, ts)
	case "sut.stop":
		// Graceful: SIGTERM then SIGKILL after timeout.
		for _, n := range ts {
			if _, err := c.d.Run(ctx, "stop", "-t", strconv.Itoa(kit.Int(s.With, "timeout", 30)), n); err != nil {
				return kit.Result{}, err
			}
		}
		return kit.Result{Note: "stopped (SIGTERM) " + strings.Join(ts, ", ")}, nil
	case "sut.kill":
		sig := kit.Str(s.With, "signal")
		if sig == "" {
			sig = "KILL"
		}
		for _, n := range ts {
			if _, err := c.d.Run(ctx, "kill", "-s", sig, n); err != nil {
				return kit.Result{}, err
			}
		}
		return kit.Result{Note: fmt.Sprintf("sent SIG%s to %s", sig, strings.Join(ts, ", "))}, nil
	case "sut.start":
		for _, n := range ts {
			if _, err := c.d.Run(ctx, "start", n); err != nil {
				return kit.Result{}, err
			}
		}
		return kit.Result{Note: "started " + strings.Join(ts, ", ")}, c.waitHealthy(ctx, ts)
	case "sut.pause", "sut.unpause":
		verb := strings.TrimPrefix(s.Name, "sut.")
		for _, n := range ts {
			if _, err := c.d.Run(ctx, verb, n); err != nil {
				return kit.Result{}, err
			}
		}
		return kit.Result{Note: verb + "d " + strings.Join(ts, ", ")}, nil
	case "sut.wait_healthy":
		return kit.Result{}, c.waitHealthy(ctx, ts)
	}
	return kit.Result{}, fmt.Errorf("sut: unknown step %s", s.Name)
}

// Check resolves:
//
//	sut.restarts                         restarts of all instances (docker RestartCount)
//	sut.running                          number of running instances
//	sut.exit_code                        exit code of the first stopped instance
//	sut.log(<text>).count                log lines containing text (all instances)
//	sut.metric(<name>, label=value)      sum of a Prometheus metric scraped from the instances
func (c *Connector) Check(ctx context.Context, ref kit.CheckRef) (kit.Observation, error) {
	segs := ref.Segments
	at := time.Now().UTC()
	switch segs[1].Name {
	case "restarts", "running", "exit_code":
		total := 0
		exit := 0
		for _, n := range c.names {
			out, err := c.d.Run(ctx, "inspect", "-f", "{{.RestartCount}} {{.State.Running}} {{.State.ExitCode}}", n)
			if err != nil {
				return kit.Observation{At: at}, err
			}
			f := strings.Fields(out)
			switch segs[1].Name {
			case "restarts":
				r, _ := strconv.Atoi(f[0])
				total += r
			case "running":
				if f[1] == "true" {
					total++
				}
			case "exit_code":
				if f[1] != "true" && exit == 0 {
					exit, _ = strconv.Atoi(f[2])
				}
			}
		}
		if segs[1].Name == "exit_code" {
			total = exit
		}
		return kit.Observation{Value: total, Source: "docker inspect " + strings.Join(c.names, " "), At: time.Now().UTC()}, nil
	case "log":
		if len(segs[1].Args) == 0 || len(segs) < 3 {
			return kit.Observation{At: at}, fmt.Errorf("expected sut.log(<text>).count")
		}
		text := strings.Join(segs[1].Args, ",")
		n := 0
		for _, name := range c.names {
			out, _ := c.d.Run(ctx, "logs", name)
			n += strings.Count(out, text)
		}
		return kit.Observation{Value: n, Source: fmt.Sprintf("occurrences of %q in the logs of %s", text, strings.Join(c.names, ", ")), At: time.Now().UTC()}, nil
	case "metric":
		if len(segs[1].Args) == 0 {
			return kit.Observation{At: at}, fmt.Errorf("expected sut.metric(<name>, label=value)")
		}
		v, src, err := c.scrape(ctx, segs[1].Args[0], segs[1].KV)
		return kit.Observation{Value: v, Source: src, At: time.Now().UTC()}, err
	}
	return kit.Observation{At: at}, fmt.Errorf("unknown sut check %q", segs[1].Name)
}

var sampleRe = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{([^}]*)\})?\s+(\S+)`)

// scrape sums a metric over every instance's /metrics.
func (c *Connector) scrape(ctx context.Context, metric string, labels map[string]string) (float64, string, error) {
	svc := c.env.Service
	port := svc.Ports[svc.Metrics.Port]
	sum := 0.0
	var urls []string
	for _, n := range c.names {
		base, err := c.baseURL(ctx, n, port)
		if err != nil {
			return 0, "", err
		}
		url := base + svc.Metrics.Path
		urls = append(urls, url)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := c.http.Do(req)
		if err != nil {
			return 0, "", err
		}
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			m := sampleRe.FindStringSubmatch(sc.Text())
			if m == nil || m[1] != metric || !labelsMatch(m[3], labels) {
				continue
			}
			f, err := strconv.ParseFloat(m[4], 64)
			if err == nil {
				sum += f
			}
		}
		resp.Body.Close()
	}
	return sum, fmt.Sprintf("sum of %s%v scraped from %s", metric, labels, strings.Join(urls, ", ")), nil
}

func labelsMatch(raw string, want map[string]string) bool {
	for k, v := range want {
		if !strings.Contains(raw, fmt.Sprintf(`%s="%s"`, k, v)) {
			return false
		}
	}
	return true
}

var dsnPass = regexp.MustCompile(`(://[^:/@\s]+:)[^@\s]+@`)

// Redact hides credentials in env values and DSNs.
func Redact(k, v string) string {
	lk := strings.ToLower(k)
	if strings.Contains(lk, "password") || strings.Contains(lk, "secret") || strings.Contains(lk, "token") || strings.Contains(lk, "key") && !strings.Contains(lk, "keyprefix") {
		return "[REDACTED]"
	}
	return dsnPass.ReplaceAllString(v, "${1}[REDACTED]@")
}

// Collect saves logs and the (redacted) container description of every instance.
func (c *Connector) Collect(ctx context.Context, _ kit.TimeWindow) ([]kit.Artifact, error) {
	var out []kit.Artifact
	for _, n := range c.names {
		logs, err := c.d.Run(ctx, "logs", "--timestamps", n)
		if err != nil {
			var ce *infra.CmdError
			if !asCmdErr(err, &ce) {
				return out, err
			}
			logs = ce.Stderr
		}
		rel := path.Join(c.env.CaseDir, "logs", n+".log")
		if _, err := c.env.Evidence.WriteFile(rel, []byte(logs+"\n")); err != nil {
			return out, err
		}
		out = append(out, kit.Artifact{Kind: "log", Path: rel, Title: "Service logs " + n, Source: "sut"})
		raw, err := c.d.Run(ctx, "inspect", n)
		if err == nil {
			var desc []map[string]any
			if json.Unmarshal([]byte(raw), &desc) == nil && len(desc) == 1 {
				summary := map[string]any{"name": n, "image": c.image, "image_id": c.imageID,
					"state": desc[0]["State"], "restart_count": desc[0]["RestartCount"]}
				if cfg, ok := desc[0]["Config"].(map[string]any); ok {
					var env []string
					if l, ok := cfg["Env"].([]any); ok {
						for _, e := range l {
							k, v, _ := strings.Cut(fmt.Sprint(e), "=")
							env = append(env, k+"="+Redact(k, v))
						}
					}
					summary["env"] = env
					summary["labels"] = cfg["Labels"]
				}
				rel := path.Join(c.env.CaseDir, "logs", n+".container.json")
				if _, err := c.env.Evidence.WriteJSON(rel, summary); err == nil {
					out = append(out, kit.Artifact{Kind: "container", Path: rel, Title: "Container state and env (redacted) " + n, Source: "sut"})
				}
			}
		}
	}
	return out, nil
}

func asCmdErr(err error, target **infra.CmdError) bool {
	ce, ok := err.(*infra.CmdError)
	if ok {
		*target = ce
	}
	return ok
}

func (c *Connector) Teardown(ctx context.Context) error {
	if len(c.names) == 0 {
		return nil
	}
	_, err := c.d.Run(ctx, append([]string{"rm", "-f", "-v"}, c.names...)...)
	return err
}

var (
	_ kit.Connector = (*Connector)(nil)
	_ kit.Checker   = (*Connector)(nil)
	_ kit.ImageInfo = (*Connector)(nil)
	_               = io.Discard
)

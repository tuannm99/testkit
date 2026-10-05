// Package dockerstats exports container resource usage (USE method) for
// TestKit-managed containers from the Docker Engine stats API, with the run
// labels as Prometheus labels. It replaces cAdvisor, which cannot resolve
// containers when the engine uses the containerd image store (Docker >= 29
// default). Metric names follow cAdvisor so dashboards stay portable.
package dockerstats

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Engine talks to the Docker Engine API over its unix socket.
type Engine struct {
	c *http.Client
}

func NewEngine(sock string) *Engine {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}
	return &Engine{c: &http.Client{Transport: tr, Timeout: 10 * time.Second}}
}

func (e *Engine) get(ctx context.Context, path string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	resp, err := e.c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("docker %s: %d %s", path, resp.StatusCode, b)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type container struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Labels map[string]string `json:"Labels"`
}

type stats struct {
	CPU struct {
		Usage struct {
			Total  uint64 `json:"total_usage"`
			User   uint64 `json:"usage_in_usermode"`
			Kernel uint64 `json:"usage_in_kernelmode"`
		} `json:"cpu_usage"`
		Throttling struct {
			ThrottledTime uint64 `json:"throttled_time"`
		} `json:"throttling_data"`
	} `json:"cpu_stats"`
	Memory struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes   uint64 `json:"rx_bytes"`
		TxBytes   uint64 `json:"tx_bytes"`
		RxDropped uint64 `json:"rx_dropped"`
		TxDropped uint64 `json:"tx_dropped"`
	} `json:"networks"`
	Blkio struct {
		IOServiceBytes []struct {
			Op    string `json:"op"`
			Value uint64 `json:"value"`
		} `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
	Pids struct {
		Current uint64 `json:"current"`
	} `json:"pids_stats"`
}

type sample struct {
	labels string
	s      stats
}

// Exporter keeps the latest sample of every managed container.
type Exporter struct {
	E        *Engine
	Selector string // docker label filter, e.g. testkit.managed=true
	mu       sync.Mutex
	samples  []sample
	lastErr  string
}

var labelMap = [][2]string{
	{"testkit.run_id", "run_id"}, {"testkit.ns", "ns"}, {"testkit.service", "service"},
	{"testkit.case", "case"}, {"testkit.trigger", "trigger"}, {"com.docker.compose.service", "component"},
}

func promLabels(c container) string {
	name := strings.TrimPrefix(firstOr(c.Names, c.ID[:12]), "/")
	parts := []string{fmt.Sprintf(`name=%q`, name), fmt.Sprintf(`id=%q`, "/docker/"+c.ID)}
	for _, m := range labelMap {
		if v := c.Labels[m[0]]; v != "" {
			parts = append(parts, fmt.Sprintf(`%s=%q`, m[1], v))
		}
	}
	return strings.Join(parts, ",")
}

func firstOr(l []string, def string) string {
	if len(l) > 0 {
		return l[0]
	}
	return def
}

// Poll refreshes samples every interval until ctx is done.
func (x *Exporter) Poll(ctx context.Context, interval time.Duration) {
	for {
		x.collect(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func (x *Exporter) collect(ctx context.Context) {
	var cs []container
	filters := url.QueryEscape(fmt.Sprintf(`{"label":[%q]}`, x.Selector))
	if err := x.E.get(ctx, "/containers/json?filters="+filters, &cs); err != nil {
		x.mu.Lock()
		x.lastErr = err.Error()
		x.mu.Unlock()
		return
	}
	out := make([]sample, len(cs))
	var wg sync.WaitGroup
	for i, c := range cs {
		wg.Add(1)
		go func(i int, c container) {
			defer wg.Done()
			var s stats
			if err := x.E.get(ctx, "/containers/"+c.ID+"/stats?stream=false&one-shot=true", &s); err == nil {
				out[i] = sample{labels: promLabels(c), s: s}
			}
		}(i, c)
	}
	wg.Wait()
	var kept []sample
	for _, s := range out {
		if s.labels != "" {
			kept = append(kept, s)
		}
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].labels < kept[j].labels })
	x.mu.Lock()
	x.samples, x.lastErr = kept, ""
	x.mu.Unlock()
}

// ServeHTTP writes the Prometheus text exposition format.
func (x *Exporter) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	x.mu.Lock()
	samples, lastErr := x.samples, x.lastErr
	x.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	type metric struct {
		name, typ, help string
		val             func(stats) []valued
	}
	one := func(v float64) []valued { return []valued{{"", v}} }
	metrics := []metric{
		{"container_cpu_usage_seconds_total", "counter", "Cumulative CPU time consumed.", func(s stats) []valued { return one(float64(s.CPU.Usage.Total) / 1e9) }},
		{"container_cpu_user_seconds_total", "counter", "User CPU time.", func(s stats) []valued { return one(float64(s.CPU.Usage.User) / 1e9) }},
		{"container_cpu_system_seconds_total", "counter", "Kernel CPU time.", func(s stats) []valued { return one(float64(s.CPU.Usage.Kernel) / 1e9) }},
		{"container_cpu_cfs_throttled_seconds_total", "counter", "Throttled CPU time.", func(s stats) []valued { return one(float64(s.CPU.Throttling.ThrottledTime) / 1e9) }},
		{"container_memory_usage_bytes", "gauge", "Memory usage including cache.", func(s stats) []valued { return one(float64(s.Memory.Usage)) }},
		{"container_memory_working_set_bytes", "gauge", "Memory usage minus inactive file cache.", func(s stats) []valued {
			inactive := s.Memory.Stats["inactive_file"]
			if v, ok := s.Memory.Stats["total_inactive_file"]; ok {
				inactive = v
			}
			ws := float64(s.Memory.Usage) - float64(inactive)
			if ws < 0 {
				ws = 0
			}
			return one(ws)
		}},
		{"container_spec_memory_limit_bytes", "gauge", "Memory limit.", func(s stats) []valued { return one(float64(s.Memory.Limit)) }},
		{"container_network_receive_bytes_total", "counter", "Bytes received.", func(s stats) []valued { return perIface(s, func(rx, _ uint64) uint64 { return rx }) }},
		{"container_network_transmit_bytes_total", "counter", "Bytes transmitted.", func(s stats) []valued { return perIface(s, func(_, tx uint64) uint64 { return tx }) }},
		{"container_fs_reads_bytes_total", "counter", "Bytes read from block devices.", func(s stats) []valued { return one(blk(s, "read")) }},
		{"container_fs_writes_bytes_total", "counter", "Bytes written to block devices.", func(s stats) []valued { return one(blk(s, "write")) }},
		{"container_pids", "gauge", "Number of processes/threads.", func(s stats) []valued { return one(float64(s.Pids.Current)) }},
	}
	for _, m := range metrics {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", m.name, m.help, m.name, m.typ)
		for _, s := range samples {
			for _, v := range m.val(s.s) {
				fmt.Fprintf(w, "%s{%s%s} %g\n", m.name, s.labels, v.extra, v.v)
			}
		}
	}
	up := 1
	if lastErr != "" {
		up = 0
	}
	fmt.Fprintf(w, "# HELP tkstats_up Docker API reachable.\n# TYPE tkstats_up gauge\ntkstats_up %d\n", up)
	fmt.Fprintf(w, "# HELP tkstats_containers Containers sampled.\n# TYPE tkstats_containers gauge\ntkstats_containers %d\n", len(samples))
}

type valued struct {
	extra string
	v     float64
}

func perIface(s stats, f func(rx, tx uint64) uint64) []valued {
	var out []valued
	keys := make([]string, 0, len(s.Networks))
	for k := range s.Networks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		n := s.Networks[k]
		out = append(out, valued{fmt.Sprintf(`,interface=%q`, k), float64(f(n.RxBytes, n.TxBytes))})
	}
	return out
}

func blk(s stats, op string) float64 {
	var t uint64
	for _, e := range s.Blkio.IOServiceBytes {
		if strings.EqualFold(e.Op, op) {
			t += e.Value
		}
	}
	return float64(t)
}

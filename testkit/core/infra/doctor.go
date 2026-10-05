package infra

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Check severities.
const (
	OK   = "OK"
	Warn = "WARN"
	Fail = "FAIL"
	Skip = "SKIP"
)

// CheckResult is one line of `testkit doctor`.
type CheckResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// Capabilities are the special privileges available on this host. Test groups
// that need a missing capability are skipped and reported, never failed.
type Capabilities struct {
	DockerSock bool `json:"docker_sock"` // containers may mount the docker socket
	NetAdmin   bool `json:"net_admin"`   // containers may get CAP_NET_ADMIN
	Netem      bool `json:"netem"`       // the kernel supports tc netem (sch_netem)
}

// Has reports whether a capability name from a `requires:` list is present.
func (c Capabilities) Has(name string) bool {
	switch name {
	case "docker.sock", "docker_sock":
		return c.DockerSock
	case "NET_ADMIN", "net_admin":
		return c.NetAdmin
	case "netem":
		return c.Netem
	}
	return false
}

// DoctorReport is written to out/.infra/doctor.json for later runs.
type DoctorReport struct {
	At           time.Time     `json:"at"`
	Checks       []CheckResult `json:"checks"`
	Capabilities Capabilities  `json:"capabilities"`
}

// Failed reports whether any check failed (warnings do not fail).
func (r DoctorReport) Failed() bool {
	for _, c := range r.Checks {
		if c.Status == Fail {
			return true
		}
	}
	return false
}

// Doctor checks that this host can run TestKit.
func (s *Stack) Doctor(ctx context.Context) DoctorReport {
	rep := DoctorReport{At: time.Now().UTC()}
	add := func(c CheckResult) { rep.Checks = append(rep.Checks, c) }

	if _, err := s.D.Run(ctx, "version", "--format", "{{.Client.Version}}"); err != nil {
		add(CheckResult{"docker cli", Fail, err.Error(), "install Docker (https://docs.docker.com/get-docker/)"})
		return rep
	}
	ver, err := s.D.Run(ctx, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		add(CheckResult{"docker daemon", Fail, firstLine(err.Error()), "start the Docker daemon / Docker Desktop"})
		return rep
	}
	add(CheckResult{"docker daemon", OK, "server " + ver, ""})

	cv, err := s.D.Run(ctx, "compose", "version", "--short")
	switch {
	case err != nil:
		add(CheckResult{"docker compose", Fail, "compose plugin not found", "install the docker compose v2 plugin"})
	case !versionAtLeast(cv, 2, 20):
		add(CheckResult{"docker compose", Fail, "version " + cv + " < 2.20 (needs up --wait)", "upgrade docker compose"})
	default:
		add(CheckResult{"docker compose", OK, "version " + cv, ""})
	}

	// Memory as seen by the engine (the VM on Docker Desktop).
	if memStr, err := s.D.Run(ctx, "info", "--format", "{{.MemTotal}} {{.NCPU}}"); err == nil {
		f := strings.Fields(memStr)
		mem, _ := strconv.ParseInt(f[0], 10, 64)
		gb := float64(mem) / (1 << 30)
		st, fix := OK, ""
		switch {
		case gb < 4:
			st, fix = Fail, "give the Docker engine at least 4 GiB (8 GiB for observability+stores)"
		case gb < 8:
			st, fix = Warn, "8 GiB recommended when running every profile"
		}
		add(CheckResult{"engine memory", st, fmt.Sprintf("%.1f GiB, %s CPUs", gb, f[len(f)-1]), fix})
	}

	add(diskCheck(s.P.Abs(s.P.OutDir)))
	add(s.pinCheck())
	add(s.portCheck(ctx))

	base := s.P.Get("BASE_IMAGE")
	if n := s.P.Get("TK_NOFILE_LIMIT"); n != "" {
		if _, err := s.D.Run(ctx, "run", "--rm", "--label", LabelProject+"="+s.P.Name,
			"--ulimit", "nofile="+n+":"+n, base, "true"); err == nil {
			add(CheckResult{"nofile limit", OK, "containers may raise RLIMIT_NOFILE to " + n, ""})
		} else {
			add(CheckResult{"nofile limit", Fail, "engine refuses --ulimit nofile=" + n,
				"lower TK_NOFILE_LIMIT (e.g. export TK_NOFILE_LIMIT=$(ulimit -Hn)) or raise the engine limit"})
		}
	}
	// docker.sock: needed by observability (container discovery for metrics/logs)
	// and by docker-level chaos. Probed by actually mounting it.
	if _, err := s.D.Run(ctx, "run", "--rm", "--label", LabelProject+"="+s.P.Name,
		"-v", "/var/run/docker.sock:/var/run/docker.sock:ro", base, "test", "-S", "/var/run/docker.sock"); err == nil {
		rep.Capabilities.DockerSock = true
		add(CheckResult{"cap docker.sock", OK, "containers can mount /var/run/docker.sock (observability discovery, docker chaos)", ""})
	} else {
		add(CheckResult{"cap docker.sock", Warn, "cannot mount docker.sock: observability profile and docker chaos will be skipped",
			"allow mounting /var/run/docker.sock or run rootful docker"})
	}
	if _, err := s.D.Run(ctx, "run", "--rm", "--label", LabelProject+"="+s.P.Name,
		"--cap-add", "NET_ADMIN", base, "ip", "link", "set", "lo", "mtu", "65000"); err == nil {
		// Changing the MTU needs CAP_NET_ADMIN and no extra kernel module.
		rep.Capabilities.NetAdmin = true
		add(CheckResult{"cap NET_ADMIN", OK, "containers can get CAP_NET_ADMIN (tc/netem chaos)", ""})
	} else {
		add(CheckResult{"cap NET_ADMIN", Warn, "CAP_NET_ADMIN unavailable: netem-based chaos will be skipped (toxiproxy still works)", ""})
	}
	if img := s.P.Get("NETTOOLS_IMAGE"); rep.Capabilities.NetAdmin && img != "" {
		if _, err := s.D.Run(ctx, "run", "--rm", "--label", LabelProject+"="+s.P.Name, "--cap-add", "NET_ADMIN",
			img, "tc", "qdisc", "add", "dev", "eth0", "root", "netem", "delay", "1ms"); err == nil {
			rep.Capabilities.Netem = true
			add(CheckResult{"cap netem", OK, "kernel supports tc netem (network delay/loss chaos)", ""})
		} else {
			add(CheckResult{"cap netem", Warn, "tc netem unavailable (kernel module sch_netem missing): chaos.netem steps will be skipped",
				"load sch_netem on the docker host (modprobe sch_netem) to enable them"})
		}
	}
	return rep
}

// SaveDoctor persists the report so runs know the host capabilities.
func (s *Stack) SaveDoctor(r DoctorReport) error {
	p := filepath.Join(s.P.Abs(s.P.OutDir), ".infra", "doctor.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(r, "", "  ")
	return os.WriteFile(p, raw, 0o644)
}

// LoadCapabilities reads the last doctor report (zero value when absent).
func (s *Stack) LoadCapabilities() (Capabilities, bool) {
	raw, err := os.ReadFile(filepath.Join(s.P.Abs(s.P.OutDir), ".infra", "doctor.json"))
	if err != nil {
		return Capabilities{}, false
	}
	var r DoctorReport
	if json.Unmarshal(raw, &r) != nil {
		return Capabilities{}, false
	}
	return r.Capabilities, true
}

var tagRe = regexp.MustCompile(`^[^:@]+(:[^:@/]+)?(@sha256:[a-f0-9]{64})?$`)

// pinCheck rejects unpinned or "latest" images in the compose file and env files.
func (s *Stack) pinCheck() CheckResult {
	var bad []string
	for k, v := range s.P.PinnedImages() {
		ref := v
		// strip registry host:port before looking for the tag.
		if i := strings.LastIndex(ref, "/"); i >= 0 {
			ref = ref[i+1:]
		}
		if !strings.Contains(ref, ":") && !strings.Contains(ref, "@") || strings.HasSuffix(ref, ":latest") || !tagRe.MatchString(ref) {
			bad = append(bad, k+"="+v)
		}
	}
	if cf, err := s.ComposeFile(); err == nil {
		for name, svc := range cf {
			if !strings.Contains(svc.Image, "${") && (!strings.Contains(svc.Image, ":") || strings.HasSuffix(svc.Image, ":latest")) {
				bad = append(bad, "compose:"+name+"="+svc.Image)
			}
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		return CheckResult{"image pinning", Fail, "unpinned images: " + strings.Join(bad, ", "), "pin every image in infra/compose/versions.env"}
	}
	return CheckResult{"image pinning", OK, fmt.Sprintf("%d images pinned", len(s.P.PinnedImages())), ""}
}

// portCheck verifies that the published host ports are free, or held by this
// project's own containers.
func (s *Stack) portCheck(ctx context.Context) CheckResult {
	if os.Getenv("TESTKIT_IN_CONTAINER") == "1" {
		return CheckResult{"host ports", Skip, "runner is containerised; host ports are checked by docker when publishing", ""}
	}
	ours := map[string]bool{}
	if out, err := s.D.Run(ctx, "ps", "--filter", "label=com.docker.compose.project="+s.P.Name, "--format", "{{.Ports}}"); err == nil {
		for _, m := range regexp.MustCompile(`127\.0\.0\.1:(\d+)->`).FindAllStringSubmatch(out, -1) {
			ours[m[1]] = true
		}
	}
	var busy []string
	env := s.P.Env()
	for _, k := range sortedKeys(env) {
		if !strings.HasPrefix(k, "TK_PORT_") {
			continue
		}
		port := env[k]
		if ours[port] {
			continue
		}
		l, err := net.Listen("tcp", "127.0.0.1:"+port)
		if err != nil {
			busy = append(busy, k+"="+port)
			continue
		}
		l.Close()
	}
	if len(busy) > 0 {
		return CheckResult{"host ports", Fail, "in use by another process: " + strings.Join(busy, ", "),
			"free them or override TK_PORT_* (and TESTKIT_PROJECT for a second stack)"}
	}
	return CheckResult{"host ports", OK, "all TK_PORT_* free or owned by this stack", ""}
}

func diskCheck(dir string) CheckResult {
	_ = os.MkdirAll(dir, 0o755)
	if runtime.GOOS == "windows" {
		return CheckResult{"disk", Skip, "not measured on windows", ""}
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return CheckResult{"disk", Warn, err.Error(), ""}
	}
	gb := float64(st.Bavail) * float64(st.Bsize) / (1 << 30)
	switch {
	case gb < 5:
		return CheckResult{"disk", Fail, fmt.Sprintf("%.1f GiB free under %s", gb, dir), "free at least 5 GiB (images + evidence)"}
	case gb < 15:
		return CheckResult{"disk", Warn, fmt.Sprintf("%.1f GiB free under %s", gb, dir), "15 GiB recommended"}
	}
	return CheckResult{"disk", OK, fmt.Sprintf("%.1f GiB free under %s", gb, dir), ""}
}

func versionAtLeast(v string, major, minor int) bool {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return false
	}
	ma, _ := strconv.Atoi(parts[0])
	mi, _ := strconv.Atoi(parts[1])
	return ma > major || ma == major && mi >= minor
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

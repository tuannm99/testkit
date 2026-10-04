package infra

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tuannm99/testkit/testkit/core/config"
)

// Profiles known by the compose file, in start order.
var Profiles = []string{"core", "stores", "mocks", "observability", "chaos"}

// Label keys put on every resource TestKit creates outside compose.
const (
	LabelManaged = "testkit.managed"
	LabelProject = "testkit.project"
	LabelRunID   = "testkit.run_id"
	LabelCase    = "testkit.case"
	LabelService = "testkit.service"
)

// Stack is the compose-managed infrastructure of one TestKit project.
type Stack struct {
	P   *config.Project
	D   *Docker
	Out io.Writer
}

func NewStack(p *config.Project, d *Docker, out io.Writer) *Stack {
	d.Env = append(d.Env, "TESTKIT_PROJECT="+p.Name, "TESTKIT_NETWORK="+p.Network)
	return &Stack{P: p, D: d, Out: out}
}

// ComposeService is the subset of a compose service TestKit reads.
type ComposeService struct {
	Profiles []string          `yaml:"profiles"`
	Image    string            `yaml:"image"`
	Labels   map[string]string `yaml:"labels"`
}

// ComposeFile parses the compose file (without interpolation).
func (s *Stack) ComposeFile() (map[string]ComposeService, error) {
	raw, err := os.ReadFile(s.P.Abs(s.P.Compose))
	if err != nil {
		return nil, err
	}
	var f struct {
		Services map[string]ComposeService `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", s.P.Compose, err)
	}
	return f.Services, nil
}

func (s *Stack) baseArgs(profiles ...string) []string {
	args := []string{"compose", "-p", s.P.Name, "-f", s.P.Abs(s.P.Compose)}
	for _, f := range s.P.EnvFiles {
		args = append(args, "--env-file", s.P.Abs(f))
	}
	for _, p := range profiles {
		args = append(args, "--profile", p)
	}
	return args
}

// UpOptions selects what to start.
type UpOptions struct {
	Profiles []string
	Services []*config.Service // nil: every store/mock of the profile
	Build    bool              // force rebuild of TestKit-built images
	Timeout  time.Duration
}

// Selection is the resolved list of compose services for an UpOptions.
type Selection struct {
	Profiles []string `json:"profiles"`
	Services []string `json:"services"`
}

// Resolve maps profiles + service descriptors to compose services. Only what
// the declared services need is selected ("chỉ dựng phần cần").
func (s *Stack) Resolve(o UpOptions) (Selection, error) {
	cf, err := s.ComposeFile()
	if err != nil {
		return Selection{}, err
	}
	known := map[string]bool{}
	for _, p := range Profiles {
		known[p] = true
	}
	for _, p := range o.Profiles {
		if !known[p] {
			return Selection{}, fmt.Errorf("unknown profile %q (known: %s)", p, strings.Join(Profiles, ", "))
		}
	}
	// What the declared services need, by component name.
	var need map[string]bool
	if len(o.Services) > 0 {
		need = map[string]bool{"mockhub": true}
		for _, svc := range o.Services {
			for _, st := range svc.Stores.Names() {
				need[st] = true
			}
			for _, m := range svc.MockComponents() {
				need[m] = true
			}
		}
	}
	filtered := map[string]bool{"stores": true, "mocks": true}
	set := map[string]bool{}
	for name, cs := range cf {
		for _, prof := range cs.Profiles {
			if !contains(o.Profiles, prof) {
				continue
			}
			if need != nil && filtered[prof] && !need[name] {
				continue
			}
			set[name] = true
		}
	}
	if len(set) == 0 {
		return Selection{}, fmt.Errorf("nothing to start for profiles %v", o.Profiles)
	}
	return Selection{Profiles: o.Profiles, Services: config.SortedKeys(set)}, nil
}

// State is persisted after `up` and copied into every run manifest.
type State struct {
	Project   string            `json:"project"`
	Network   string            `json:"network"`
	Selection Selection         `json:"selection"`
	Images    map[string]string `json:"images"` // compose service -> image ref
	Digests   map[string]string `json:"digests"`
	StartedAt time.Time         `json:"started_at"`
}

func (s *Stack) statePath() string {
	return filepath.Join(s.P.Abs(s.P.OutDir), ".infra", s.P.Name+".state.json")
}

// LoadState returns the state written by the last `up` (nil if none).
func (s *Stack) LoadState() (*State, error) {
	raw, err := os.ReadFile(s.statePath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	st := &State{}
	return st, json.Unmarshal(raw, st)
}

// Up builds TestKit images when needed, starts the selection and waits until
// every container reports healthy. It is idempotent.
func (s *Stack) Up(ctx context.Context, o UpOptions) (*State, error) {
	sel, err := s.Resolve(o)
	if err != nil {
		return nil, err
	}
	if contains(sel.Services, "mockhub") {
		if err := s.EnsureImage(ctx, "mockhub", o.Build); err != nil {
			return nil, err
		}
	}
	timeout := o.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	args := append(s.baseArgs(sel.Profiles...), "up", "-d", "--wait",
		"--wait-timeout", fmt.Sprint(int(timeout.Seconds())), "--remove-orphans")
	args = append(args, sel.Services...)
	if err := s.D.Stream(ctx, s.Out, args...); err != nil {
		s.dumpUnhealthy(ctx, sel)
		return nil, fmt.Errorf("compose up: %w", err)
	}
	st := &State{Project: s.P.Name, Network: s.P.Network, Selection: sel,
		Images: map[string]string{}, Digests: map[string]string{}, StartedAt: time.Now().UTC()}
	if prev, _ := s.LoadState(); prev != nil {
		// Keep services started by an earlier `up` that are still running.
		for _, svc := range prev.Selection.Services {
			if !contains(st.Selection.Services, svc) {
				st.Selection.Services = append(st.Selection.Services, svc)
			}
		}
		for _, p := range prev.Selection.Profiles {
			if !contains(st.Selection.Profiles, p) {
				st.Selection.Profiles = append(st.Selection.Profiles, p)
			}
		}
		sort.Strings(st.Selection.Services)
	}
	if err := s.fillImages(ctx, st); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(s.statePath()), 0o755); err != nil {
		return nil, err
	}
	raw, _ := json.MarshalIndent(st, "", "  ")
	return st, os.WriteFile(s.statePath(), raw, 0o644)
}

// fillImages records the image and content digest of every running container.
func (s *Stack) fillImages(ctx context.Context, st *State) error {
	out, err := s.D.Run(ctx, "ps", "--filter", "label=com.docker.compose.project="+s.P.Name,
		"--format", `{{.Label "com.docker.compose.service"}}|{{.Image}}`)
	if err != nil {
		return err
	}
	for _, l := range Lines(out) {
		svc, img, _ := strings.Cut(l, "|")
		st.Images[svc] = img
		if d, err := s.D.Run(ctx, "image", "inspect", "--format", "{{.Id}}", img); err == nil {
			st.Digests[svc] = d
		}
	}
	return nil
}

func (s *Stack) dumpUnhealthy(ctx context.Context, sel Selection) {
	out, _ := s.D.Run(ctx, append(s.baseArgs(sel.Profiles...), "ps", "-a",
		"--format", "{{.Service}}\t{{.Status}}")...)
	fmt.Fprintf(s.Out, "\n--- container status ---\n%s\n", out)
	for _, l := range Lines(out) {
		if strings.Contains(l, "healthy") && !strings.Contains(l, "unhealthy") {
			continue
		}
		svc := strings.Fields(l)[0]
		logs, _ := s.D.Run(ctx, append(s.baseArgs(sel.Profiles...), "logs", "--tail", "40", svc)...)
		fmt.Fprintf(s.Out, "--- last logs of %s ---\n%s\n", svc, logs)
	}
}

// Leftovers lists every resource that still belongs to the project.
type Leftovers struct {
	Containers []string `json:"containers"`
	Volumes    []string `json:"volumes"`
	Networks   []string `json:"networks"`
}

func (l Leftovers) Empty() bool {
	return len(l.Containers)+len(l.Volumes)+len(l.Networks) == 0
}

func (s *Stack) Leftovers(ctx context.Context) (Leftovers, error) {
	var l Leftovers
	seen := map[string]bool{}
	for _, f := range []string{"label=com.docker.compose.project=" + s.P.Name, "label=" + LabelProject + "=" + s.P.Name} {
		out, err := s.D.Run(ctx, "ps", "-a", "--filter", f, "--format", "{{.Names}}")
		if err != nil {
			return l, err
		}
		for _, n := range Lines(out) {
			if !seen[n] {
				seen[n] = true
				l.Containers = append(l.Containers, n)
			}
		}
	}
	out, err := s.D.Run(ctx, "volume", "ls", "--filter", "label=com.docker.compose.project="+s.P.Name, "--format", "{{.Name}}")
	if err != nil {
		return l, err
	}
	l.Volumes = Lines(out)
	out, err = s.D.Run(ctx, "network", "ls", "--filter", "name=^"+s.P.Network+"$", "--format", "{{.Name}}")
	if err != nil {
		return l, err
	}
	l.Networks = Lines(out)
	return l, nil
}

// Down removes the compose stack, every container TestKit started for this
// project (services under test, load generators, chaos helpers), volumes and
// the network. It is idempotent and returns what is left (should be nothing).
func (s *Stack) Down(ctx context.Context) (Leftovers, error) {
	args := append(s.baseArgs("*"), "down", "-v", "--remove-orphans", "-t", "5")
	if err := s.D.Stream(ctx, s.Out, args...); err != nil {
		return Leftovers{}, fmt.Errorf("compose down: %w", err)
	}
	out, err := s.D.Run(ctx, "ps", "-aq", "--filter", "label="+LabelProject+"="+s.P.Name)
	if err != nil {
		return Leftovers{}, err
	}
	if ids := Lines(out); len(ids) > 0 {
		if _, err := s.D.Run(ctx, append([]string{"rm", "-f", "-v"}, ids...)...); err != nil {
			return Leftovers{}, err
		}
	}
	if l, _ := s.Leftovers(ctx); len(l.Networks) > 0 {
		if _, err := s.D.Run(ctx, "network", "rm", s.P.Network); err != nil {
			return Leftovers{}, err
		}
	}
	_ = os.Remove(s.statePath())
	return s.Leftovers(ctx)
}

// Running reports which compose services are running and healthy.
func (s *Stack) Running(ctx context.Context) (map[string]string, error) {
	out, err := s.D.Run(ctx, "ps", "-a", "--filter", "label=com.docker.compose.project="+s.P.Name,
		"--format", `{{.Label "com.docker.compose.service"}}|{{.Status}}`)
	if err != nil {
		return nil, err
	}
	res := map[string]string{}
	for _, l := range Lines(out) {
		svc, status, _ := strings.Cut(l, "|")
		res[svc] = status
	}
	return res, nil
}

// RequireHealthy fails unless every named compose service is up and healthy.
func (s *Stack) RequireHealthy(ctx context.Context, services []string) error {
	run, err := s.Running(ctx)
	if err != nil {
		return err
	}
	var missing []string
	for _, svc := range services {
		st, ok := run[svc]
		if !ok || !strings.HasPrefix(st, "Up") || strings.Contains(st, "unhealthy") || strings.Contains(st, "starting") {
			missing = append(missing, fmt.Sprintf("%s (%s)", svc, orNone(st)))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("infrastructure not ready: %s — run `testkit up` first", strings.Join(missing, ", "))
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "not running"
	}
	return s
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

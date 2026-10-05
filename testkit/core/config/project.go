// Package config loads the declarative inputs of TestKit: the project file
// (testkit.yaml), the env files it references (pinned versions, ports, test
// credentials) and the per-service descriptors (services/*.yaml).
//
// Nothing in TestKit hard-codes an address, a port, a credential or an image
// tag: they all come from here.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ProjectFile is the name of the project descriptor searched for from the
// working directory upwards.
const ProjectFile = "testkit.yaml"

// ImageBuild describes an image TestKit builds itself.
type ImageBuild struct {
	Context    string            `yaml:"context"`
	Dockerfile string            `yaml:"dockerfile"`
	Target     string            `yaml:"target"`
	Args       map[string]string `yaml:"args"`
}

// QC describes the test management tool results are exported to.
type QC struct {
	Tool       string `yaml:"tool"`        // zephyr-scale
	ProjectKey string `yaml:"project_key"` // Jira project key, e.g. ORD
	API        string `yaml:"api"`         // Zephyr Scale API base URL (Cloud: https://api.zephyrscale.smartbear.com/v2)
	TokenEnv   string `yaml:"token_env"`   // name of the env var holding the API token (never the token itself)
	CycleName  string `yaml:"cycle_name"`  // template: {{ .suite }} {{ .run_id }} {{ .date }}
	Folder     string `yaml:"folder"`      // test case folder used by the CSV import
	// AutoCreate lets Zephyr create test cases for executions without qc_key
	// (matched by name). Off by default: cases should be imported first.
	AutoCreate bool `yaml:"auto_create_test_cases"`
}

// Project is the parsed testkit.yaml plus the merged environment.
type Project struct {
	Root         string                `yaml:"-"`
	Name         string                `yaml:"project"`
	Network      string                `yaml:"network"`
	Compose      string                `yaml:"compose"`
	EnvFiles     []string              `yaml:"env_files"`
	ServicesDir  string                `yaml:"services_dir"`
	ScenariosDir string                `yaml:"scenarios_dir"`
	MocksDir     string                `yaml:"mocks_dir"`
	BaselinesDir string                `yaml:"baselines_dir"`
	OutDir       string                `yaml:"out_dir"`
	Images       map[string]ImageBuild `yaml:"images"`
	QC           *QC                   `yaml:"qc"`

	// fileEnv holds values read from EnvFiles; process env overrides them.
	fileEnv map[string]string
}

// FindRoot walks up from start until it finds testkit.yaml.
// TESTKIT_ROOT short-circuits the search.
func FindRoot(start string) (string, error) {
	if r := os.Getenv("TESTKIT_ROOT"); r != "" {
		return filepath.Abs(r)
	}
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ProjectFile)); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("%s not found from %s upwards (set TESTKIT_ROOT)", ProjectFile, start)
		}
		dir = parent
	}
}

// LoadProject loads testkit.yaml found from start upwards.
func LoadProject(start string) (*Project, error) {
	root, err := FindRoot(start)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(root, ProjectFile))
	if err != nil {
		return nil, err
	}
	p := &Project{Root: root}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(p); err != nil {
		return nil, fmt.Errorf("%s: %w", ProjectFile, err)
	}
	if p.Name == "" || p.Compose == "" || p.Network == "" {
		return nil, errors.New("testkit.yaml: project, network and compose are required")
	}
	if p.OutDir == "" {
		p.OutDir = "out"
	}
	p.fileEnv = map[string]string{}
	for _, f := range p.EnvFiles {
		kv, err := ParseEnvFile(p.Abs(f))
		if err != nil {
			return nil, err
		}
		for k, v := range kv {
			p.fileEnv[k] = v
		}
	}
	// Process environment overrides for the two identity settings.
	if v := os.Getenv("TESTKIT_PROJECT"); v != "" {
		p.Name = v
	}
	if v := os.Getenv("TESTKIT_NETWORK"); v != "" {
		p.Network = v
	}
	return p, nil
}

// Abs resolves a path relative to the project root.
func (p *Project) Abs(rel string) string {
	if filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(p.Root, rel)
}

// Rel shows a path relative to the project root when it is inside it.
func (p *Project) Rel(path string) string {
	if r, err := filepath.Rel(p.Root, path); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return path
}

// Get returns a configuration value: process env first, then env files.
func (p *Project) Get(key string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return p.fileEnv[key]
}

// MustGet is Get but reports missing keys as an error.
func (p *Project) MustGet(key string) (string, error) {
	v := p.Get(key)
	if v == "" {
		return "", fmt.Errorf("configuration key %s is not set (env files: %v)", key, p.EnvFiles)
	}
	return v, nil
}

// Env returns the merged configuration (files overridden by process env) for
// every key defined in the env files. Used to build the run manifest and the
// environment of docker compose.
func (p *Project) Env() map[string]string {
	out := make(map[string]string, len(p.fileEnv))
	for k := range p.fileEnv {
		out[k] = p.Get(k)
	}
	out["TESTKIT_PROJECT"] = p.Name
	out["TESTKIT_NETWORK"] = p.Network
	return out
}

// Images returns every pinned *_IMAGE value, sorted by key.
func (p *Project) PinnedImages() map[string]string {
	out := map[string]string{}
	for k, v := range p.Env() {
		if strings.HasSuffix(k, "_IMAGE") {
			out[k] = v
		}
	}
	return out
}

// ImageTag returns the tag of an image TestKit builds itself.
func (p *Project) ImageTag(name string) string {
	return fmt.Sprintf("testkit/%s:%s", name, p.Get("TESTKIT_VERSION"))
}

// SortedKeys is a small helper used for deterministic output.
func SortedKeys[M ~map[string]V, V any](m M) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ParseEnvFile parses KEY=VALUE lines. '#' starts a comment line; surrounding
// quotes are stripped. It is intentionally the same subset docker compose reads.
func ParseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, n)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		out[strings.TrimSpace(k)] = v
	}
	return out, sc.Err()
}

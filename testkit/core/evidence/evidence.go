// Package evidence owns the output directory of a run:
//
//	out/<run_id>/
//	  manifest.json   commit, image versions, environment, timing, sha256 of every file
//	  report.html  junit.xml  traceability.csv
//	  <TC-ID>/case.json timeline.json input/ output/ ui/ grafana/ logs/
//
// Every file is hashed into the manifest so a QC reviewer can verify that the
// bundle was not modified after the run (`testkit verify`).
package evidence

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// NewRunID returns a sortable, namespace-safe id: r20261004-180501-ab12.
func NewRunID(now time.Time) string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return fmt.Sprintf("r%s-%s", now.UTC().Format("20060102-150405"), hex.EncodeToString(b))
}

var runIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,40}$`)

// ValidRunID reports whether id is safe to use in paths and namespaces.
func ValidRunID(id string) bool { return runIDRe.MatchString(id) }

// Dir is the evidence directory of one run.
type Dir struct {
	Root string
}

// Open creates (if needed) out/<runID>.
func Open(outDir, runID string) (*Dir, error) {
	if !ValidRunID(runID) {
		return nil, fmt.Errorf("invalid run id %q", runID)
	}
	root := filepath.Join(outDir, runID)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Dir{Root: root}, nil
}

// Path joins elements under the run directory.
func (d *Dir) Path(elem ...string) string {
	return filepath.Join(append([]string{d.Root}, elem...)...)
}

// Rel returns a path relative to the run directory (for links in reports).
func (d *Dir) Rel(abs string) string {
	r, err := filepath.Rel(d.Root, abs)
	if err != nil {
		return abs
	}
	return filepath.ToSlash(r)
}

// WriteFile writes data at rel, creating parent directories.
func (d *Dir) WriteFile(rel string, data []byte) (string, error) {
	p := d.Path(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	return p, os.WriteFile(p, data, 0o644)
}

// WriteJSON writes v as indented JSON at rel.
func (d *Dir) WriteJSON(rel string, v any) (string, error) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return d.WriteFile(rel, append(raw, '\n'))
}

// FileHash is one manifest entry.
type FileHash struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// HashTree hashes every file under root except the manifest itself.
func HashTree(root string) ([]FileHash, error) {
	var out []FileHash
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if rel == ManifestFile {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		n, err := io.Copy(h, f)
		if err != nil {
			return err
		}
		out = append(out, FileHash{Path: rel, SHA256: hex.EncodeToString(h.Sum(nil)), Size: n})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, err
}

// ManifestFile is the manifest name inside a run directory.
const ManifestFile = "manifest.json"

// Manifest describes how a run was produced.
type Manifest struct {
	RunID       string            `json:"run_id"`
	Tool        string            `json:"tool"`
	ToolVersion string            `json:"tool_version"`
	Command     []string          `json:"command"`
	StartedAt   time.Time         `json:"started_at"`
	FinishedAt  time.Time         `json:"finished_at"`
	Git         GitInfo           `json:"git"`
	Host        HostInfo          `json:"host"`
	Images      map[string]string `json:"images"`         // pinned image refs (versions.env)
	Running     map[string]string `json:"running_images"` // compose service -> image actually running
	Digests     map[string]string `json:"image_ids"`      // compose service / SUT -> image id
	Services    map[string]string `json:"services"`       // service under test -> image ref
	Mocks       []MockProvenance  `json:"mocks"`
	Env         map[string]string `json:"environment"` // non-secret settings
	Files       []FileHash        `json:"files"`
}

type GitInfo struct {
	Commit string `json:"commit"`
	Branch string `json:"branch"`
	Dirty  bool   `json:"dirty"`
}

type HostInfo struct {
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	DockerVersion string `json:"docker_version"`
	Hostname      string `json:"hostname"`
	CPUs          int    `json:"cpus"`
}

// MockProvenance records which real API version each mock imitates.
type MockProvenance struct {
	Service    string `json:"service"`
	Mock       string `json:"mock"`
	Kind       string `json:"kind"`
	APIVersion string `json:"api_version"`
	VerifiedAt string `json:"verified_at"`
	Against    string `json:"verified_against,omitempty"` // sandbox | docs
	Spec       string `json:"spec,omitempty"`
	SpecSHA256 string `json:"spec_sha256,omitempty"`
}

// Seal hashes every file and writes the manifest. Call it last.
func (d *Dir) Seal(m *Manifest) error {
	files, err := HashTree(d.Root)
	if err != nil {
		return err
	}
	m.Files = files
	_, err = d.WriteJSON(ManifestFile, m)
	return err
}

// Verify re-hashes a sealed run directory and lists every mismatch.
func Verify(root string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(root, ManifestFile))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	now, err := HashTree(root)
	if err != nil {
		return nil, err
	}
	want := map[string]string{}
	for _, f := range m.Files {
		want[f.Path] = f.SHA256
	}
	var problems []string
	for _, f := range now {
		w, ok := want[f.Path]
		switch {
		case !ok:
			problems = append(problems, "added after seal: "+f.Path)
		case w != f.SHA256:
			problems = append(problems, "modified: "+f.Path)
		}
		delete(want, f.Path)
	}
	for p := range want {
		problems = append(problems, "missing: "+p)
	}
	sort.Strings(problems)
	return problems, nil
}

// SafeName turns an arbitrary label into a file-name-safe stem.
func SafeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

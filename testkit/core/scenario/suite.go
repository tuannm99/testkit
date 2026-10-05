package scenario

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Suite is a named selection of cases with how to run them and the hard
// rules (gate) that decide GO / NO-GO for a release.
type Suite struct {
	Kind         string       `yaml:"kind"` // Suite
	Name         string       `yaml:"suite"`
	Title        string       `yaml:"title"`
	Cases        []string     `yaml:"cases"`   // files or directories, relative to the suite file
	Exclude      []string     `yaml:"exclude"` // case ids left out
	OnlyApproved *bool        `yaml:"only_approved"`
	Mutations    bool         `yaml:"mutations"`
	Retries      int          `yaml:"retries"`
	Parallel     int          `yaml:"parallel"`
	Pack         bool         `yaml:"pack"` // write out/<run_id>.zip after the run
	Requirements []string     `yaml:"requirements"`
	Gate         GateSpec     `yaml:"gate"`
	Quarantine   []Quarantine `yaml:"quarantine"`

	File string `yaml:"-"`
}

// GateSpec tunes the release gate; the zero value is the strictest gate.
type GateSpec struct {
	AllowSkippedCapability bool `yaml:"allow_skipped_capability"` // missing privilege: listed, not blocking
}

// Quarantine excuses a known-flaky case for a limited time; a flaky case
// outside quarantine, or past its deadline, is NO-GO.
type Quarantine struct {
	Case     string `yaml:"case"`
	Reason   string `yaml:"reason"`
	Owner    string `yaml:"owner"`
	Deadline string `yaml:"deadline"` // YYYY-MM-DD
	Ticket   string `yaml:"ticket"`
}

// LoadSuite reads and validates a suite file.
func LoadSuite(path string) (*Suite, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := &Suite{}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.File, _ = filepath.Abs(path)
	switch {
	case s.Kind != "Suite":
		return nil, fmt.Errorf("%s: kind must be Suite", path)
	case s.Name == "":
		return nil, fmt.Errorf("%s: suite name is required", path)
	case len(s.Cases) == 0:
		return nil, fmt.Errorf("%s: cases is empty", path)
	}
	for _, q := range s.Quarantine {
		if q.Case == "" || q.Owner == "" || q.Ticket == "" {
			return nil, fmt.Errorf("%s: quarantine of %q needs case, owner and ticket", path, q.Case)
		}
		if _, err := time.Parse("2006-01-02", q.Deadline); err != nil {
			return nil, fmt.Errorf("%s: quarantine of %s: deadline must be YYYY-MM-DD", path, q.Case)
		}
	}
	return s, nil
}

// Paths resolves the case paths relative to the suite file.
func (s *Suite) Paths() []string {
	out := make([]string, len(s.Cases))
	for i, c := range s.Cases {
		if filepath.IsAbs(c) {
			out[i] = c
		} else {
			out[i] = filepath.Join(filepath.Dir(s.File), c)
		}
	}
	return out
}

// ApprovedOnly defaults to true: a release runs reviewed cases only.
func (s *Suite) ApprovedOnly() bool { return s.OnlyApproved == nil || *s.OnlyApproved }

package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApproveKeepsFileAndReplacesAdmission(t *testing.T) {
	f := filepath.Join(t.TempDir(), "c.yaml")
	src := "# drafted\nid: TC-X-1\nstatus: draft # waiting\nowner: qa\nexpect:\n  - { id: A1, check: x, eq: 1 }\nadmission:\n  run_id: old\n  by: someone\n\ntags: [a]\n"
	if err := os.WriteFile(f, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Approve(f, Admission{RunID: "r1", By: "qc-lead", Stability: 2, Killed: []string{"M1[kafka]"}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f)
	s := string(b)
	for _, want := range []string{"# drafted\n", "status: approved\n", "  - { id: A1, check: x, eq: 1 }\n", "tags: [a]\n", "run_id: r1", "by: qc-lead", "- M1[kafka]"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if strings.Contains(s, "old") || strings.Count(s, "admission:") != 1 || strings.Contains(s, "status: draft") {
		t.Errorf("old admission/status kept:\n%s", s)
	}
}

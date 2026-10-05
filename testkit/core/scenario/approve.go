package scenario

import (
	"bytes"
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

var (
	statusLine    = regexp.MustCompile(`(?m)^status:[ \t]*\S+[ \t]*(#.*)?$`)
	admissionNode = regexp.MustCompile(`(?m)^admission:.*\n(?:[ \t]+.*\n|[ \t]*\n)*`)
)

// Approve rewrites a case file: status becomes approved and the admission
// block (replacing any previous one) records the gate run. Everything else in
// the file — comments, order, formatting — is kept as written.
func Approve(file string, a Admission) error {
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	if !statusLine.Match(raw) {
		return fmt.Errorf("%s: no top-level status line", file)
	}
	out := statusLine.ReplaceAll(raw, []byte("status: approved"))
	out = admissionNode.ReplaceAll(out, nil)
	var block bytes.Buffer
	enc := yaml.NewEncoder(&block)
	enc.SetIndent(2)
	if err := enc.Encode(map[string]Admission{"admission": a}); err != nil {
		return err
	}
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, block.Bytes()...)
	// The result must still be a valid case with the new values.
	var check Case
	if err := yaml.NewDecoder(bytes.NewReader(out)).Decode(&check); err != nil {
		return fmt.Errorf("%s: rewritten file does not parse: %w", file, err)
	}
	if check.Status != "approved" || check.Admission == nil || check.Admission.RunID != a.RunID {
		return fmt.Errorf("%s: rewrite did not take effect", file)
	}
	return os.WriteFile(file, out, 0o644)
}

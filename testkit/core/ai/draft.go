package ai

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DraftInput is what a draft is written from.
type DraftInput struct {
	ID          string
	Requirement string
	Service     string
	Descriptor  string   // the service YAML
	Examples    []string // approved cases of the service, as written
	Vocabulary  string   // steps and checks TestKit understands
	// Repair round: the previous draft and what lint said about it.
	Previous   string
	LintErrors []string
}

// DraftRequest builds the request.
func DraftRequest(in DraftInput) Request {
	var b strings.Builder
	b.WriteString(prompt("draft"))
	fmt.Fprintf(&b, "\nid to use: %s\nservice: %s\n\nRequirement:\n%s\n", in.ID, in.Service, strings.TrimSpace(in.Requirement))
	fmt.Fprintf(&b, "\nVocabulary (steps, given keys, checks):\n%s\n", in.Vocabulary)
	fmt.Fprintf(&b, "\nService descriptor (%s):\n```yaml\n%s\n```\n", in.Service, strings.TrimSpace(in.Descriptor))
	for i, e := range in.Examples {
		fmt.Fprintf(&b, "\nExample case %d:\n```yaml\n%s\n```\n", i+1, strings.TrimSpace(e))
	}
	if in.Previous != "" {
		fmt.Fprintf(&b, "\nYour previous draft did not pass `testkit lint`. Fix exactly these problems and answer with the whole corrected YAML:\n- %s\n\nPrevious draft:\n```yaml\n%s\n```\n",
			strings.Join(in.LintErrors, "\n- "), strings.TrimSpace(in.Previous))
	}
	return Request{Task: "draft", System: SystemPrompt(), Prompt: b.String(), MaxTokens: 16000}
}

// FinalizeDraft takes the model's YAML and enforces what is not the model's
// to decide: the id, status draft, the provenance, and no admission or QC
// key, no owner, and the requirement id the person gave. Comments and order
// written by the model are kept.
func FinalizeDraft(answer, id, requirement, by string, sources []string, now time.Time) ([]byte, error) {
	src := extractBlock(answer, "yaml")
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		return nil, fmt.Errorf("draft is not valid YAML: %w", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("draft is not a YAML mapping")
	}
	m := doc.Content[0]
	set := func(key string, val *yaml.Node) {
		for i := 0; i+1 < len(m.Content); i += 2 {
			if m.Content[i].Value == key {
				m.Content[i+1] = val
				return
			}
		}
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, val)
	}
	del := func(key string) {
		for i := 0; i+1 < len(m.Content); i += 2 {
			if m.Content[i].Value == key {
				m.Content = append(m.Content[:i], m.Content[i+2:]...)
				return
			}
		}
	}
	scalar := func(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: v} }
	set("id", scalar(id))
	set("status", scalar("draft"))
	if requirement == "" {
		requirement = "REQ-TBD" // visibly unassigned until a person links the real requirement
	}
	set("requirement", scalar(requirement))
	del("admission")
	del("qc_key")
	del("owner") // a person, set by whoever takes the draft over
	var gen yaml.Node
	if err := gen.Encode(map[string]any{"by": by, "at": now.UTC().Format(time.RFC3339), "sources": sources}); err != nil {
		return nil, err
	}
	set("generated", &gen)
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	header := "# Drafted by " + by + " — status draft: review, then `testkit admit --approve --by <name>` (mutation gate) before it can run in a release.\n"
	return append([]byte(header), out.Bytes()...), nil
}

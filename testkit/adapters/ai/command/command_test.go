package command

import (
	"context"
	"strings"
	"testing"

	"github.com/tuannm99/testkit/testkit/core/ai"
	"github.com/tuannm99/testkit/testkit/core/config"
)

func TestComplete(t *testing.T) {
	p, err := New(&config.AI{Command: []string{"sh", "-c", "tr a-z A-Z"}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.Complete(context.Background(), ai.Request{System: "rules", Prompt: "task"})
	if err != nil || !strings.Contains(resp.Text, "RULES") || !strings.Contains(resp.Text, "TASK") {
		t.Fatalf("%+v %v", resp, err)
	}
	bad, _ := New(&config.AI{Command: []string{"sh", "-c", "echo boom >&2; exit 3"}})
	if _, err := bad.Complete(context.Background(), ai.Request{}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("%v", err)
	}
}

package orchestrator

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tuannm99/testkit/testkit/core/kit"
)

// slowTrigger takes a while per send and fails if its context is cancelled
// mid-send, like a Kafka produce waiting for the broker ack.
type slowTrigger struct {
	mu  sync.Mutex
	ids map[string]bool
}

func (t *slowTrigger) Enqueue(ctx context.Context, j kit.Job) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(30 * time.Millisecond):
	}
	t.mu.Lock()
	t.ids[j.ID] = true
	t.mu.Unlock()
	return nil
}

func (t *slowTrigger) Drain(context.Context) error { return nil }

func TestLoadWaitDeliversEveryJob(t *testing.T) {
	tr := &slowTrigger{ids: map[string]bool{}}
	e := &execution{trigger: tr}
	if _, err := e.startLoad(context.Background(), kit.Step{Name: "load.start", With: map[string]any{"rate": 100, "from": 1, "to": 20}}); err != nil {
		t.Fatal(err)
	}
	res := e.stopLoad(true, 10*time.Second)
	out := res.Output.(map[string]any)
	if len(tr.ids) != 20 || out["sent"] != int64(20) || out["errors"] != int64(0) {
		t.Fatalf("delivered %d, report %v (%s)", len(tr.ids), out, res.Note)
	}
	if err := e.loadError(true); err != nil {
		t.Fatal(err)
	}
}

func TestLoadStopFinishesStartedSends(t *testing.T) {
	tr := &slowTrigger{ids: map[string]bool{}}
	e := &execution{trigger: tr}
	if _, err := e.startLoad(context.Background(), kit.Step{Name: "load.start", With: map[string]any{"rate": 50, "from": 1, "to": 1000}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // let some arrivals start
	res := e.stopLoad(false, 0)
	out := res.Output.(map[string]any)
	tr.mu.Lock()
	n := len(tr.ids)
	tr.mu.Unlock()
	if n == 0 || n == 1000 || out["sent"] != int64(n) || out["errors"] != int64(0) {
		t.Fatalf("delivered %d, report %v", n, out)
	}
}

type failingTrigger struct{ slowTrigger }

func (t *failingTrigger) Enqueue(ctx context.Context, j kit.Job) error {
	if j.ID == "L3" {
		return context.DeadlineExceeded
	}
	return t.slowTrigger.Enqueue(ctx, j)
}

func TestLoadWaitErrorsWhenAJobIsNotSent(t *testing.T) {
	e := &execution{trigger: &failingTrigger{slowTrigger{ids: map[string]bool{}}}}
	if _, err := e.startLoad(context.Background(), kit.Step{With: map[string]any{"rate": 100, "from": 1, "to": 5}}); err != nil {
		t.Fatal(err)
	}
	e.stopLoad(true, 10*time.Second)
	if err := e.loadError(true); err == nil {
		t.Fatal("a lost job was not reported as a generator error")
	}
}

package orchestrator

import (
	"testing"

	"github.com/tuannm99/testkit/testkit/core/assert"
	"github.com/tuannm99/testkit/testkit/core/result"
)

func exec(trigger, check string, actual any) *result.Execution {
	return &result.Execution{CaseID: "TC-X", Trigger: trigger, Result: result.Pass,
		Assertions: []assert.Outcome{{ID: "A1", Check: check, Result: "pass", Actual: actual}}}
}

// Parity compares verdicts always, values only for deterministic checks: a recovery
// time of 1.6 s through Kafka and 5.1 s through a table poller is not a difference.
func TestParityIgnoresTimingValuesButNotVerdicts(t *testing.T) {
	timing := parity([]*result.Execution{exec("kafka", "experiment.recovery_seconds", 1.6), exec("db-poll", "experiment.recovery_seconds", 5.1)})
	if len(timing) != 1 || !timing[0].Match {
		t.Fatalf("timing values must not break parity: %+v", timing)
	}
	counts := parity([]*result.Execution{exec("kafka", "mock.payment.succeeded", 3), exec("redis", "mock.payment.succeeded", 4)})
	if len(counts) != 1 || counts[0].Match {
		t.Fatalf("a different count of charges must break parity: %+v", counts)
	}
	b := exec("redis", "experiment.recovery_seconds", 9.0)
	b.Assertions[0].Result = "fail"
	verdict := parity([]*result.Execution{exec("kafka", "experiment.recovery_seconds", 1.6), b})
	if len(verdict) != 1 || verdict[0].Match {
		t.Fatalf("a different verdict must break parity even for timing checks: %+v", verdict)
	}
}

package assert

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tuannm99/testkit/testkit/core/kit"
)

func TestCompare(t *testing.T) {
	cases := []struct {
		op       string
		act, exp any
		want     bool
	}{
		{"eq", int64(3), 3, true}, {"eq", "3", 3, true}, {"eq", "paid", "paid", true}, {"eq", nil, "x", false},
		{"ne", "a", "b", true}, {"gt", 5, 3, true}, {"lte", "2.5", 2.5, true},
		{"between", 5, []any{1, 10}, true}, {"between", 11, []any{1, 10}, false},
		{"in", "paid", []any{"paid", "failed"}, true}, {"not_in", "x", []any{"a"}, true},
		{"contains", "order paid o1", "paid", true}, {"contains", []any{"a", "b"}, "b", true},
		{"matches", "ch_o1", `^ch_o\d+$`, true}, {"exists", 0, nil, true}, {"not_exists", nil, nil, true},
		{"empty", []any{}, nil, true}, {"len_eq", []any{1, 2}, 2, true}, {"approx", 1.05, 1.0, true},
	}
	for _, c := range cases {
		got, err := Compare(c.op, c.act, c.exp, 0.1)
		if err != nil || got != c.want {
			t.Errorf("%s(%v, %v) = %v, %v; want %v", c.op, c.act, c.exp, got, err, c.want)
		}
	}
	if _, err := Compare("gt", "paid", 1, 0); err == nil {
		t.Error("gt on non-numbers must be an error, not a silent false")
	}
}

func TestParseCheck(t *testing.T) {
	r, err := ParseCheck("mock.payment.calls(status=201, path=/v1/charges)")
	if err != nil || len(r.Segments) != 3 || r.Segments[2].KV["status"] != "201" || r.Segments[2].KV["path"] != "/v1/charges" {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = ParseCheck("mail.to(customer).count")
	if err != nil || r.Segments[1].Args[0] != "customer" || r.Segments[2].Name != "count" {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = ParseCheck("kafka.topic(orders.dlq).count")
	if err != nil || r.Segments[1].Args[0] != "orders.dlq" {
		t.Fatalf("%+v %v", r, err)
	}
	for _, bad := range []string{"", "postgres", "a..b", "a.b(", "a.b)x"} {
		if _, err := ParseCheck(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestEventuallyPollsUntilPass(t *testing.T) {
	n := 0
	resolve := func(context.Context, kit.CheckRef) (kit.Observation, error) {
		n++
		return kit.Observation{Value: n, At: time.Now()}, nil
	}
	out := Eventually(context.Background(), []Expectation{{ID: "A1", Check: "x.y", Op: "gte", Expected: 3}}, resolve,
		Poll{Within: 2 * time.Second, Interval: time.Millisecond, MaxInt: time.Millisecond}, nil)
	if out[0].Result != "pass" || out[0].Attempts != 3 || out[0].FirstPass.IsZero() {
		t.Fatalf("%+v", out[0])
	}
}

func TestEventuallyTimesOutAndReportsError(t *testing.T) {
	resolve := func(context.Context, kit.CheckRef) (kit.Observation, error) {
		return kit.Observation{At: time.Now()}, errors.New("db down")
	}
	start := time.Now()
	out := Eventually(context.Background(), []Expectation{{ID: "A1", Check: "x.y", Op: "eq", Expected: 1}}, resolve,
		Poll{Within: 100 * time.Millisecond, Interval: 10 * time.Millisecond}, nil)
	if out[0].Result != "error" || time.Since(start) > time.Second {
		t.Fatalf("%+v", out[0])
	}
}

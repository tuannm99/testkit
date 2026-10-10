package infra

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/tuannm99/testkit/testkit/core/config"
)

func project(t *testing.T) *config.Project {
	_, file, _, _ := runtime.Caller(0)
	p, err := config.LoadProject(filepath.Join(filepath.Dir(file), "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveOnlyWhatServicesDeclare(t *testing.T) {
	p := project(t)
	svc, err := config.LoadService(p.Abs("testkit/services/order-worker.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	st := NewStack(p, NewDocker(nil), io.Discard)
	sel, err := st.Resolve(UpOptions{Profiles: []string{"core", "stores", "mocks"}, Services: []*config.Service{svc}})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(sel.Services, ",")
	if got != "clickhouse,elasticsearch,kafka,mailpit,mockhub,mongo,postgres,rabbitmq,redis" {
		t.Fatalf("selection = %s", got)
	}
	// A service declaring only Postgres + an HTTP mock must not start the other stores or Mailpit.
	small := &config.Service{Name: "small", Stores: config.Stores{Postgres: &config.PostgresStore{}},
		Mocks: map[string]config.MockSpec{"p": {Kind: "http"}}}
	sel, err = st.Resolve(UpOptions{Profiles: []string{"core", "stores", "mocks"}, Services: []*config.Service{small}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sel.Services, ","); got != "mockhub,postgres" {
		t.Fatalf("small selection = %s", got)
	}
	all, _ := st.Resolve(UpOptions{Profiles: []string{"stores"}})
	if len(all.Services) != 7 {
		t.Fatalf("all stores = %v", all.Services)
	}
	if _, err := st.Resolve(UpOptions{Profiles: []string{"nope"}}); err == nil {
		t.Fatal("unknown profile accepted")
	}
}

func TestPinCheckRejectsLatest(t *testing.T) {
	p := project(t)
	st := NewStack(p, NewDocker(nil), io.Discard)
	if c := st.pinCheck(); c.Status != OK {
		t.Fatalf("repo images must be pinned: %+v", c)
	}
	os.Setenv("REDIS_IMAGE", "redis:latest")
	defer os.Unsetenv("REDIS_IMAGE")
	if c := st.pinCheck(); c.Status != Fail || !strings.Contains(c.Detail, "REDIS_IMAGE") {
		t.Fatalf("latest not rejected: %+v", c)
	}
}

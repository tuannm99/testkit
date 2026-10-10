// Package contract holds the contract tests every trigger and store
// connector must pass. A new trigger (RabbitMQ, SQS, ...) is added to the
// table below and must pass the same suite.
//
// They run against the TestKit stack: TESTKIT_INTEGRATION=1 go test ./adapters/trigger/contract/
package contract

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	goredis "github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/tuannm99/testkit/testkit/adapters/store/postgres"
	"github.com/tuannm99/testkit/testkit/adapters/store/redis"
	"github.com/tuannm99/testkit/testkit/adapters/trigger/dbpoll"
	"github.com/tuannm99/testkit/testkit/adapters/trigger/kafka"
	"github.com/tuannm99/testkit/testkit/adapters/trigger/rabbitmq"
	"github.com/tuannm99/testkit/testkit/adapters/trigger/redisq"
	"github.com/tuannm99/testkit/testkit/core/config"
	"github.com/tuannm99/testkit/testkit/core/evidence"
	"github.com/tuannm99/testkit/testkit/core/kit"
)

// consumer simulates the service under test for one trigger: it takes every
// delivered job and acknowledges it the way the real service would.
type consumer func(ctx context.Context, env *kit.Env) (int, error)

type triggerCase struct {
	name       string
	factory    kit.Factory
	consume    consumer
	stores     []kit.Factory // store connectors the trigger needs besides Postgres and Kafka
	redisQueue string        // declared Redis queue the "redis" trigger uses
}

func project(t *testing.T) *config.Project {
	_, f, _, _ := runtime.Caller(0)
	p, err := config.LoadProject(filepath.Join(filepath.Dir(f), "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func env(t *testing.T, ns, redisQueue string) *kit.Env {
	p := project(t)
	dir := t.TempDir()
	mig := filepath.Join(dir, "mig")
	_ = os.MkdirAll(mig, 0o755)
	_ = os.WriteFile(filepath.Join(mig, "001.sql"), []byte(`CREATE TABLE jobs (id bigserial PRIMARY KEY, job_key text NOT NULL,
		status text NOT NULL DEFAULT 'queued');`), 0o644)
	svc := &config.Service{Name: "contract", Dir: dir,
		Stores: config.Stores{Postgres: &config.PostgresStore{Migrations: "mig", Snapshot: []string{"jobs"}},
			Kafka:    &config.KafkaStore{Topics: []config.KafkaTopic{{Name: "jobs", Partitions: 2}}, Groups: []string{"svc"}},
			RabbitMQ: &config.RabbitStore{Queues: []config.RabbitQueue{{Name: "jobs", DLQ: "jobs.dlq"}}},
			Redis: &config.RedisStore{Queues: []config.RedisQueue{
				{Name: "jobs-stream", Kind: "stream", Group: "svc", DLQ: "jobs-stream-dlq"},
				{Name: "jobs-list", Kind: "list", Processing: "jobs-list:processing"}}}},
		Triggers: map[string]config.TriggerSpec{
			"rabbitmq": {Queue: "jobs", Key: "{{ .job.id }}", Value: `{"id":"{{ .job.id }}"}`},
			"redis":    {Queue: redisQueue, Key: "{{ .job.id }}", Value: `{"id":"{{ .job.id }}"}`},
			"kafka":    {Topic: "jobs", Group: "svc", Key: "{{ .job.id }}", Value: `{"id":"{{ .job.id }}"}`},
			"db-poll":  {SQL: "INSERT INTO jobs (job_key) VALUES ('{{ .job.id }}')", Drained: "SELECT count(*) FROM jobs WHERE status <> 'done'"},
		}}
	ev, _ := evidence.Open(dir, "r00000000-000000-ctr")
	return &kit.Env{RunID: "r00000000-000000-ctr", NS: kit.Namespace(ns), CaseID: "TC-CONTRACT-1", Project: p, Service: svc,
		Runner: p.Endpoints(config.RunnerInNetwork()), Internal: p.Endpoints(true), Evidence: ev, CaseDir: "c"}
}

var triggers = []triggerCase{
	{name: "kafka", factory: kafka.NewTrigger, consume: func(ctx context.Context, env *kit.Env) (int, error) {
		cl, err := kgo.NewClient(kgo.SeedBrokers(env.Runner.KafkaBrokers...), kgo.ConsumerGroup(env.NS.Group("svc")),
			kgo.ConsumeTopics(env.NS.Topic("jobs")), kgo.DisableAutoCommit(), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
		if err != nil {
			return 0, err
		}
		defer cl.Close()
		n := 0
		for n < 4 && ctx.Err() == nil {
			f := cl.PollFetches(ctx)
			f.EachRecord(func(*kgo.Record) { n++ })
			if err := cl.CommitUncommittedOffsets(ctx); err != nil {
				return n, err
			}
		}
		return n, ctx.Err()
	}},
	{name: "db-poll", factory: dbpoll.New, consume: func(ctx context.Context, env *kit.Env) (int, error) {
		pool, err := postgres.Open(ctx, env.Runner.Postgres, env.NS.Database())
		if err != nil {
			return 0, err
		}
		defer pool.Close()
		tag, err := pool.Exec(ctx, "UPDATE jobs SET status = 'done' WHERE status = 'queued'")
		return int(tag.RowsAffected()), err
	}},
	{name: "rabbitmq", factory: rabbitmq.NewTrigger, stores: []kit.Factory{rabbitmq.New}, consume: func(ctx context.Context, env *kit.Env) (int, error) {
		conn, err := amqp.Dial(fmt.Sprintf("amqp://%s:%s@%s/%s", env.Runner.RabbitUser, env.Runner.RabbitPass, env.Runner.RabbitAMQP, env.NS))
		if err != nil {
			return 0, err
		}
		defer conn.Close()
		ch, err := conn.Channel()
		if err != nil {
			return 0, err
		}
		msgs, err := ch.Consume("jobs", "", false, false, false, false, nil)
		if err != nil {
			return 0, err
		}
		n := 0
		for n < 4 {
			select {
			case d := <-msgs:
				if err := d.Ack(false); err != nil {
					return n, err
				}
				n++
			case <-ctx.Done():
				return n, ctx.Err()
			}
		}
		return n, nil
	}},
	{name: "redis-stream", factory: redisq.New, stores: []kit.Factory{redis.New}, redisQueue: "jobs-stream",
		consume: func(ctx context.Context, env *kit.Env) (int, error) {
			cl := goredis.NewClient(&goredis.Options{Addr: env.Runner.Redis})
			defer cl.Close()
			key := env.NS.KeyPrefix() + "jobs-stream"
			n := 0
			for n < 4 && ctx.Err() == nil {
				res, err := cl.XReadGroup(ctx, &goredis.XReadGroupArgs{Group: "svc", Consumer: "c1", Streams: []string{key, ">"},
					Count: 4, Block: 500 * time.Millisecond}).Result()
				if err == goredis.Nil {
					continue
				}
				if err != nil {
					return n, err
				}
				for _, st := range res {
					for _, m := range st.Messages {
						if err := cl.XAck(ctx, key, "svc", m.ID).Err(); err != nil {
							return n, err
						}
						n++
					}
				}
			}
			return n, ctx.Err()
		}},
	{name: "redis-list", factory: redisq.New, stores: []kit.Factory{redis.New}, redisQueue: "jobs-list",
		consume: func(ctx context.Context, env *kit.Env) (int, error) {
			cl := goredis.NewClient(&goredis.Options{Addr: env.Runner.Redis})
			defer cl.Close()
			key, proc := env.NS.KeyPrefix()+"jobs-list", env.NS.KeyPrefix()+"jobs-list:processing"
			n := 0
			for n < 4 && ctx.Err() == nil {
				body, err := cl.BLMove(ctx, key, proc, "RIGHT", "LEFT", 500*time.Millisecond).Result()
				if err == goredis.Nil {
					continue
				}
				if err != nil {
					return n, err
				}
				if err := cl.LRem(ctx, proc, 1, body).Err(); err != nil {
					return n, err
				}
				n++
			}
			return n, ctx.Err()
		}},
}

func requireStack(t *testing.T) {
	if os.Getenv("TESTKIT_INTEGRATION") != "1" {
		t.Skip("contract tests need the TestKit stack: TESTKIT_INTEGRATION=1 (after testkit up --services order-worker)")
	}
}

// TestTriggerContract: Enqueue delivers every job (duplicates included),
// Drain blocks while something is pending and returns once everything is
// acknowledged, Teardown removes the namespace.
func TestTriggerContract(t *testing.T) {
	requireStack(t)
	for i, tc := range triggers {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			e := env(t, fmt.Sprintf("tk_contract_%d_%d", time.Now().Unix()%100000, i), tc.redisQueue)
			pg, kf := postgres.New(), kafka.New()
			for _, c := range []kit.Connector{pg, kf} {
				if err := c.Provision(ctx, e); err != nil {
					t.Fatalf("provision %s: %v", c.Name(), err)
				}
			}
			for _, f := range tc.stores {
				c := f()
				if err := c.Provision(ctx, e); err != nil {
					t.Fatalf("provision %s: %v", c.Name(), err)
				}
				defer c.Teardown(context.Background()) //nolint:errcheck
			}
			defer func() {
				_ = kf.Teardown(context.Background())
				_ = pg.Teardown(context.Background())
				assertGone(t, e)
			}()
			tr := tc.factory().(kit.TriggerConnector)
			if err := tr.Provision(ctx, e); err != nil {
				t.Fatal(err)
			}
			defer tr.Teardown(context.Background()) //nolint:errcheck

			for _, j := range []kit.Job{{ID: "j1"}, {ID: "j2"}, {ID: "j3", Duplicate: 2}} {
				if err := tr.Enqueue(ctx, j); err != nil {
					t.Fatalf("enqueue %s: %v", j.ID, err)
				}
			}
			short, c2 := context.WithTimeout(ctx, 700*time.Millisecond)
			if err := tr.Drain(short); err == nil {
				t.Fatal("Drain returned while 4 deliveries were pending")
			}
			c2()
			cctx, c3 := context.WithTimeout(ctx, 30*time.Second)
			n, err := tc.consume(cctx, e)
			c3()
			if err != nil || n != 4 {
				t.Fatalf("consumer saw %d deliveries (want 4: 3 jobs + 1 duplicate): %v", n, err)
			}
			dctx, c4 := context.WithTimeout(ctx, 30*time.Second)
			defer c4()
			if err := tr.Drain(dctx); err != nil {
				t.Fatalf("Drain after acknowledging everything: %v", err)
			}
		})
	}
}

// assertGone checks that the store connectors removed the namespace.
func assertGone(t *testing.T, e *kit.Env) {
	ctx := context.Background()
	admin, err := postgres.Open(ctx, e.Runner.Postgres, "testkit")
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	var n int
	_ = admin.QueryRow(ctx, "SELECT count(*) FROM pg_database WHERE datname = $1", e.NS.Database()).Scan(&n)
	if n != 0 {
		t.Errorf("database %s still exists after teardown", e.NS.Database())
	}
	cl, err := kgo.NewClient(kgo.SeedBrokers(e.Runner.KafkaBrokers...))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	cl.ForceMetadataRefresh()
	topics, err := listTopics(ctx, cl)
	if err == nil {
		for _, tp := range topics {
			if tp == e.NS.Topic("jobs") {
				t.Errorf("topic %s still exists after teardown", tp)
			}
		}
	}
}

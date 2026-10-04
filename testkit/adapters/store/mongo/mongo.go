// Package mongo is the MongoDB connector: one database per namespace (the
// stack runs a single-node replica set so services may use transactions and
// change streams), fixtures, checks, collection dumps, drop at teardown.
package mongo

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/tuannm99/testkit/testkit/core/kit"
)

type Connector struct {
	env *kit.Env
	cl  *mongo.Client
	db  *mongo.Database
}

func New() kit.Connector { return &Connector{} }

func (c *Connector) Name() string            { return "mongo" }
func (c *Connector) CheckPrefixes() []string { return []string{"mongo"} }

func (c *Connector) Provision(ctx context.Context, env *kit.Env) error {
	c.env = env
	cl, err := mongo.Connect(options.Client().ApplyURI(env.Runner.Mongo).SetServerSelectionTimeout(10 * time.Second))
	if err != nil {
		return err
	}
	c.cl, c.db = cl, cl.Database(env.NS.Database())
	return cl.Ping(ctx, nil)
}

func (c *Connector) Health(ctx context.Context) error { return c.cl.Ping(ctx, nil) }

func (c *Connector) collection(entity string) (string, string, error) {
	if e, ok := c.env.Service.Entities[entity]; ok && e.Mongo != nil {
		key := e.Mongo.Key
		if key == "" {
			key = "_id"
		}
		return e.Mongo.Table, key, nil
	}
	return "", "", fmt.Errorf("entity %q has no mongo mapping in service %s", entity, c.env.Service.Name)
}

func (c *Connector) Apply(ctx context.Context, s kit.Step) (kit.Result, error) {
	switch s.Name {
	case "mongo.insert":
		coll, _, err := c.collection(kit.Str(s.With, "entity"))
		if err != nil {
			return kit.Result{}, err
		}
		rows, _ := s.With["rows"].([]any)
		if len(rows) == 0 {
			return kit.Result{}, nil
		}
		_, err = c.db.Collection(coll).InsertMany(ctx, rows)
		return kit.Result{Note: fmt.Sprintf("%d document(s) into %s", len(rows), coll)}, err
	}
	return kit.Result{}, fmt.Errorf("mongo: unknown step %s", s.Name)
}

// typed turns a filter value written in a check into the most likely BSON type.
func typed(v string) any {
	if i, err := strconv.ParseInt(v, 10, 64); err == nil {
		return i
	}
	if b, err := strconv.ParseBool(v); err == nil {
		return b
	}
	return v
}

// Check resolves:
//
//	mongo.<entity>.count(field=v)     documents matching equality filters
//	mongo.<entity>.<key>.<field>      a field of the document with that key
//	mongo.<entity>.<key>.exists       document exists
func (c *Connector) Check(ctx context.Context, ref kit.CheckRef) (kit.Observation, error) {
	segs := ref.Segments
	at := time.Now().UTC()
	coll, key, err := c.collection(segs[1].Name)
	if err != nil {
		return kit.Observation{At: at}, err
	}
	if len(segs) < 3 {
		return kit.Observation{At: at}, fmt.Errorf("expected mongo.<entity>.count or mongo.<entity>.<key>.<field>")
	}
	if segs[2].Name == "count" {
		filter := bson.D{}
		keys := make([]string, 0, len(segs[2].KV))
		for k := range segs[2].KV {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			filter = append(filter, bson.E{Key: k, Value: typed(segs[2].KV[k])})
		}
		n, err := c.db.Collection(coll).CountDocuments(ctx, filter)
		return kit.Observation{Value: n, Source: fmt.Sprintf("db.%s.countDocuments(%v) in %s", coll, filter, c.db.Name()), At: time.Now().UTC()}, err
	}
	var doc bson.M
	err = c.db.Collection(coll).FindOne(ctx, bson.D{{Key: key, Value: segs[2].Name}}).Decode(&doc)
	src := fmt.Sprintf("db.%s.findOne({%s: %q}) in %s", coll, key, segs[2].Name, c.db.Name())
	at = time.Now().UTC()
	if err == mongo.ErrNoDocuments {
		if len(segs) == 4 && segs[3].Name == "exists" {
			return kit.Observation{Value: false, Source: src, At: at}, nil
		}
		return kit.Observation{Value: nil, Source: src, At: at}, nil
	}
	if err != nil {
		return kit.Observation{Source: src, At: at}, err
	}
	plain := toPlain(doc)
	if len(segs) == 3 {
		return kit.Observation{Value: plain, Raw: plain, Source: src, At: at}, nil
	}
	if segs[3].Name == "exists" {
		return kit.Observation{Value: true, Raw: plain, Source: src, At: at}, nil
	}
	return kit.Observation{Value: plain[segs[3].Name], Raw: plain, Source: src + " → ." + segs[3].Name, At: at}, nil
}

// toPlain converts BSON to JSON-friendly values (relaxed extended JSON).
func toPlain(doc bson.M) map[string]any {
	raw, err := bson.MarshalExtJSON(doc, false, false)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

// Collect dumps the snapshot collections.
func (c *Connector) Collect(ctx context.Context, _ kit.TimeWindow) ([]kit.Artifact, error) {
	if c.db == nil || c.env.Service.Stores.Mongo == nil {
		return nil, nil
	}
	var out []kit.Artifact
	for _, coll := range c.env.Service.Stores.Mongo.Snapshot {
		cur, err := c.db.Collection(coll).Find(ctx, bson.D{}, options.Find().SetLimit(1000))
		if err != nil {
			return out, err
		}
		var docs []map[string]any
		for cur.Next(ctx) {
			var d bson.M
			if cur.Decode(&d) == nil {
				docs = append(docs, toPlain(d))
			}
		}
		cur.Close(ctx)
		rel := path.Join(c.env.CaseDir, "output", "mongo", coll+".json")
		if _, err := c.env.Evidence.WriteJSON(rel, map[string]any{"database": c.db.Name(), "collection": coll, "documents": docs}); err != nil {
			return out, err
		}
		out = append(out, kit.Artifact{Kind: "snapshot", Path: rel, Title: fmt.Sprintf("Mongo %s (%d docs)", coll, len(docs)), Source: "mongo"})
	}
	return out, nil
}

func (c *Connector) Teardown(ctx context.Context) error {
	if c.cl == nil {
		return nil
	}
	defer c.cl.Disconnect(context.Background()) //nolint:errcheck
	return c.db.Drop(ctx)
}

var (
	_ kit.Connector = (*Connector)(nil)
	_ kit.Checker   = (*Connector)(nil)
)

// Package audit records an audit document per order event in MongoDB. The
// document id is the event id: a retried job hits E11000 (duplicate key),
// which means "already recorded" and is a success.
package audit

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/tuannm99/testkit/reference-worker/internal/domain"
	"github.com/tuannm99/testkit/reference-worker/internal/failpoint"
)

type Mongo struct {
	Client *mongo.Client
	DB     string
}

func Open(ctx context.Context, uri string) (*mongo.Client, error) {
	if uri == "" {
		return nil, nil
	}
	return mongo.Connect(options.Client().ApplyURI(uri).SetServerSelectionTimeout(10 * time.Second))
}

func (m *Mongo) Enabled() bool { return m != nil && m.Client != nil }

// Record inserts the audit document; E11000 means it was recorded before.
func (m *Mongo) Record(ctx context.Context, eventID, typ string, o domain.Order) error {
	if !m.Enabled() {
		return nil
	}
	_, err := m.Client.Database(m.DB).Collection("audit").InsertOne(ctx, map[string]any{
		"_id": eventID, "order_id": o.ID, "type": typ, "amount_cents": o.AmountCents, "at": time.Now().UTC(),
	})
	if mongo.IsDuplicateKeyError(err) && !failpoint.Enabled(failpoint.MongoDupIsError) {
		return nil // already recorded by an earlier attempt
	}
	if err != nil {
		return fmt.Errorf("audit insert: %w", err)
	}
	return nil
}

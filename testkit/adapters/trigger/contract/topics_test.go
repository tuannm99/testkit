package contract

import (
	"context"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
)

func listTopics(ctx context.Context, cl *kgo.Client) ([]string, error) {
	td, err := kadm.NewClient(cl).ListTopics(ctx)
	if err != nil {
		return nil, err
	}
	return td.Names(), nil
}

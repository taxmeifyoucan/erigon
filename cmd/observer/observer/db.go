package observer

import (
	"context"
	"github.com/ledgerwatch/erigon/p2p/enode"
	"time"
)

type DB interface {
	UpsertNode(ctx context.Context, node *enode.Node) error
	FindCandidates(ctx context.Context, minUnusedDuration time.Duration, limit uint) ([]*enode.Node, error)
	MarkTakenNodes(ctx context.Context, nodes []*enode.Node) error

	// TakeCandidates runs FindCandidates + MarkTakenNodes in a transaction.
	TakeCandidates(ctx context.Context, minUnusedDuration time.Duration, limit uint) ([]*enode.Node, error)

	IsConflictError(err error) bool
}
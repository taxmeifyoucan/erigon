package database

import (
	"context"
	"net"
	"time"
)

type NodeID string

type NodeAddr1 struct {
	IP       net.IP
	PortDisc uint16
	PortRLPx uint16
}

type NodeAddr struct {
	NodeAddr1
	IPv6 NodeAddr1
}

type HandshakeError struct {
	StringCode string
	Time       time.Time
}

type DB interface {
	UpsertNodeAddr(ctx context.Context, id NodeID, addr NodeAddr) error
	FindNodeAddr(ctx context.Context, id NodeID) (*NodeAddr, error)

	ResetPingError(ctx context.Context, id NodeID) error
	UpdatePingError(ctx context.Context, id NodeID) error

	UpdateClientID(ctx context.Context, id NodeID, clientID string) error
	InsertHandshakeError(ctx context.Context, id NodeID, handshakeErr string) error
	DeleteHandshakeErrors(ctx context.Context, id NodeID) error
	FindHandshakeLastErrors(ctx context.Context, id NodeID, limit uint) ([]HandshakeError, error)
	UpdateHandshakeRetryTime(ctx context.Context, id NodeID, retryTime time.Time) error
	FindHandshakeRetryTime(ctx context.Context, id NodeID) (*time.Time, error)
	FindHandshakeCandidates(ctx context.Context, limit uint) ([]NodeID, error)
	MarkTakenHandshakeCandidates(ctx context.Context, nodes []NodeID) error
	// TakeHandshakeCandidates runs FindHandshakeCandidates + MarkTakenHandshakeCandidates in a transaction.
	TakeHandshakeCandidates(ctx context.Context, limit uint) ([]NodeID, error)

	UpdateForkCompatibility(ctx context.Context, id NodeID, isCompatFork bool) error

	UpdateNeighborBucketKeys(ctx context.Context, id NodeID, keys []string) error
	FindNeighborBucketKeys(ctx context.Context, id NodeID) ([]string, error)

	FindCandidates(ctx context.Context, minUnusedDuration time.Duration, maxPingTries uint, limit uint) ([]NodeID, error)
	MarkTakenNodes(ctx context.Context, nodes []NodeID) error
	// TakeCandidates runs FindCandidates + MarkTakenNodes in a transaction.
	TakeCandidates(ctx context.Context, minUnusedDuration time.Duration, maxPingTries uint, limit uint) ([]NodeID, error)

	IsConflictError(err error) bool

	CountNodes(ctx context.Context) (uint, error)
	CountCompatibleNodes(ctx context.Context) (uint, error)
	CountIPs(ctx context.Context) (uint, error)
	EnumerateClientIDs(ctx context.Context, enumFunc func(clientID *string)) error
}

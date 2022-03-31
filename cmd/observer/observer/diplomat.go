package observer

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"github.com/ledgerwatch/erigon/cmd/observer/database"
	"github.com/ledgerwatch/erigon/p2p/enode"
	"github.com/ledgerwatch/log/v3"
	"time"
)

type Diplomat struct {
	node *enode.Node

	privateKey              *ecdsa.PrivateKey
	handshakeLastTry        *database.HandshakeTry
	handshakeRefreshTimeout time.Duration

	log log.Logger
}

func NewDiplomat(
	node *enode.Node,
	privateKey *ecdsa.PrivateKey,
	handshakeLastTry *database.HandshakeTry,
	handshakeRefreshTimeout time.Duration,
	logger log.Logger,
) *Diplomat {
	instance := Diplomat{
		node,
		privateKey,
		handshakeLastTry,
		handshakeRefreshTimeout,
		logger,
	}
	return &instance
}

func (diplomat *Diplomat) Run(ctx context.Context) (*string, *HandshakeError) {
	if !diplomat.canRetryHandshake() {
		return nil, nil
	}

	return diplomat.tryRequestClientID(ctx)
}

func (diplomat *Diplomat) handshake(ctx context.Context) (*HelloMessage, *HandshakeError) {
	node := diplomat.node
	return Handshake(ctx, node.IP(), node.TCP(), node.Pubkey(), diplomat.privateKey)
}

func (diplomat *Diplomat) tryRequestClientID(ctx context.Context) (*string, *HandshakeError) {
	diplomat.log.Debug("Handshaking with a node")
	hello, handshakeErr := diplomat.handshake(ctx)

	if (handshakeErr != nil) && !errors.Is(handshakeErr, context.Canceled) {
		diplomat.log.Debug("Failed to handshake", "err", handshakeErr)
		return nil, handshakeErr
	}

	clientID := &hello.ClientID
	diplomat.log.Debug("Got client ID", "clientID", *clientID)
	return clientID, nil
}

func (diplomat *Diplomat) canRetryHandshake() bool {
	if (diplomat.handshakeLastTry != nil) && !diplomat.handshakeLastTry.HasFailed {
		return time.Since(diplomat.handshakeLastTry.Time) > diplomat.handshakeRefreshTimeout
	}
	return true
}

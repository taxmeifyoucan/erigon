package observer

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"github.com/ledgerwatch/erigon/cmd/observer/database"
	"github.com/ledgerwatch/erigon/p2p"
	"github.com/ledgerwatch/erigon/p2p/enode"
	"github.com/ledgerwatch/log/v3"
	"time"
)

type Diplomat struct {
	node       *enode.Node
	privateKey *ecdsa.PrivateKey

	handshakeLastErrors     []database.HandshakeError
	handshakeRefreshTimeout time.Duration
	handshakeRetryDelay     time.Duration
	handshakeMaxTries       uint

	log log.Logger
}

func NewDiplomat(
	node *enode.Node,
	privateKey *ecdsa.PrivateKey,
	handshakeLastErrors []database.HandshakeError,
	handshakeRefreshTimeout time.Duration,
	handshakeRetryDelay time.Duration,
	handshakeMaxTries uint,
	logger log.Logger,
) *Diplomat {
	instance := Diplomat{
		node,
		privateKey,
		handshakeLastErrors,
		handshakeRefreshTimeout,
		handshakeRetryDelay,
		handshakeMaxTries,
		logger,
	}
	return &instance
}

func (diplomat *Diplomat) Run(ctx context.Context) (*string, *HandshakeError) {
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

func (diplomat *Diplomat) NextRetryTime(handshakeErr *HandshakeError) time.Time {
	return time.Now().Add(diplomat.NextRetryDelay(handshakeErr))
}

func (diplomat *Diplomat) NextRetryDelay(handshakeErr *HandshakeError) time.Duration {
	if handshakeErr == nil {
		return diplomat.handshakeRefreshTimeout
	}

	dbHandshakeErr := database.HandshakeError{
		StringCode: handshakeErr.StringCode(),
		Time:       time.Now(),
	}

	lastErrors := append([]database.HandshakeError{dbHandshakeErr}, diplomat.handshakeLastErrors...)

	if uint(len(lastErrors)) < diplomat.handshakeMaxTries {
		return diplomat.handshakeRetryDelay
	}

	if containsHandshakeError(diplomat.transientError(), lastErrors) {
		return diplomat.handshakeRetryDelay
	}

	if len(lastErrors) < 2 {
		return 1000000 * time.Hour // never
	}

	backOffDelay := 2 * lastErrors[0].Time.Sub(lastErrors[1].Time)
	if backOffDelay < diplomat.handshakeRetryDelay {
		return diplomat.handshakeRetryDelay
	}

	return backOffDelay
}

func (diplomat *Diplomat) transientError() *HandshakeError {
	return NewHandshakeError(HandshakeErrorIDDisconnect, p2p.DiscTooManyPeers, uint64(p2p.DiscTooManyPeers))
}

func containsHandshakeError(target *HandshakeError, list []database.HandshakeError) bool {
	for _, err := range list {
		if err.StringCode == target.StringCode() {
			return true
		}
	}
	return false
}

package observer

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"github.com/ledgerwatch/erigon/cmd/observer/database"
	"github.com/ledgerwatch/erigon/cmd/observer/utils"
	"github.com/ledgerwatch/log/v3"
	"golang.org/x/sync/semaphore"
	"sync/atomic"
	"time"
)

type Diplomacy struct {
	db        database.DBRetrier
	saveQueue *utils.TaskQueue

	privateKey        *ecdsa.PrivateKey
	concurrencyLimit  uint
	refreshTimeout    time.Duration
	retryDelay        time.Duration
	maxHandshakeTries uint
	transientError    *HandshakeError

	statusLogPeriod time.Duration
	log             log.Logger
}

func NewDiplomacy(
	db database.DBRetrier,
	saveQueue *utils.TaskQueue,
	privateKey *ecdsa.PrivateKey,
	concurrencyLimit uint,
	refreshTimeout time.Duration,
	retryDelay time.Duration,
	maxHandshakeTries uint,
	transientError *HandshakeError,
	statusLogPeriod time.Duration,
	logger log.Logger,
) *Diplomacy {
	instance := Diplomacy{
		db,
		saveQueue,
		privateKey,
		concurrencyLimit,
		refreshTimeout,
		retryDelay,
		maxHandshakeTries,
		transientError,
		statusLogPeriod,
		logger,
	}
	return &instance
}

func (diplomacy *Diplomacy) startSelectCandidates(ctx context.Context) <-chan database.NodeID {
	candidatesChan := make(chan database.NodeID)
	go func() {
		err := diplomacy.selectCandidates(ctx, candidatesChan)
		if (err != nil) && !errors.Is(err, context.Canceled) {
			diplomacy.log.Error("Failed to select handshake candidates", "err", err)
		}
		close(candidatesChan)
	}()
	return candidatesChan
}

func (diplomacy *Diplomacy) selectCandidates(ctx context.Context, candidatesChan chan<- database.NodeID) error {
	for ctx.Err() == nil {
		candidates, err := diplomacy.db.TakeHandshakeCandidates(
			ctx,
			diplomacy.refreshTimeout,
			diplomacy.retryDelay,
			diplomacy.maxHandshakeTries,
			diplomacy.transientError.StringCode(),
			diplomacy.concurrencyLimit)
		if err != nil {
			if diplomacy.db.IsConflictError(err) {
				diplomacy.log.Warn("Failed to take handshake candidates", "err", err)
			} else {
				return err
			}
		}

		if len(candidates) == 0 {
			utils.Sleep(ctx, 1*time.Second)
		}

		for _, id := range candidates {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case candidatesChan <- id:
			}
		}
	}

	return ctx.Err()
}

func (diplomacy *Diplomacy) Run(ctx context.Context) error {
	candidatesChan := diplomacy.startSelectCandidates(ctx)
	sem := semaphore.NewWeighted(int64(diplomacy.concurrencyLimit))

	count := 0
	statusLogDate := time.Now()
	clientIDCountPtr := new(uint64)

	for id := range candidatesChan {
		if err := sem.Acquire(ctx, 1); err != nil {
			if !errors.Is(err, context.Canceled) {
				return fmt.Errorf("failed to acquire semaphore: %w", err)
			} else {
				break
			}
		}

		count++
		if time.Since(statusLogDate) > diplomacy.statusLogPeriod {
			clientIDCount := atomic.LoadUint64(clientIDCountPtr)
			diplomacy.log.Info("Handshaking", "count", count, "clientIDCount", clientIDCount)
			statusLogDate = time.Now()
		}

		nodeAddr, err := diplomacy.db.FindNodeAddr(ctx, id)
		if err != nil {
			if diplomacy.db.IsConflictError(err) {
				diplomacy.log.Warn("Failed to get the node address", "err", err)
				sem.Release(1)
				continue
			}
			return fmt.Errorf("failed to get the node address: %w", err)
		}

		node, err := makeNodeFromAddr(id, *nodeAddr)
		if err != nil {
			return fmt.Errorf("failed to make node from node address: %w", err)
		}

		nodeDesc := node.URLv4()
		logger := diplomacy.log.New("node", nodeDesc)

		handshakeLastTry, err := diplomacy.db.FindHandshakeLastTry(ctx, id)
		if err != nil {
			if diplomacy.db.IsConflictError(err) {
				diplomacy.log.Warn("Failed to get handshake last try", "err", err)
				sem.Release(1)
				continue
			}
			return fmt.Errorf("failed to get handshake last try: %w", err)
		}

		diplomat := NewDiplomat(
			node,
			diplomacy.privateKey,
			handshakeLastTry,
			diplomacy.refreshTimeout,
			logger)

		go func(id database.NodeID) {
			defer sem.Release(1)

			clientID, handshakeErr := diplomat.Run(ctx)

			if clientID != nil {
				atomic.AddUint64(clientIDCountPtr, 1)
			}

			var isCompatFork *bool
			if (clientID != nil) && IsClientIDBlacklisted(*clientID) {
				isCompatFork = new(bool)
				*isCompatFork = false
			}

			diplomacy.saveQueue.EnqueueTask(ctx, func(ctx context.Context) error {
				return diplomacy.saveDiplomatResult(ctx, id, clientID, handshakeErr, isCompatFork)
			})
		}(id)
	}
	return nil
}

func (diplomacy *Diplomacy) saveDiplomatResult(
	ctx context.Context,
	id database.NodeID,
	clientID *string,
	handshakeErr *HandshakeError,
	isCompatFork *bool,
) error {
	if clientID != nil {
		dbErr := diplomacy.db.UpdateClientID(ctx, id, *clientID)
		if dbErr != nil {
			return dbErr
		}
	}

	if handshakeErr != nil {
		dbErr := diplomacy.db.UpdateHandshakeError(ctx, id, handshakeErr.StringCode())
		if dbErr != nil {
			return dbErr
		}
	}

	if isCompatFork != nil {
		dbErr := diplomacy.db.UpdateForkCompatibility(ctx, id, *isCompatFork)
		if dbErr != nil {
			return dbErr
		}
	}

	return nil
}

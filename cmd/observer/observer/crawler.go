package observer

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"github.com/ledgerwatch/erigon/cmd/observer/database"
	"github.com/ledgerwatch/erigon/cmd/observer/utils"
	"github.com/ledgerwatch/erigon/core/forkid"
	"github.com/ledgerwatch/erigon/p2p/enode"
	"github.com/ledgerwatch/erigon/params"
	"github.com/ledgerwatch/log/v3"
	"golang.org/x/sync/semaphore"
	"sync/atomic"
	"time"
)

type Crawler struct {
	transport  DiscV4Transport
	db         database.DBRetrier
	saveQueue  *utils.TaskQueue
	config     CrawlerConfig
	forkFilter forkid.Filter
	diplomacy  *Diplomacy
	log        log.Logger
}

type CrawlerConfig struct {
	Chain            string
	Bootnodes        []*enode.Node
	PrivateKey       *ecdsa.PrivateKey
	ConcurrencyLimit uint
	RefreshTimeout   time.Duration
	MaxPingTries     uint
	StatusLogPeriod  time.Duration

	HandshakeRefreshTimeout time.Duration
	MaxHandshakeTries       uint

	KeygenTimeout     time.Duration
	KeygenConcurrency uint
}

func NewCrawler(
	transport DiscV4Transport,
	db database.DB,
	config CrawlerConfig,
	logger log.Logger,
) (*Crawler, error) {
	saveQueueLogFuncProvider := func(err error) func(msg string, ctx ...interface{}) {
		if db.IsConflictError(err) {
			return logger.Warn
		}
		return logger.Error
	}
	saveQueue := utils.NewTaskQueue("Crawler.saveQueue", config.ConcurrencyLimit*2, saveQueueLogFuncProvider)

	chain := config.Chain
	chainConfig := params.ChainConfigByChainName(chain)
	genesisHash := params.GenesisHashByChainName(chain)
	if (chainConfig == nil) || (genesisHash == nil) {
		return nil, fmt.Errorf("unknown chain %s", chain)
	}

	forkFilter := forkid.NewStaticFilter(chainConfig, *genesisHash)

	diplomacy := NewDiplomacy(
		database.NewDBRetrier(db, logger),
		saveQueue,
		config.PrivateKey,
		config.ConcurrencyLimit,
		config.HandshakeRefreshTimeout,
		4*time.Hour,
		config.MaxHandshakeTries,
		config.StatusLogPeriod,
		logger)

	instance := Crawler{
		transport,
		database.NewDBRetrier(db, logger),
		saveQueue,
		config,
		forkFilter,
		diplomacy,
		logger,
	}
	return &instance, nil
}

func (crawler *Crawler) startSaveQueue(ctx context.Context) {
	go crawler.saveQueue.Run(ctx)
}

func (crawler *Crawler) startDiplomacy(ctx context.Context) {
	go func() {
		err := crawler.diplomacy.Run(ctx)
		if (err != nil) && !errors.Is(err, context.Canceled) {
			crawler.log.Error("Diplomacy has failed", "err", err)
		}
	}()
}

type candidateNode struct {
	id   database.NodeID
	node *enode.Node
}

func (crawler *Crawler) startSelectCandidates(ctx context.Context) <-chan candidateNode {
	nodes := make(chan candidateNode)
	go func() {
		err := crawler.selectCandidates(ctx, nodes)
		if (err != nil) && !errors.Is(err, context.Canceled) {
			crawler.log.Error("Failed to select candidates", "err", err)
		}
		close(nodes)
	}()
	return nodes
}

func (crawler *Crawler) selectCandidates(ctx context.Context, nodes chan<- candidateNode) error {
	for _, node := range crawler.config.Bootnodes {
		id, err := nodeID(node)
		if err != nil {
			return fmt.Errorf("failed to get a bootnode ID: %w", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case nodes <- candidateNode{id, node}:
		}
	}

	for ctx.Err() == nil {
		candidates, err := crawler.db.TakeCandidates(
			ctx,
			crawler.config.RefreshTimeout,
			crawler.config.MaxPingTries,
			crawler.config.MaxHandshakeTries,
			crawler.config.ConcurrencyLimit)
		if err != nil {
			if crawler.db.IsConflictError(err) {
				crawler.log.Warn("Failed to take candidates", "err", err)
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
			case nodes <- candidateNode{id, nil}:
			}
		}
	}

	return ctx.Err()
}

func (crawler *Crawler) Run(ctx context.Context) error {
	crawler.startSaveQueue(ctx)
	crawler.startDiplomacy(ctx)

	nodes := crawler.startSelectCandidates(ctx)
	sem := semaphore.NewWeighted(int64(crawler.config.ConcurrencyLimit))
	// allow only 1 keygen at a time
	keygenSem := semaphore.NewWeighted(int64(1))

	crawledCount := 0
	crawledCountLogDate := time.Now()
	foundPeersCountPtr := new(uint64)

	for candidate := range nodes {
		if err := sem.Acquire(ctx, 1); err != nil {
			if !errors.Is(err, context.Canceled) {
				return fmt.Errorf("failed to acquire semaphore: %w", err)
			} else {
				break
			}
		}

		crawledCount++
		if time.Since(crawledCountLogDate) > crawler.config.StatusLogPeriod {
			foundPeersCount := atomic.LoadUint64(foundPeersCountPtr)
			crawler.log.Info("Crawling", "crawledCount", crawledCount, "foundPeersCount", foundPeersCount)
			crawledCountLogDate = time.Now()
		}

		id := candidate.id
		node := candidate.node

		if node == nil {
			nodeAddr, err := crawler.db.FindNodeAddr(ctx, id)
			if err != nil {
				if crawler.db.IsConflictError(err) {
					crawler.log.Warn("Failed to get the node address", "err", err)
					sem.Release(1)
					continue
				}
				return fmt.Errorf("failed to get the node address: %w", err)
			}

			node, err = makeNodeFromAddr(id, *nodeAddr)
			if err != nil {
				return fmt.Errorf("failed to make node from node address: %w", err)
			}
		}

		nodeDesc := node.URLv4()
		logger := crawler.log.New("node", nodeDesc)

		handshakeLastTry, err := crawler.db.FindHandshakeLastTry(ctx, id)
		if err != nil {
			if crawler.db.IsConflictError(err) {
				crawler.log.Warn("Failed to get handshake last try", "err", err)
				sem.Release(1)
				continue
			}
			return fmt.Errorf("failed to get handshake last try: %w", err)
		}

		diplomat := NewDiplomat(
			node,
			crawler.config.PrivateKey,
			handshakeLastTry,
			crawler.config.HandshakeRefreshTimeout,
			logger)

		keygenCachedHexKeys, err := crawler.db.FindNeighborBucketKeys(ctx, id)
		if err != nil {
			if crawler.db.IsConflictError(err) {
				crawler.log.Warn("Failed to get neighbor bucket keys", "err", err)
				sem.Release(1)
				continue
			}
			return fmt.Errorf("failed to get neighbor bucket keys: %w", err)
		}
		keygenCachedKeys, err := parseHexPublicKeys(keygenCachedHexKeys)
		if err != nil {
			return fmt.Errorf("failed to parse cached neighbor bucket keys: %w", err)
		}

		interrogator, err := NewInterrogator(
			node,
			crawler.transport,
			crawler.forkFilter,
			diplomat,
			crawler.config.KeygenTimeout,
			crawler.config.KeygenConcurrency,
			keygenSem,
			keygenCachedKeys,
			logger)
		if err != nil {
			return fmt.Errorf("failed to create Interrogator for node %s: %w", nodeDesc, err)
		}

		go func() {
			defer sem.Release(1)

			result, err := interrogator.Run(ctx)

			isPingError := (err != nil) && (err.id == InterrogationErrorPing)

			var isCompatFork *bool
			if result != nil {
				isCompatFork = result.IsCompatFork
			} else if (err != nil) &&
				((err.id == InterrogationErrorIncompatibleForkID) ||
					(err.id == InterrogationErrorBlacklistedClientID)) {
				isCompatFork = new(bool)
				*isCompatFork = false
			}

			var clientID *string
			if result != nil {
				clientID = result.ClientID
			} else if (err != nil) && (err.id == InterrogationErrorBlacklistedClientID) {
				clientID = new(string)
				*clientID = err.wrappedErr.Error()
			}

			if err != nil {
				if !errors.Is(err, context.Canceled) {
					var logFunc func(msg string, ctx ...interface{})
					switch err.id {
					case InterrogationErrorPing:
						fallthrough
					case InterrogationErrorIncompatibleForkID:
						fallthrough
					case InterrogationErrorBlacklistedClientID:
						fallthrough
					case InterrogationErrorFindNodeTimeout:
						logFunc = logger.Debug
					default:
						logFunc = logger.Warn
					}
					logFunc("Failed to interrogate node", "err", err)
				}
			}

			if result != nil {
				peers := result.Peers
				logger.Debug(fmt.Sprintf("Got %d peers", len(peers)))
				atomic.AddUint64(foundPeersCountPtr, uint64(len(peers)))
			}

			crawler.saveQueue.EnqueueTask(ctx, func(ctx context.Context) error {
				return crawler.saveInterrogationResult(ctx, id, result, isPingError, isCompatFork, clientID)
			})
		}()
	}
	return nil
}

func (crawler *Crawler) saveInterrogationResult(
	ctx context.Context,
	id database.NodeID,
	result *InterrogationResult,
	isPingError bool,
	isCompatFork *bool,
	clientID *string,
) error {
	var peers []*enode.Node
	if result != nil {
		peers = result.Peers
	}

	for _, peer := range peers {
		peerID, err := nodeID(peer)
		if err != nil {
			return fmt.Errorf("failed to get peer node ID: %w", err)
		}

		dbErr := crawler.db.UpsertNodeAddr(ctx, peerID, makeNodeAddr(peer))
		if dbErr != nil {
			return dbErr
		}
	}

	if (result != nil) && (len(result.KeygenKeys) >= 15) {
		keygenHexKeys := hexEncodePublicKeys(result.KeygenKeys)
		dbErr := crawler.db.UpdateNeighborBucketKeys(ctx, id, keygenHexKeys)
		if dbErr != nil {
			return dbErr
		}
	}

	if (result != nil) && (result.HandshakeErr != nil) {
		dbErr := crawler.db.UpdateHandshakeError(ctx, id, result.HandshakeErr.StringCode())
		if dbErr != nil {
			return dbErr
		}
	}

	if isPingError {
		dbErr := crawler.db.UpdatePingError(ctx, id)
		if dbErr != nil {
			return dbErr
		}
	} else {
		dbErr := crawler.db.ResetPingError(ctx, id)
		if dbErr != nil {
			return dbErr
		}
	}

	if isCompatFork != nil {
		dbErr := crawler.db.UpdateForkCompatibility(ctx, id, *isCompatFork)
		if dbErr != nil {
			return dbErr
		}
	}

	if clientID != nil {
		dbErr := crawler.db.UpdateClientID(ctx, id, *clientID)
		if dbErr != nil {
			return dbErr
		}
	}

	return nil
}

package observer

import (
	"context"
	"errors"
	"fmt"
	"github.com/ledgerwatch/erigon/core/forkid"
	"github.com/ledgerwatch/erigon/p2p/enode"
	"github.com/ledgerwatch/erigon/params"
	"github.com/ledgerwatch/log/v3"
	"golang.org/x/sync/semaphore"
	"time"
)

type Crawler struct {
	transport        DiscV4Transport
	db               DBRetrier
	bootnodes        []*enode.Node
	forkFilter       forkid.Filter
	concurrencyLimit uint
	log              log.Logger
}

func NewCrawler(
	transport DiscV4Transport,
	db DB,
	bootnodes []*enode.Node,
	chain string,
	concurrencyLimit uint,
	logger log.Logger,
) (*Crawler, error) {
	chainConfig := params.ChainConfigByChainName(chain)
	genesisHash := params.GenesisHashByChainName(chain)
	if (chainConfig == nil) || (genesisHash == nil) {
		return nil, fmt.Errorf("unknown chain %s", chain)
	}

	forkFilter := forkid.NewStaticFilter(chainConfig, *genesisHash)

	instance := Crawler{
		transport,
		NewDBRetrier(db, logger),
		bootnodes,
		forkFilter,
		concurrencyLimit,
		logger,
	}
	return &instance, nil
}

func (crawler *Crawler) startSelectCandidates(ctx context.Context) <-chan *enode.Node {
	nodes := make(chan *enode.Node)
	go func() {
		err := crawler.selectCandidates(ctx, nodes)
		if (err != nil) && !errors.Is(err, context.Canceled) {
			crawler.log.Error("Failed to select candidates", "err", err)
		}
		close(nodes)
	}()
	return nodes
}

func (crawler *Crawler) selectCandidates(ctx context.Context, nodes chan<- *enode.Node) error {
	for _, node := range crawler.bootnodes {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case nodes <- node:
		}
	}

	for ctx.Err() == nil {
		reselectPeriod := 10*time.Minute
		limit := crawler.concurrencyLimit
		candidates, err := crawler.db.TakeCandidates(ctx, reselectPeriod, limit)
		if err != nil {
			return err
		}

		if len(candidates) == 0 {
			sleep(ctx, 1*time.Second)
		}

		for _, node := range candidates {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case nodes <- node:
			}
		}
	}

	return ctx.Err()
}

func (crawler *Crawler) Run(ctx context.Context) error {
	nodes := crawler.startSelectCandidates(ctx)
	sem := semaphore.NewWeighted(int64(crawler.concurrencyLimit))

	for node := range nodes {
		if err := sem.Acquire(ctx, 1); err != nil {
			if !errors.Is(err, context.Canceled) {
				return fmt.Errorf("failed to acquire semaphore: %w", err)
			} else {
				break
			}
		}

		nodeDesc := node.URLv4()
		logger := crawler.log.New("node", nodeDesc)

		interrogator, err := NewInterrogator(node, crawler.transport, crawler.forkFilter, logger)
		if err != nil {
			return fmt.Errorf("failed to create Interrogator for node %s: %w", nodeDesc, err)
		}

		go func() {
			defer sem.Release(1)

			peers, err := interrogator.Run(ctx)
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					logger.Warn("Failed to interrogate node", "err", err)
				}
				return
			}

			logger.Info(fmt.Sprintf("Got %d peers", len(peers)))
			for _, peer := range peers {
				err = crawler.db.UpsertNode(ctx, peer)
				if err != nil {
					if !errors.Is(err, context.Canceled) {
						logger.Error("Failed to save node", "err", err)
					}
					break
				}
			}
		}()
	}
	return nil
}
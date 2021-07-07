package main

import (
	"context"
	"fmt"

	"github.com/ipfs/go-ipfs/core"
	"github.com/ipfs/go-ipfs/core/coreapi"
	cfg "github.com/lazyledger/lazyledger-core/config"
	"github.com/lazyledger/lazyledger-core/evidence"
	"github.com/lazyledger/lazyledger-core/ipfs"
	dbm "github.com/lazyledger/lazyledger-core/libs/db"
	"github.com/lazyledger/lazyledger-core/libs/log"
	"github.com/lazyledger/lazyledger-core/light/provider"
	lightclient "github.com/lazyledger/lazyledger-core/light/provider/http"
	mempl "github.com/lazyledger/lazyledger-core/mempool"
	"github.com/lazyledger/lazyledger-core/node"
	"github.com/lazyledger/lazyledger-core/p2p/ipld"
	"github.com/lazyledger/lazyledger-core/proxy"
	"github.com/lazyledger/lazyledger-core/rpc/client/http"
	rpctypes "github.com/lazyledger/lazyledger-core/rpc/core/types"
	sm "github.com/lazyledger/lazyledger-core/state"
	"github.com/lazyledger/lazyledger-core/store"
	"github.com/lazyledger/lazyledger-core/types"
	"github.com/lazyledger/lazyledger-core/types/consts"
	"github.com/tendermint/tendermint/state"

	"github.com/spf13/cobra"
)

const (
	subscriber = "auditor"
	chainID    = "test"
)

func main() {
	rootCmd := cobra.Command{
		Use:     "frankenstein-auditor remote",
		Aliases: []string{"auditor-node"},
		RunE:    start,
		Args:    cobra.ExactArgs(1),
	}

	if err := rootCmd.Execute(); err != nil {
		panic(err)
	}

}

func start(cmd *cobra.Command, args []string) error {
	// create new node
	ipfsConfig := ipfs.DefaultConfig()
	ipfsConfig.Bootstraps = []string{"/ip4/0.0.0.0/tcp/16001/p2p/12D3KooWSSg1YAoX3Pi1s8FtQRjqwwjD3cascfKx4M3EXtXz1PMG"}

	provider := ipfs.Embedded(true, ipfsConfig, log.TestingLogger())
	n, err := NewNode(context.Background(), args[0], provider)
	if err != nil {
		return err
	}

	api, err := coreapi.NewCoreAPI(n.ipfs)

	// subscribe to headers
	headers, err := n.SubscribeHeaders(n.ctx)
	if err != nil {
		return err
	}

	// verify in the same way that the blockchain reactor does
	for event := range headers {
		headerEvent := event.Data.(types.EventDataNewBlockHeader)
		header := headerEvent.Header

		lb, err := n.provider.DASLightBlock(n.ctx, header.Height)
		if err != nil {
			return err
		}

		err = lb.ValidatorSet.VerifyCommitLight(chainID, header.LastBlockID, header.Height, lb.Commit)
		if err != nil {
			return err
		}

		shares, err := ipld.RetrieveShares(n.ctx, consts.TxNamespaceID, lb.DataAvailabilityHeader, api)
		if err != nil {
			return err
		}

		// todo(evan): download and parse the rest of the reserved Txs
		txs, err := types.ParseTxs(shares)
		if err != nil {
			return err
		}

		for _, tx := range txs {

		}

	}
	// request the reserved txs
	// replay each of the
	return nil
}

func DefaultNewNode(config *cfg.Config, ipfs ipfs.NodeProvider, logger log.Logger) (*Node, error) {
	return NewNode(
		context.TODO(),
		config,
		"http://127.0.0.1:25667",
		ipfs,
		proxy.DefaultClientCreator(config.ProxyApp, config.DBDir()),
		node.DefaultGenesisDocProviderFunc(config),
		node.DefaultDBProvider,
		logger,
	)
}

type Node struct {
	ctx      context.Context
	config   *cfg.Config
	client   *http.HTTP
	provider provider.Provider
	ipfs     *core.IpfsNode
	logger   log.Logger
}

func NewNode(
	ctx context.Context,
	config *cfg.Config,
	remote string,
	ipfsProvider ipfs.NodeProvider,
	clientCreator proxy.ClientCreator,
	genesisDocProvider node.GenesisDocProvider,
	dbProvider node.DBProvider,
	logger log.Logger,
) (*Node, error) {

	stateDB, err := dbProvider(&node.DBContext{"state", config})
	if err != nil {
		return nil, err
	}

	stateStore := sm.NewStore(stateDB)

	genState, genDoc, err := node.LoadStateFromDBOrGenesisDocProvider(stateDB, genesisDocProvider)
	if err != nil {
		return nil, err
	}

	// Create the proxyApp and establish connections to the ABCI app (consensus, mempool, query).
	proxyApp := proxy.NewAppConns(clientCreator)
	proxyApp.SetLogger(logger.With("module", "proxy"))
	if err := proxyApp.Start(); err != nil {
		return nil, fmt.Errorf("error starting proxy app connections: %v", err)
	}

	blockStoreDB, err := dbProvider(&node.DBContext{"blockstore", config})
	if err != nil {
		return nil, err
	}

	ipfsNode, err := ipfsProvider()
	if err != nil {
		return nil, err
	}

	blockStore := store.NewBlockStore(blockStoreDB, ipfsNode.Blockstore, logger)

	evReactor, evidencePool, err := createEvidenceReactor(config, dbProvider, stateDB, blockStore, logger)
	if err != nil {
		return nil, err
	}

	// todo(evan): create a custom mempool that only returns the reserved txs that are included in the block.
	memplReactor, mempool := createMempoolAndMempoolReactor(config, proxyApp, genState, logger)

	// make block executor for consensus and blockchain reactors to execute blocks
	blockExec := sm.NewBlockExecutor(
		stateStore,
		logger.With("module", "state"),
		proxyApp.Consensus(),
		mempool,
		evidencePool,
	)

	client, err := http.New(remote, "/websocket")
	if err != nil {
		return nil, err
	}

	lclient := lightclient.NewWithClient(chainID, client)

	return &Node{
		ctx:      ctx,
		config:   config,
		ipfs:     ipfsNode,
		provider: lclient,
		client:   client,
		logger:   logger,
	}, nil
}

func (n Node) OnStart() error {
	return n.client.Start()
}

func (n Node) OnStop() error {
	err := n.ipfs.Close()
	if err != nil {
		return err
	}
	err = n.client.Stop()
	if err != nil {
		return err
	}
	return nil
}

func (n Node) SubscribeHeaders(ctx context.Context) (<-chan rpctypes.ResultEvent, error) {
	return n.client.Subscribe(ctx, subscriber, types.EventQueryNewBlockHeader.String())
}

func createEvidenceReactor(config *cfg.Config, dbProvider node.DBProvider,
	stateDB dbm.DB, blockStore *store.BlockStore, logger log.Logger) (*evidence.Reactor, *evidence.Pool, error) {

	evidenceDB, err := dbProvider(&node.DBContext{"evidence", config})
	if err != nil {
		return nil, nil, err
	}
	evidenceLogger := logger.With("module", "evidence")
	evidencePool, err := evidence.NewPool(evidenceDB, state.NewStore(stateDB), blockStore)
	if err != nil {
		return nil, nil, err
	}
	evidenceReactor := evidence.NewReactor(evidencePool)
	evidenceReactor.SetLogger(evidenceLogger)
	return evidenceReactor, evidencePool, nil
}

func createMempoolAndMempoolReactor(config *cfg.Config, proxyApp proxy.AppConns,
	state sm.State, logger log.Logger) (*mempl.Reactor, *mempl.CListMempool) {

	mempool := mempl.NewCListMempool(
		config.Mempool,
		proxyApp.Mempool(),
		state.LastBlockHeight,
		mempl.WithPreCheck(sm.TxPreCheck(state)),
		mempl.WithPostCheck(sm.TxPostCheck(state)),
	)
	mempoolLogger := logger.With("module", "mempool")
	mempoolReactor := mempl.NewReactor(config.Mempool, mempool)
	mempoolReactor.SetLogger(mempoolLogger)

	if config.Consensus.WaitForTxs() {
		mempool.EnableTxsAvailable()
	}
	return mempoolReactor, mempool
}

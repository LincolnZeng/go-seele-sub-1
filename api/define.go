package api

import (
	"math/big"

	"github.com/seeleteam/go-seele/common"
	"github.com/seeleteam/go-seele/core/state"
	"github.com/seeleteam/go-seele/core/store"
	"github.com/seeleteam/go-seele/core/types"
	"github.com/seeleteam/go-seele/log"
	"github.com/seeleteam/go-seele/p2p"
	"github.com/seeleteam/go-seele/rpc"
)

// Backend interface provides the common API services (that are provided by
// both full and light clients) with access to necessary functions.
type Backend interface {
	GetP2pServer() *p2p.Server
	GetNetVersion() uint64
	GetMinerCoinbase() common.Address

	TxPoolBackend() Pool
	ChainBackend() Chain
	ProtocolBackend() Protocol
	Log() *log.SeeleLog
}

func GetAPIs(apiBackend Backend) []rpc.API {
	return []rpc.API{
		{
			Namespace: "seele",
			Version:   "1.0",
			Service:   NewPublicSeeleAPI(apiBackend),
			Public:    true,
		},
		{
			Namespace: "txpool",
			Version:   "1.0",
			Service:   NewTransactionPoolAPI(apiBackend),
			Public:    true,
		},
		{
			Namespace: "network",
			Version:   "1.0",
			Service:   NewPrivateNetworkAPI(apiBackend),
			Public:    false,
		},
		{
			Namespace: "debug",
			Version:   "1.0",
			Service:   NewPrivateDebugAPI(apiBackend),
			Public:    false,
		}}
}

// MinerInfo miner simple info
type GetMinerInfo struct {
	Coinbase           common.Address
	CurrentBlockHeight uint64
	HeaderHash         common.Hash
	Shard              uint
	MinerStatus        string
	MinerThread        int
}

// GetBalanceResponse response param for GetBalance api
type GetBalanceResponse struct {
	Account common.Address
	Balance *big.Int
}

// GetLogsResponse response param for GetLogs api
type GetLogsResponse struct {
	Txhash   common.Hash
	LogIndex uint
	Log      *types.Log
}

type PoolCore interface {
	AddTransaction(tx *types.Transaction) error
	GetTransaction(txHash common.Hash) *types.Transaction
}

type Pool interface {
	PoolCore
	GetTransactions(processing, pending bool) []*types.Transaction
	GetPendingTxCount() int
}

type Chain interface {
	CurrentHeader() *types.BlockHeader
	GetCurrentState() (*state.Statedb, error)
	GetStore() store.BlockchainStore
}

type Protocol interface {
	SendDifferentShardTx(tx *types.Transaction, shard uint)
	GetProtocolVersion() (uint, error)
}
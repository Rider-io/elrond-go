package bootstrap

import (
	"github.com/ElrondNetwork/elrond-go/data"
	"github.com/ElrondNetwork/elrond-go/data/block"
	"github.com/ElrondNetwork/elrond-go/process"
	"github.com/ElrondNetwork/elrond-go/sharding"
)

// StartOfEpochNodesConfigHandler defines the methods to process nodesConfig from epoch start metablocks
type StartOfEpochNodesConfigHandler interface {
	NodesConfigFromMetaBlock(currMetaBlock *block.MetaBlock, prevMetaBlock *block.MetaBlock) (*sharding.NodesCoordinatorRegistry, uint32, error)
	IsInterfaceNil() bool
}

// EpochStartInterceptor defines the methods to sync an epoch start metablock
type EpochStartInterceptor interface {
	process.Interceptor
	GetEpochStartMetaBlock(target int, epoch uint32) (*block.MetaBlock, error)
}

// StartInEpochNodesCoordinator defines the methods to process and save nodesCoordinator information to storage
type StartInEpochNodesCoordinator interface {
	EpochStartPrepare(metaHdr data.HeaderHandler, body data.BodyHandler)
	NodesCoordinatorToRegistry() *sharding.NodesCoordinatorRegistry
	ShardIdForEpoch(epoch uint32) (uint32, error)
	IsInterfaceNil() bool
}

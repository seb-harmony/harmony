package node

import (
	"fmt"

	"github.com/harmony-one/harmony/core/types"
	"github.com/harmony-one/harmony/shard"
	"golang.org/x/sync/errgroup"
)

// HandleConsensusBlockProcessing ..
func (node *Node) HandleConsensusBlockProcessing() error {
	var g errgroup.Group

	g.Go(func() error {
		for accepted := range node.Consensus.RoundCompleted.Request {
			// fmt.Println("received block post consensus process", accepted.Blk.String())
			if _, err := node.Blockchain().InsertChain(types.Blocks{accepted.Blk}, true); err != nil {
				accepted.Err <- err
				continue
			}
			if len(accepted.Blk.Header().ShardState()) > 0 {
				fmt.Println("before post consensus on new shard state header")
			}

			accepted.Err <- node.postConsensusProcessing(accepted.Blk)

			if len(accepted.Blk.Header().ShardState()) > 0 {
				fmt.Println("after post consensus on new shard state header")

			}

			// fmt.Println("received block post consensus process-finished", accepted.Blk.String())
		}
		return nil
	})

	g.Go(func() error {
		for verify := range node.Consensus.Verify.Request {
			// fmt.Println("received block verify process", verify.Blk.String())
			verify.Err <- node.verifyBlock(verify.Blk)
			// fmt.Println("received block verify process", verify.Blk.String())
		}
		return nil
	})

	return g.Wait()

}

// HandleIncomingBeaconBlock ..
func (node *Node) HandleIncomingBlock() error {
	var g errgroup.Group
	chans := []chan *types.Block{
		make(chan *types.Block), make(chan *types.Block),
	}

	g.Go(func() error {
		for acceptedBlock := range chans[0] {
			if acceptedBlock != nil {
				if _, err := node.Beaconchain().InsertChain(
					types.Blocks{acceptedBlock}, true,
				); err != nil {
					return err
				}
			}
			fmt.Println("beaconchain chan wrote", node.Consensus.ShardID, acceptedBlock.String())
		}
		return nil
	})

	g.Go(func() error {
		for acceptedBlock := range chans[1] {
			if acceptedBlock != nil && node.Consensus.ShardID != shard.BeaconChainShardID {
				if _, err := node.Blockchain().InsertChain(
					types.Blocks{acceptedBlock}, true,
				); err != nil {
					return err
				}
				fmt.Println("blockchain chan wrote", node.Consensus.ShardID, acceptedBlock.String())
			}
		}
		return nil
	})

	g.Go(func() error {
		for blk := range node.IncomingBlocksClient {
			if b := blk; b != nil {
				if b.ShardID() == shard.BeaconChainShardID {
					chans[0] <- b
				} else {
					chans[1] <- b
				}
			}
		}
		return nil
	})

	return g.Wait()

}

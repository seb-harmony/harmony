package node

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/harmony-one/harmony/consensus"
	"github.com/harmony-one/harmony/core/rawdb"
	"github.com/harmony-one/harmony/core/types"
	"github.com/harmony-one/harmony/internal/utils"
	"github.com/harmony-one/harmony/shard"
	staking "github.com/harmony-one/harmony/staking/types"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
)

// Constants of proposing a new block
const (
	SleepPeriod           = 20 * time.Millisecond
	IncomingReceiptsLimit = 6000 // 2000 * (numShards - 1)
)

// StartLeaderWork listen for the readiness signal
// from consensus and generate new block for consensus.
// only leader will receive the ready signal
// TODO: clean pending transactions for
// validators; or validators not prepare pending transactions
func (node *Node) StartLeaderWork() error {
	var g errgroup.Group

	g.Go(func() error {
		for range node.Consensus.ProposalNewBlock {
			utils.Logger().Debug().
				Uint64("blockNum", node.Blockchain().CurrentBlock().NumberU64()+1).
				Msg("PROPOSING NEW BLOCK ------------------------------------------------")

			newBlock, err := node.proposeNewBlock()

			if err != nil {
				return err
			}
			if !node.Consensus.IsLeader() {
				fmt.Println("this should NOT be happening")
				return errors.New(" I am not leader should not propose")
			}
			utils.Logger().Debug().
				Uint64("blockNum", newBlock.NumberU64()).
				Uint64("epoch", newBlock.Epoch().Uint64()).
				Uint64("viewID", newBlock.Header().ViewID().Uint64()).
				Int("numTxs", newBlock.Transactions().Len()).
				Int("numStakingTxs", newBlock.StakingTransactions().Len()).
				Int("crossShardReceipts", newBlock.IncomingReceipts().Len()).
				Msg("=========Successfully Proposed New Block==========")
			// Send the new block to Consensus so it can be confirmed.
			fmt.Println("now announced", newBlock.Header().String())

			node.Consensus.SetNextBlockDue(time.Now().Add(consensus.BlockTime))
			if err := node.Consensus.Announce(newBlock); err != nil {
				fmt.Println("problem with annunce why")
				return err
			}

			// if err != nil {
			// 	return err
			// }

		}
		return nil
	})

	var roundDone singleflight.Group

	g.Go(func() error {
		for quorumReached := range node.Consensus.CommitFinishChan {
			if node.Consensus.IsLeader() {
				viewID, shardID := quorumReached.ViewID, quorumReached.ShardID
				results, err, evicted := roundDone.Do(
					fmt.Sprintf("%d-%d", viewID, shardID),
					func() (interface{}, error) {

						if bufferTime := time.Until(
							node.Consensus.NextBlockDue(),
						); bufferTime > time.Second*3 {
							fmt.Println(
								"got the block done faster",
								node.Consensus.ShardID,
								bufferTime.Round(time.Second),
							)
							time.Sleep(time.Second)
						}

						fmt.Println("before finalize", node.Consensus.ShardID)

						if err := node.Consensus.FinalizeCommits(); err != nil {
							fmt.Println("why could not finalize?", err.Error())
							return nil, err
						}
						return nil, nil
					},
				)

				fmt.Println("single flight thing", results, err, evicted)

				if err != nil {
					return err
				}

				node.Consensus.ProposalNewBlock <- struct{}{}
				node.Consensus.SetNextBlockDue(time.Now().Add(consensus.BlockTime))
				fmt.Println("after sending Proposal for new block ", node.Consensus.ShardID)
			}
		}

		return nil
	})

	return g.Wait()
}

func (node *Node) proposeNewBlock() (*types.Block, error) {
	currentHeader := node.Blockchain().CurrentHeader()
	nowEpoch, blockNow := currentHeader.Epoch(), currentHeader.Number()
	utils.AnalysisStart("proposeNewBlock", nowEpoch, blockNow)
	defer utils.AnalysisEnd("proposeNewBlock", nowEpoch, blockNow)

	node.Worker.UpdateCurrent()

	header := node.Worker.GetCurrentHeader()
	// Update worker's current header and
	// state data in preparation to propose/process new transactions
	var (
		coinbase    = node.GetAddressForBLSKey(node.Consensus.LeaderPubKey, header.Epoch())
		beneficiary = coinbase
		err         error
	)

	// After staking, all coinbase will be the address of bls pub key
	if node.Blockchain().Config().IsStaking(header.Epoch()) {
		blsPubKeyBytes := node.Consensus.LeaderPubKey.GetAddress()
		coinbase.SetBytes(blsPubKeyBytes[:])
	}

	emptyAddr := common.Address{}
	if coinbase == emptyAddr {
		return nil, errors.New("[proposeNewBlock] Failed setting coinbase")
	}

	// Must set coinbase here because the operations below depend on it
	header.SetCoinbase(coinbase)

	// Get beneficiary based on coinbase
	// Before staking, coinbase itself is the beneficial
	// After staking, beneficial is the corresponding ECDSA address of the bls key
	beneficiary, err = node.Blockchain().GetECDSAFromCoinbase(header)
	if err != nil {
		return nil, err
	}

	// Prepare normal and staking transactions retrieved from transaction pool
	utils.AnalysisStart("proposeNewBlockChooseFromTxnPool")

	pendingPoolTxs, err := node.TxPool.Pending()
	if err != nil {
		utils.Logger().Err(err).Msg("Failed to fetch pending transactions")
		return nil, err
	}
	pendingPlainTxs := map[common.Address]types.Transactions{}
	pendingStakingTxs := staking.StakingTransactions{}
	for addr, poolTxs := range pendingPoolTxs {
		plainTxsPerAcc := types.Transactions{}
		for _, tx := range poolTxs {
			if plainTx, ok := tx.(*types.Transaction); ok {
				plainTxsPerAcc = append(plainTxsPerAcc, plainTx)
			} else if stakingTx, ok := tx.(*staking.StakingTransaction); ok {
				// Only process staking transactions after pre-staking epoch happened.
				if node.Blockchain().Config().IsPreStaking(node.Worker.GetCurrentHeader().Epoch()) {
					pendingStakingTxs = append(pendingStakingTxs, stakingTx)
				}
			} else {
				utils.Logger().Err(types.ErrUnknownPoolTxType).
					Msg("Failed to parse pending transactions")
				return nil, types.ErrUnknownPoolTxType
			}
		}
		if plainTxsPerAcc.Len() > 0 {
			pendingPlainTxs[addr] = plainTxsPerAcc
		}
	}
	utils.AnalysisEnd("proposeNewBlockChooseFromTxnPool")

	// Try commit normal and staking transactions based on the current state
	// The successfully committed transactions will be put in the proposed block
	if err := node.Worker.CommitTransactions(
		pendingPlainTxs, pendingStakingTxs, beneficiary,
	); err != nil {
		utils.Logger().Error().Err(err).Msg("cannot commit transactions")
		return nil, err
	}

	// Prepare cross shard transaction receipts
	receiptsList := node.proposeReceiptsProof()
	if len(receiptsList) != 0 {
		if err := node.Worker.CommitReceipts(receiptsList); err != nil {
			return nil, err
		}
	}

	isBeaconchainInCrossLinkEra := node.NodeConfig.ShardID == shard.BeaconChainShardID &&
		node.Blockchain().Config().IsCrossLink(node.Worker.GetCurrentHeader().Epoch())

	isBeaconchainInStakingEra := node.NodeConfig.ShardID == shard.BeaconChainShardID &&
		node.Blockchain().Config().IsStaking(node.Worker.GetCurrentHeader().Epoch())

	utils.AnalysisStart("proposeNewBlockVerifyCrossLinks")
	// Prepare cross links and slashing messages
	var crossLinksToPropose types.CrossLinks
	if isBeaconchainInCrossLinkEra {
		allPending, err := node.Blockchain().ReadPendingCrossLinks()
		invalidToDelete := []types.CrossLink{}
		if err == nil {
			for _, pending := range allPending {
				exist, err := node.Blockchain().ReadCrossLink(pending.ShardID(), pending.BlockNum())
				if err == nil || exist != nil {
					invalidToDelete = append(invalidToDelete, pending)
					utils.Logger().Debug().
						AnErr("[proposeNewBlock] pending crosslink is already committed onchain", err)
					continue
				}
				if err := node.VerifyCrossLink(pending); err != nil {
					invalidToDelete = append(invalidToDelete, pending)
					utils.Logger().Debug().
						AnErr("[proposeNewBlock] pending crosslink verification failed", err)
					continue
				}
				crossLinksToPropose = append(crossLinksToPropose, pending)
			}
			utils.Logger().Debug().
				Msgf("[proposeNewBlock] Proposed %d crosslinks from %d pending crosslinks",
					len(crossLinksToPropose), len(allPending),
				)
		} else {
			utils.Logger().Error().Err(err).Msgf(
				"[proposeNewBlock] Unable to Read PendingCrossLinks, number of crosslinks: %d",
				len(allPending),
			)
		}
		node.Blockchain().DeleteFromPendingCrossLinks(invalidToDelete)
	}
	utils.AnalysisEnd("proposeNewBlockVerifyCrossLinks")

	if isBeaconchainInStakingEra {
		// this will set a meaningful w.current.slashes
		if err := node.Worker.CollectVerifiedSlashes(); err != nil {
			return nil, err
		}
	}

	// Prepare shard state
	var shardState *shard.State
	if shardState, err = node.Blockchain().SuperCommitteeForNextEpoch(
		node.Beaconchain(), node.Worker.GetCurrentHeader(), false,
	); err != nil {
		return nil, err
	}

	// Prepare last commit signatures
	sig, mask, err := node.Consensus.BlockCommitSig(header.Number().Uint64() - 1)
	if err != nil {
		utils.Logger().Error().Err(err).Msg("[proposeNewBlock] Cannot get commit signatures from last block")
		return nil, err
	}

	return node.Worker.FinalizeNewBlock(
		sig, mask, node.Consensus.ViewID(),
		coinbase, crossLinksToPropose, shardState,
	)
}

func (node *Node) proposeReceiptsProof() []*types.CXReceiptsProof {
	if !node.Blockchain().Config().HasCrossTxFields(node.Worker.GetCurrentHeader().Epoch()) {
		return []*types.CXReceiptsProof{}
	}

	numProposed := 0
	validReceiptsList := []*types.CXReceiptsProof{}
	pendingReceiptsList := []*types.CXReceiptsProof{}

	node.pendingCXMutex.Lock()
	defer node.pendingCXMutex.Unlock()

	// not necessary to sort the list, but we just prefer to process the list ordered by shard and blocknum
	pendingCXReceipts := []*types.CXReceiptsProof{}
	for _, v := range node.pendingCXReceipts {
		pendingCXReceipts = append(pendingCXReceipts, v)
	}

	sort.SliceStable(pendingCXReceipts, func(i, j int) bool {
		shardCMP := pendingCXReceipts[i].MerkleProof.ShardID < pendingCXReceipts[j].MerkleProof.ShardID
		shardEQ := pendingCXReceipts[i].MerkleProof.ShardID == pendingCXReceipts[j].MerkleProof.ShardID
		blockCMP := pendingCXReceipts[i].MerkleProof.BlockNum.Cmp(
			pendingCXReceipts[j].MerkleProof.BlockNum,
		) == -1
		return shardCMP || (shardEQ && blockCMP)
	})

	m := map[common.Hash]struct{}{}

Loop:
	for _, cxp := range node.pendingCXReceipts {
		if numProposed > IncomingReceiptsLimit {
			pendingReceiptsList = append(pendingReceiptsList, cxp)
			continue
		}
		// check double spent
		if node.Blockchain().IsSpent(cxp) {
			utils.Logger().Debug().Interface("cxp", cxp).Msg("[proposeReceiptsProof] CXReceipt is spent")
			continue
		}
		hash := cxp.MerkleProof.BlockHash
		// ignore duplicated receipts
		if _, ok := m[hash]; ok {
			continue
		} else {
			m[hash] = struct{}{}
		}

		for _, item := range cxp.Receipts {
			if item.ToShardID != node.Blockchain().ShardID() {
				continue Loop
			}
		}

		if err := node.Blockchain().Validator().ValidateCXReceiptsProof(cxp); err != nil {
			if strings.Contains(err.Error(), rawdb.MsgNoShardStateFromDB) {
				pendingReceiptsList = append(pendingReceiptsList, cxp)
			} else {
				utils.Logger().Error().Err(err).Msg("[proposeReceiptsProof] Invalid CXReceiptsProof")
			}
			continue
		}

		utils.Logger().Debug().Interface("cxp", cxp).Msg("[proposeReceiptsProof] CXReceipts Added")
		validReceiptsList = append(validReceiptsList, cxp)
		numProposed = numProposed + len(cxp.Receipts)
	}

	node.pendingCXReceipts = make(map[string]*types.CXReceiptsProof)
	for _, v := range pendingReceiptsList {
		blockNum := v.Header.Number().Uint64()
		shardID := v.Header.ShardID()
		key := utils.GetPendingCXKey(shardID, blockNum)
		node.pendingCXReceipts[key] = v
	}

	utils.Logger().Debug().Msgf("[proposeReceiptsProof] number of validReceipts %d", len(validReceiptsList))
	return validReceiptsList
}

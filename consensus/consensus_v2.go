package consensus

import (
	"bytes"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	msg_pb "github.com/harmony-one/harmony/api/proto/message"
	"github.com/harmony-one/harmony/block"
	"github.com/harmony-one/harmony/consensus/quorum"
	"github.com/harmony-one/harmony/core/types"
	vrf_bls "github.com/harmony-one/harmony/crypto/vrf/bls"
	nodeconfig "github.com/harmony-one/harmony/internal/configs/node"
	"github.com/harmony-one/harmony/internal/utils"
	"github.com/harmony-one/harmony/p2p"
	"github.com/harmony-one/harmony/shard"
	"github.com/harmony-one/vdf/src/vdf_go"
	"github.com/pkg/errors"
)

const (
	// BlockTime ..
	BlockTime time.Duration = 8 * time.Second
)

var (
	// ErrEmptyMessage ..
	ErrEmptyMessage = errors.New("empty consensus message")
)

// HandleMessageUpdate will update the consensus state according to received message
func (consensus *Consensus) HandleMessageUpdate(msg *msg_pb.Message) error {

	if b := msg.GetNewBlock(); b != nil {
		fmt.Println("got my new block from leader")
		panic("did receive")
	}

	// when node is in ViewChanging mode, it still accepts normal messages into FBFTLog
	// in order to avoid possible trap forever but drop PREPARE and COMMIT
	// which are message types specifically for a node acting as leader
	if (consensus.Current.Mode() == ViewChanging) &&
		(msg.Type == msg_pb.MessageType_PREPARE ||
			msg.Type == msg_pb.MessageType_COMMIT) {
		return errors.New("omething about this")
	}

	if msg.Type == msg_pb.MessageType_VIEWCHANGE ||
		msg.Type == msg_pb.MessageType_NEWVIEW {
		if vc := msg.GetViewchange(); vc != nil &&
			vc.ShardId != consensus.ShardID {
			return errors.New("something else about here")
		}
	} else {
		if con := msg.GetConsensus(); con != nil &&
			con.ShardId != consensus.ShardID {
			return errors.New("dont know")
		}
	}

	switch t := msg.Type; true {
	// Handle validator intended messages first
	case t == msg_pb.MessageType_ANNOUNCE && consensus.validatorSanityChecks(msg):
		return consensus.onAnnounce(msg)
	case t == msg_pb.MessageType_PREPARED && consensus.validatorSanityChecks(msg):
		return consensus.onPrepared(msg)
	case t == msg_pb.MessageType_COMMITTED && consensus.validatorSanityChecks(msg):
		return consensus.onCommitted(msg)
	// Handle leader intended messages now
	case t == msg_pb.MessageType_PREPARE && consensus.leaderSanityChecks(msg):
		return consensus.onPrepare(msg)
	case t == msg_pb.MessageType_COMMIT && consensus.leaderSanityChecks(msg):
		return consensus.onCommit(msg)
	case t == msg_pb.MessageType_VIEWCHANGE && consensus.viewChangeSanityCheck(msg):
		return consensus.onViewChange(msg)
	case t == msg_pb.MessageType_NEWVIEW && consensus.viewChangeSanityCheck(msg):
		return consensus.onNewView(msg)

	}

	return nil
}

// FinalizeCommits ..
func (consensus *Consensus) FinalizeCommits() error {
	consensus.Locks.Global.Lock()
	defer consensus.Locks.Global.Unlock()

	utils.Logger().Info().
		Int64("NumCommits", consensus.Decider.SignersCount(quorum.Commit)).
		Msg("[finalizeCommits] Finalizing Block")

	// beforeCatchupNum := consensus.BlockNum()
	leaderPriKey, err := consensus.GetConsensusLeaderPrivateKey()
	if err != nil {
		utils.Logger().Error().Err(err).Msg("[FinalizeCommits] leader not found")
		return err
	}
	// Construct committed message
	network, err := consensus.construct(
		msg_pb.MessageType_COMMITTED, nil, leaderPriKey.GetPublicKey(), leaderPriKey,
	)
	if err != nil {
		utils.Logger().Warn().Err(err).
			Msg("[FinalizeCommits] Unable to construct Committed message")
		return err
	}
	msgToSend, aggSig, FBFTMsg :=
		network.Bytes,
		network.OptionalAggregateSignature,
		network.FBFTMsg
	consensus.aggregatedCommitSig = aggSig // this may not needed
	consensus.FBFTLog.AddMessage(FBFTMsg)
	// find correct block content

	block := consensus.FBFTLog.GetBlockByHash(consensus.BlockHash())
	if block == nil {
		utils.Logger().Warn().Msg("[FinalizeCommits] Cannot find block by hash")
		return errors.New("could not find block by hash")
	}

	if err := consensus.tryCatchup(); err != nil {
		return err
	}

	// if consensus.BlockNum()-beforeCatchupNum != 1 {
	// 	utils.Logger().Warn().
	// 		Uint64("beforeCatchupBlockNum", beforeCatchupNum).
	// 		Msg("[FinalizeCommits] Leader cannot provide the correct block for committed message")
	// 	return errors.New("leader cannot provide the correct block for committed message")
	// }

	// if leader success finalize the block, send committed message to validators
	if err := consensus.host.SendMessageToGroups([]nodeconfig.GroupID{
		nodeconfig.NewGroupIDByShardID(nodeconfig.ShardID(consensus.ShardID)),
	},
		p2p.ConstructMessage(msgToSend)); err != nil {
		utils.Logger().Warn().Err(err).Msg("[finalizeCommits] Cannot send committed message")
		return err
	}
	utils.Logger().Info().
		Uint64("blockNum", consensus.BlockNum()).
		Msg("[finalizeCommits] Sent Committed Message")

	utils.Logger().Info().
		Uint64("blockNum", block.NumberU64()).
		Uint64("epochNum", block.Epoch().Uint64()).
		Uint64("ViewId", block.Header().ViewID().Uint64()).
		Str("blockHash", block.Hash().String()).
		Int("index", consensus.Decider.IndexOf(consensus.LeaderPubKey())).
		Int("numTxns", len(block.Transactions())).
		Int("numStakingTxns", len(block.StakingTransactions())).
		Msg("HOORAY!!!!!!! CONSENSUS REACHED!!!!!!!")

	// if n := time.Now(); n.Before(consensus.NextBlockDue()) {
	// 	// Sleep to wait for the full block time
	// 	utils.Logger().Debug().Msg("[finalizeCommits] Waiting for Block Time")
	// 	time.Sleep(consensus.NextBlockDue().Sub(n))
	// }
	return nil
}

// NextBlockDue ..
func (consensus *Consensus) NextBlockDue() time.Time {
	return consensus.nextBlockDue.Load().(time.Time)
}

// SetNextBlockDue ..
func (consensus *Consensus) SetNextBlockDue(newTime time.Time) {
	consensus.nextBlockDue.Store(newTime)
}

// BlockCommitSig returns the byte array of aggregated
// commit signature and bitmap signed on the block
func (consensus *Consensus) BlockCommitSig(blockNum uint64) ([]byte, []byte, error) {
	num := consensus.BlockNum()
	if num <= 1 {
		return nil, nil, nil
	}
	lastCommits, err := consensus.ChainReader.ReadCommitSig(blockNum)
	if err != nil ||
		len(lastCommits) < shard.BLSSignatureSizeInBytes {
		msgs := consensus.FBFTLog.GetMessagesByTypeSeq(
			msg_pb.MessageType_COMMITTED, num-1,
		)
		if len(msgs) != 1 {
			utils.Logger().Error().
				Int("numCommittedMsg", len(msgs)).
				Msg("GetLastCommitSig failed with wrong number of committed message")
			return nil, nil, errors.Errorf(
				"GetLastCommitSig failed with wrong number of committed message %d", len(msgs),
			)
		}
		lastCommits = msgs[0].Payload
	}
	//#### Read payload data from committed msg
	aggSig := make([]byte, shard.BLSSignatureSizeInBytes)
	bitmap := make([]byte, len(lastCommits)-shard.BLSSignatureSizeInBytes)
	offset := 0
	copy(aggSig[:], lastCommits[offset:offset+shard.BLSSignatureSizeInBytes])
	offset += shard.BLSSignatureSizeInBytes
	copy(bitmap[:], lastCommits[offset:])
	//#### END Read payload data from committed msg
	return aggSig, bitmap, nil
}

// try to catch up if fall behind
func (consensus *Consensus) tryCatchup() error {
	utils.Logger().Info().Msg("[TryCatchup] commit new blocks")
	then := consensus.BlockNum()

	for {
		msgs := consensus.FBFTLog.GetMessagesByTypeSeq(
			msg_pb.MessageType_COMMITTED, consensus.BlockNum(),
		)
		if len(msgs) == 0 {
			break
		}
		if len(msgs) > 1 {
			utils.Logger().Error().
				Int("numMsgs", len(msgs)).
				Msg("DANGER!!! we should only get one committed message for a given blockNum")
			return errors.New("we should only get one committed message for a given blockNum")
		}

		var committedMsg *FBFTMessage
		var block *types.Block
		for i := range msgs {
			tmpBlock := consensus.FBFTLog.GetBlockByHash(msgs[i].BlockHash)
			if tmpBlock == nil {
				blksRepr, msgsRepr, incomingMsg :=
					consensus.FBFTLog.Blocks().String(),
					consensus.FBFTLog.Messages().String(),
					msgs[i].String()
				utils.Logger().Debug().
					Str("FBFT-log-blocks", blksRepr).
					Str("FBFT-log-messages", msgsRepr).
					Str("incoming-message", incomingMsg).
					Uint64("blockNum", msgs[i].BlockNum).
					Uint64("viewID", msgs[i].ViewID).
					Str("blockHash", msgs[i].BlockHash.Hex()).
					Msg("[TryCatchup] Failed finding a matching block for committed message")
				continue
			}

			if err := consensus.ChainVerifier.ValidateBody(tmpBlock); err != nil {
				return errors.New("why could not validate body?")
			}

			committedMsg = msgs[i]
			block = tmpBlock
			break
		}
		if block == nil || committedMsg == nil {
			utils.Logger().Error().Msg("[TryCatchup] Failed finding a valid committed message.")
			return errors.New("[TryCatchup] Failed finding a valid committed message")
		}

		if block.ParentHash() != consensus.ChainReader.CurrentHeader().Hash() {
			utils.Logger().Debug().Msg("[TryCatchup] parent block hash not match")
			return errors.New("parent block hash not match")
		}
		utils.Logger().Info().Msg("[TryCatchup] block found to commit")

		preparedMsgs := consensus.FBFTLog.GetMessagesByTypeSeqHash(
			msg_pb.MessageType_PREPARED, committedMsg.BlockNum, committedMsg.BlockHash,
		)
		msg := consensus.FBFTLog.FindMessageByMaxViewID(preparedMsgs)
		if msg == nil {
			// return errors.New("could not find message by max viewid in fbftlog")
			break
		}
		utils.Logger().Info().Msg("[TryCatchup] prepared message found to commit")

		// TODO(Chao): Explain the reasoning for these code
		consensus.SetBlockHash(common.Hash{})
		consensus.SetBlockNum(consensus.BlockNum() + 1)
		consensus.SetViewID(committedMsg.ViewID + 1)
		// TODO Need to make this one atomic as well , the publock is bad, blocks updateconsensus
		consensus.SetLeaderPubKey(committedMsg.SenderPubkey)

		utils.Logger().Info().Msg("[TryCatchup] Adding block to chain")

		// Fill in the commit signatures
		block.SetCurrentCommitSig(committedMsg.Payload)

		if err := consensus.ChainVerifier.ValidateBody(block); err != nil {
			utils.Logger().Error().Err(err).
				Msg("block processing after finishing consensus failed")
			return err
		}

		if err := consensus.PostConsensus.Process(block); err != nil {
			return err
		}

		consensus.ResetState()
		// TODO need to let state sync know that i caught up somehow
		break
	}

	now := consensus.BlockNum()
	if then < now {
		utils.Logger().Info().
			Uint64("From", then).
			Uint64("To", now).
			Msg("[TryCatchup] Caught up!")
		consensus.switchPhase(FBFTAnnounce)
	}

	// catup up and skip from view change trap
	if then < now && consensus.Current.Mode() == ViewChanging {
		consensus.Current.SetMode(Normal)
	}

	// clean up old log
	consensus.FBFTLog.DeleteBlocksLessThan(now - 1)
	consensus.FBFTLog.DeleteMessagesLessThan(now - 1)
	return nil
}

// GenerateVrfAndProof generates new VRF/Proof from hash of previous block
func (consensus *Consensus) GenerateVrfAndProof(
	newBlock *types.Block, vrfBlockNumbers []uint64,
) []uint64 {
	key, err := consensus.GetConsensusLeaderPrivateKey()
	if err != nil {
		utils.Logger().Error().
			Err(err).
			Msg("[GenerateVrfAndProof] VRF generation error")
		return vrfBlockNumbers
	}
	sk := vrf_bls.NewVRFSigner(key)
	blockHash := [32]byte{}
	previousHeader := consensus.ChainReader.GetHeaderByNumber(
		newBlock.NumberU64() - 1,
	)
	previousHash := previousHeader.Hash()
	copy(blockHash[:], previousHash[:])

	vrf, proof := sk.Evaluate(blockHash[:])
	newBlock.AddVrf(append(vrf[:], proof...))

	utils.Logger().Info().
		Uint64("MsgBlockNum", newBlock.NumberU64()).
		Uint64("Epoch", newBlock.Header().Epoch().Uint64()).
		Int("Num of VRF", len(vrfBlockNumbers)).
		Msg("[ConsensusMainLoop] Leader generated a VRF")

	return vrfBlockNumbers
}

// ValidateVrfAndProof validates a VRF/Proof from hash of previous block
func (consensus *Consensus) ValidateVrfAndProof(headerObj *block.Header) bool {
	vrfPk := vrf_bls.NewVRFVerifier(consensus.LeaderPubKey())
	var blockHash [32]byte
	previousHeader := consensus.ChainReader.GetHeaderByNumber(
		headerObj.Number().Uint64() - 1,
	)
	previousHash := previousHeader.Hash()
	copy(blockHash[:], previousHash[:])
	vrfProof := [96]byte{}
	copy(vrfProof[:], headerObj.Vrf()[32:])
	hash, err := vrfPk.ProofToHash(blockHash[:], vrfProof[:])

	if err != nil {
		utils.Logger().Warn().
			Err(err).
			Str("MsgBlockNum", headerObj.Number().String()).
			Msg("[OnAnnounce] VRF verification error")
		return false
	}

	if !bytes.Equal(hash[:], headerObj.Vrf()[:32]) {
		utils.Logger().Warn().
			Str("MsgBlockNum", headerObj.Number().String()).
			Msg("[OnAnnounce] VRF proof is not valid")
		return false
	}

	vrfBlockNumbers, _ := consensus.ChainReader.ReadEpochVrfBlockNums(
		headerObj.Epoch(),
	)
	utils.Logger().Info().
		Str("MsgBlockNum", headerObj.Number().String()).
		Int("Number of VRF", len(vrfBlockNumbers)).
		Msg("[OnAnnounce] validated a new VRF")

	return true
}

// GenerateVdfAndProof generates new VDF/Proof from VRFs in the current epoch
func (consensus *Consensus) GenerateVdfAndProof(
	newBlock *types.Block, vrfBlockNumbers []uint64,
) {
	//derive VDF seed from VRFs generated in the current epoch
	seed := [32]byte{}
	for i := 0; i < consensus.VdfSeedSize(); i++ {
		previousVrf := consensus.ChainReader.GetVrfByNumber(vrfBlockNumbers[i])
		for j := 0; j < len(seed); j++ {
			seed[j] = seed[j] ^ previousVrf[j]
		}
	}

	utils.Logger().Info().
		Uint64("MsgBlockNum", newBlock.NumberU64()).
		Uint64("Epoch", newBlock.Header().Epoch().Uint64()).
		Int("Num of VRF", len(vrfBlockNumbers)).
		Msg("[ConsensusMainLoop] VDF computation started")

	// TODO ek – limit concurrency
	go func() {
		vdf := vdf_go.New(shard.Schedule.VdfDifficulty(), seed)
		outputChannel := vdf.GetOutputChannel()
		start := time.Now()
		vdf.Execute()
		duration := time.Since(start)
		utils.Logger().Info().
			Dur("duration", duration).
			Msg("[ConsensusMainLoop] VDF computation finished")
		output := <-outputChannel

		// The first 516 bytes are the VDF+proof and the last 32 bytes are XORed VRF as seed
		rndBytes := [548]byte{}
		copy(rndBytes[:516], output[:])
		copy(rndBytes[516:], seed[:])
		consensus.RndChannel <- rndBytes
	}()
}

// ValidateVdfAndProof validates the VDF/proof in the current epoch
func (consensus *Consensus) ValidateVdfAndProof(headerObj *block.Header) bool {
	vrfBlockNumbers, err := consensus.ChainReader.ReadEpochVrfBlockNums(headerObj.Epoch())
	if err != nil {
		utils.Logger().Error().Err(err).
			Str("MsgBlockNum", headerObj.Number().String()).
			Msg("[OnAnnounce] failed to read VRF block numbers for VDF computation")
	}

	//extra check to make sure there's no index out of range error
	//it can happen if epoch is messed up, i.e. VDF ouput is generated in the next epoch
	if consensus.VdfSeedSize() > len(vrfBlockNumbers) {
		return false
	}

	seed := [32]byte{}
	for i := 0; i < consensus.VdfSeedSize(); i++ {
		previousVrf := consensus.ChainReader.GetVrfByNumber(vrfBlockNumbers[i])
		for j := 0; j < len(seed); j++ {
			seed[j] = seed[j] ^ previousVrf[j]
		}
	}

	vdfObject := vdf_go.New(shard.Schedule.VdfDifficulty(), seed)
	vdfOutput := [516]byte{}
	copy(vdfOutput[:], headerObj.Vdf())
	if vdfObject.Verify(vdfOutput) {
		utils.Logger().Info().
			Str("MsgBlockNum", headerObj.Number().String()).
			Int("Num of VRF", consensus.VdfSeedSize()).
			Msg("[OnAnnounce] validated a new VDF")

	} else {
		utils.Logger().Warn().
			Str("MsgBlockNum", headerObj.Number().String()).
			Uint64("Epoch", headerObj.Epoch().Uint64()).
			Int("Num of VRF", consensus.VdfSeedSize()).
			Msg("[OnAnnounce] VDF proof is not valid")
		return false
	}

	return true
}

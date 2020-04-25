package consensus

import (
	"bytes"
	"testing"

	msg_pb "github.com/harmony-one/harmony/api/proto/message"
	"github.com/harmony-one/harmony/consensus/quorum"
	"github.com/harmony-one/harmony/crypto/bls"
	"github.com/harmony-one/harmony/multibls"
	"github.com/harmony-one/harmony/shard"
)

func TestPopulateMessageFields(t *testing.T) {
	blsPriKey := bls.RandPrivateKey()
	decider := quorum.NewDecider(
		quorum.SuperMajorityVote, shard.BeaconChainShardID,
	)
	consensus, err := New(
		nil, shard.BeaconChainShardID, multibls.GetPrivateKey(blsPriKey), decider,
	)
	if err != nil {
		t.Fatalf("Cannot craeate consensus: %v", err)
	}
	consensus.viewID = 2
	blockHash := [32]byte{}
	consensus.blockHash = blockHash

	msg := &msg_pb.Message{
		Request: &msg_pb.Message_Consensus{
			Consensus: &msg_pb.ConsensusRequest{},
		},
	}

	consensusMsg := consensus.populateMessageFields(msg.GetConsensus(), consensus.blockHash[:],
		blsPriKey.GetPublicKey())

	if consensusMsg.ViewId != 2 {
		t.Errorf("Consensus ID is not populated correctly")
	}
	if !bytes.Equal(consensusMsg.BlockHash[:], blockHash[:]) {
		t.Errorf("Block hash is not populated correctly")
	}
	if !bytes.Equal(consensusMsg.SenderPubkey, blsPriKey.GetPublicKey().Serialize()) {
		t.Errorf("Sender ID is not populated correctly")
	}
}

func TestSignAndMarshalConsensusMessage(t *testing.T) {
	decider := quorum.NewDecider(quorum.SuperMajorityVote, shard.BeaconChainShardID)
	blsPriKey := bls.RandPrivateKey()
	consensus, err := New(
		nil, shard.BeaconChainShardID, multibls.GetPrivateKey(blsPriKey), decider,
	)
	if err != nil {
		t.Fatalf("Cannot craeate consensus: %v", err)
	}
	consensus.viewID = 2
	consensus.blockHash = [32]byte{}

	msg := &msg_pb.Message{}
	marshaledMessage, err := consensus.signAndMarshalConsensusMessage(msg, blsPriKey)

	if err != nil || len(marshaledMessage) == 0 {
		t.Errorf("Failed to sign and marshal the message: %s", err)
	}
	if len(msg.Signature) == 0 {
		t.Error("No signature is signed on the consensus message.")
	}
}

func TestSetViewID(t *testing.T) {
	decider := quorum.NewDecider(
		quorum.SuperMajorityVote, shard.BeaconChainShardID,
	)
	blsPriKey := bls.RandPrivateKey()
	consensus, err := New(
		nil, shard.BeaconChainShardID, multibls.GetPrivateKey(blsPriKey), decider,
	)
	if err != nil {
		t.Fatalf("Cannot craeate consensus: %v", err)
	}

	height := uint64(1000)
	consensus.SetViewID(height)
	if consensus.viewID != height {
		t.Errorf("Cannot set consensus ID. Got: %v, Expected: %v", consensus.viewID, height)
	}
}

package drand

import (
	"github.com/harmony-one/harmony/api/proto"
	msg_pb "github.com/harmony-one/harmony/api/proto/message"
	"github.com/harmony-one/harmony/pkg/utils"
)

// Constructs the init message
func (dRand *DRand) constructInitMessage() []byte {
	message := &msg_pb.Message{
		ServiceType: msg_pb.ServiceType_DRAND,
		Type:        msg_pb.MessageType_DRAND_INIT,
		Request: &msg_pb.Message_Drand{
			Drand: &msg_pb.DrandRequest{},
		},
	}

	drandMsg := message.GetDrand()
	drandMsg.SenderPubkey = dRand.pubKey.Serialize()
	drandMsg.BlockHash = dRand.blockHash[:]
	drandMsg.ShardId = dRand.ShardID
	// Don't need the payload in init message
	marshaledMessage, err := dRand.signAndMarshalDRandMessage(message)
	if err != nil {
		utils.Logger().Error().Err(err).Msg("Failed to sign and marshal the init message")
	}
	return proto.ConstructDRandMessage(marshaledMessage)
}

package slash

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/harmony-one/bls/ffi/go/bls"
	"github.com/harmony-one/harmony/block"
	"github.com/harmony-one/harmony/core/state"
	"github.com/harmony-one/harmony/shard"
)

// Record is an proof of a slashing made by a witness of a double-signing event
type Record struct {
	Offender shard.BlsPublicKey
	Signed   struct {
		Header    *block.Header
		Signature *bls.Sign
	} `json:"signed"`
	DoubleSigned struct {
		Header    *block.Header
		Signature *bls.Sign
	} `json:"double-signed"`
	Beneficiary common.Address // the reporter who will get rewarded
}

// TODO(Edgar) Implement Verify and Apply

// Verify checks that the signature is valid
func Verify(candidate *Record) error {
	return nil
}

// Apply ..
func Apply(state *state.DB, slashes []byte) error {
	return nil
}

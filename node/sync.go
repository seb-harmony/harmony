package node

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"sort"
	"sync/atomic"
	"time"

	"github.com/Workiva/go-datastructures/trie/ctrie"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
	protobuf "github.com/golang/protobuf/proto"
	msg_pb "github.com/harmony-one/harmony/api/proto/message"
	"github.com/harmony-one/harmony/block"
	"github.com/harmony-one/harmony/core/types"
	"github.com/harmony-one/harmony/internal/utils"
	"github.com/harmony-one/harmony/p2p"
	"github.com/harmony-one/harmony/shard"
	ipfs_interface "github.com/ipfs/interface-go-ipfs-core"
	libp2p_network "github.com/libp2p/go-libp2p-core/network"
	libp2p_peer "github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-msgio"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

func harmonyProtocolPeers(
	ctx context.Context,
	conns []ipfs_interface.ConnectionInfo,
	host *p2p.Host,
) ([]ipfs_interface.ConnectionInfo, error) {

	streamHandles, okTrie := ctx.Value(trieCtxKey).(*ctrie.Ctrie)

	if !okTrie {
		return nil, errors.New("could not cast from context value")
	}

	var filtered []ipfs_interface.ConnectionInfo
	for _, neighbor := range conns {
		id := neighbor.ID()
		peerID, err := id.MarshalBinary()

		if err != nil {
			return nil, err
		}

		// only pull up things we don't have handles for yet
		if _, exists := streamHandles.Lookup(peerID); exists {
			continue
		}

		protocols, err := host.IPFSNode.PeerHost.Peerstore().SupportsProtocols(
			id, p2p.Protocol,
		)
		if err != nil {
			return nil, err
		}
		seen := false
		for _, protocol := range protocols {
			if seen = protocol == p2p.Protocol; seen {
				break
			}
		}
		if !seen {
			continue
		}

		filtered = append(filtered, neighbor)
	}

	return filtered, nil
}

func protocolPeerHeights(
	ctx context.Context,
	conns []ipfs_interface.ConnectionInfo,
	host *p2p.Host,
	node *Node,
) (map[libp2p_peer.ID]*msg_pb.Message, error) {
	hmyPeers := make(chan libp2p_peer.ID)
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		defer close(hmyPeers)
		for _, neighbor := range conns {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case hmyPeers <- neighbor.ID():
			}
		}

		return nil
	})

	type peerResp struct {
		id  libp2p_peer.ID
		msg *msg_pb.Message
	}

	collect := make(chan *peerResp)
	const nWorkers = 10
	workers := int32(nWorkers)
	for i := 0; i < nWorkers; i++ {
		g.Go(func() error {
			defer func() {
				// Last one out closes shop
				if atomic.AddInt32(&workers, -1) == 0 {
					close(collect)
				}
			}()

			for id := range hmyPeers {
				msgSender, err := node.messageSenderForPeer(ctx, id)
				if err != nil {
					return err
				}
				if rpmes, err := msgSender.SendRequest(ctx, &msg_pb.Message{
					ServiceType: msg_pb.ServiceType_CLIENT_SUPPORT,
					Type:        msg_pb.MessageType_SYNC_REQUEST_BLOCK_HEIGHT,
				}); err != nil {
					return err
				} else {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case collect <- &peerResp{id, rpmes}:
					}
				}
			}
			return nil
		})
	}

	reduce := map[libp2p_peer.ID]*msg_pb.Message{}
	g.Go(func() error {
		for resp := range collect {
			reduce[resp.id] = resp.msg
		}
		return nil
	})

	return reduce, g.Wait()
}

type hashCount struct {
	hash        common.Hash
	peersWithIt []libp2p_peer.ID
}

type mostCommonHash struct {
	beacon []hashCount
	shard  []hashCount
}

func commonHash(
	collect map[libp2p_peer.ID]*msg_pb.Message,
) *mostCommonHash {

	beaconCounters, shardCounters :=
		map[common.Hash]hashCount{}, map[common.Hash]hashCount{}

	for peerID, c := range collect {
		height := c.GetSyncBlockHeight()
		shardHash := common.BytesToHash(height.GetShardHash())
		beaconHash := common.BytesToHash(height.GetBeaconHash())
		currentS := shardCounters[shardHash]
		currentS.peersWithIt = append(currentS.peersWithIt, peerID)
		shardCounters[shardHash] = currentS
		currentB := beaconCounters[beaconHash]
		currentB.peersWithIt = append(currentB.peersWithIt, peerID)
		beaconCounters[beaconHash] = currentB
	}

	b, s :=
		make([]hashCount, 0, len(beaconCounters)),
		make([]hashCount, 0, len(shardCounters))

	for h, value := range beaconCounters {
		value.hash = h
		b = append(b, value)
	}

	for h, value := range shardCounters {
		value.hash = h
		s = append(s, value)
	}

	sort.SliceStable(b, func(i, j int) bool {
		return len(b[i].peersWithIt) > len(b[j].peersWithIt)
	})

	sort.SliceStable(s, func(i, j int) bool {
		return len(s[i].peersWithIt) > len(s[j].peersWithIt)
	})

	return &mostCommonHash{b, s}
}

func syncFromHMYPeersIfNeeded(
	ctx context.Context, host *p2p.Host, node *Node,
) error {
	conns, err := host.CoreAPI.Swarm().Peers(ctx)
	if err != nil {
		return err
	}

	hmyConns, err := harmonyProtocolPeers(ctx, conns, host)
	if err != nil {
		return err
	}

	// NOTE keeping it below 5 because checking all conns can eat lots of resources
	collect, err := protocolPeerHeights(ctx, hmyConns[:7], host, node)
	if err != nil {
		return err
	}

	if len(collect) == 0 {
		return nil
	}

	// slices given back are already ordered in descending order
	chainCommonHashes := commonHash(collect)
	start := node.Blockchain().CurrentHeader().Number().Uint64()

	for _, i := range chainCommonHashes.shard {
		s := rand.NewSource(time.Now().Unix())
		r := rand.New(s)
		idx := r.Intn(len(i.peersWithIt))
		chosen := i.peersWithIt[idx]
		msgSender, err := node.messageSenderForPeer(ctx, chosen)

		if err != nil {
			return err
		}

		rpmes, err := msgSender.SendRequest(ctx, &msg_pb.Message{
			ServiceType: msg_pb.ServiceType_CLIENT_SUPPORT,
			Type:        msg_pb.MessageType_SYNC_REQUEST_BLOCK,
			Request: &msg_pb.Message_SyncBlock{
				SyncBlock: &msg_pb.SyncBlock{
					ShardId: node.Consensus.ShardID,
					Height:  start + 1,
				},
			},
		})

		if err != nil {
			return err
		}

		data := rpmes.GetSyncBlock().GetBlockRlp()
		var blocks []*types.Block
		if err := rlp.DecodeBytes(data, &blocks); err != nil {
			return err
		}

		if len(blocks) == 0 {
			return nil
		}

		fmt.Println("now want to try to write?")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case node.incomingSyncingBlocks <- blocks[0]:
		}
	}

	return nil
}

const (
	blockSyncInterval = 10 * time.Second
)

// HandleIncomingBlocksBySync ..
func (node *Node) HandleIncomingBlocksBySync() error {
	for blk := range node.incomingSyncingBlocks {
		blks := []*types.Block{blk}
		fmt.Println("will try to insert", node.Consensus.ShardID, blk)
		if node.Consensus.ShardID == shard.BeaconChainShardID {
			if _, err := node.Beaconchain().InsertChain(
				blks, true,
			); err != nil {
				return err
			}

		} else {
			if _, err := node.Blockchain().InsertChain(
				blks, true,
			); err != nil {
				return err
			}
		}

		fmt.Println("inserted just fine->", blk.String(), node.Consensus.ShardID)
	}

	return nil
}

func (node *Node) handleNewMessage(s libp2p_network.Stream) error {
	r := msgio.NewVarintReaderSize(s, libp2p_network.MessageSizeMax)
	mPeer := s.Conn().RemotePeer()
	if err := s.SetDeadline(time.Now().Add(25 * time.Second)); err != nil {
		return err
	}

	for {
		var req msg_pb.Message
		msgbytes, err := r.ReadMsg()

		if err != nil {
			defer r.ReleaseMsg(msgbytes)
			if err == io.EOF {
				return nil
			}
			// This string test is necessary because there isn't a single stream reset error
			// instance	in use.
			if err.Error() != "stream reset" {
				utils.Logger().Info().Err(err).Msgf("error reading message")
			}

			return err
		}
		if err := protobuf.Unmarshal(msgbytes, &req); err != nil {
			return err
		}

		r.ReleaseMsg(msgbytes)
		handler := node.syncHandlerForMsgType(req.GetType())

		if handler == nil {
			utils.Logger().Warn().
				Msgf("can't handle received message", "from", mPeer, "type", req.GetType())
			return errors.New("cant receive this message")
		}

		resp, err := handler(context.Background(), mPeer, &req)
		if err != nil {
			return err
		}
		if resp == nil {
			continue
		}
		if err := writeMsg(s, resp); err != nil {
			return err
		}
	}

	return nil

}

func (node *Node) handleNewStream(s libp2p_network.Stream) {
	defer s.Reset()
	if err := node.handleNewMessage(s); err != nil {
		return
	}
	_ = s.Close()
}

// HandleIncomingHMYProtocolStreams ..
func (node *Node) HandleIncomingHMYProtocolStreams() {
	node.host.IPFSNode.PeerHost.SetStreamHandler(
		p2p.Protocol, node.handleNewStream,
	)
}

type msgCtxKey string

var (
	trieCtxKey = msgCtxKey("msgSendr-ctx-key")
)

func (node *Node) messageSenderForPeer(
	ctx context.Context, p libp2p_peer.ID,
) (*messageSender, error) {

	peerID, err := p.MarshalBinary()
	if err != nil {
		return nil, err
	}

	streamHandles, okTrie := ctx.Value(trieCtxKey).(*ctrie.Ctrie)

	if !okTrie {
		return nil, errors.Errorf(
			"cast for ctrie failed from context for peerID %s",
			p.Pretty(),
		)
	}

	existingMS, ok := streamHandles.Lookup(peerID)

	if ok {
		return existingMS.(*messageSender), nil
	}

	ms := &messageSender{p: p, host: node.host}

	node.streamHandles.Insert(peerID, ms)

	if err := ms.prepOrInvalidate(ctx); err != nil {

		if msCur, ok := streamHandles.Lookup(peerID); ok {
			// Changed. Use the new one, old one is invalid and
			// not in the map so we can just throw it away.
			if ms != msCur {
				return msCur.(*messageSender), nil
			}
			// Not changed, remove the now invalid stream from the
			// map.
			streamHandles.Remove(peerID)
		}
		// Invalid but not in map. Must have been removed by a disconnect.
		return nil, err
	}
	// All ready to go.
	return ms, nil
}

type syncHandler func(
	context.Context, libp2p_peer.ID, *msg_pb.Message,
) (*msg_pb.Message, error)

func (node *Node) syncRespBlockHeightHandler(
	ctx context.Context, peer libp2p_peer.ID, msg *msg_pb.Message,
) (*msg_pb.Message, error) {

	beaconHeader := node.Beaconchain().CurrentHeader()
	shardHeader := node.Blockchain().CurrentHeader()

	return &msg_pb.Message{
		ServiceType: msg_pb.ServiceType_CLIENT_SUPPORT,
		Type:        msg_pb.MessageType_SYNC_RESPONSE_BLOCK_HEIGHT,
		Request: &msg_pb.Message_SyncBlockHeight{
			SyncBlockHeight: &msg_pb.SyncBlockHeight{
				ShardId:      node.Consensus.ShardID,
				BeaconHeight: beaconHeader.Number().Uint64(),
				BeaconHash:   beaconHeader.Hash().Bytes(),
				ShardHeight:  shardHeader.Number().Uint64(),
				ShardHash:    shardHeader.Hash().Bytes(),
			},
		},
	}, nil
}

var (
	errDoNotHaveDesiredBlockNum = errors.Errorf("do not have block num")
)

func (node *Node) syncRespBlockHeaderHandler(
	ctx context.Context, peer libp2p_peer.ID, msg *msg_pb.Message,
) (*msg_pb.Message, error) {

	desiredBlockNum, shardID :=
		msg.GetSyncBlockHeader().GetHeight(),
		msg.GetSyncBlockHeader().GetShardId()

	var header *block.Header

	if shardID == shard.BeaconChainShardID {
		header = node.Beaconchain().CurrentHeader()
	} else {
		header = node.Blockchain().CurrentHeader()
	}

	latest := header.Number().Uint64()

	if desiredBlockNum > latest {
		return nil, errors.Wrapf(
			errDoNotHaveDesiredBlockNum,
			"%d %d", desiredBlockNum, latest,
		)
	}

	if shardID == shard.BeaconChainShardID {
		header = node.Beaconchain().GetHeaderByNumber(desiredBlockNum)
	} else {
		header = node.Blockchain().GetHeaderByNumber(desiredBlockNum)
	}

	headersData, err := rlp.EncodeToBytes([]*block.Header{header})

	if err != nil {
		return nil, err
	}

	return &msg_pb.Message{
		ServiceType: msg_pb.ServiceType_CLIENT_SUPPORT,
		Type:        msg_pb.MessageType_SYNC_RESPONSE_BLOCK_HEADER,
		Request: &msg_pb.Message_SyncBlockHeader{
			SyncBlockHeader: &msg_pb.SyncBlockHeader{
				HeaderRlp: headersData,
			},
		},
	}, nil
}

func (node *Node) syncRespBlockHandler(
	ctx context.Context, peer libp2p_peer.ID, msg *msg_pb.Message,
) (*msg_pb.Message, error) {

	desiredBlockNum, shardID :=
		msg.GetSyncBlock().GetHeight(),
		msg.GetSyncBlock().GetShardId()

	var block *types.Block

	if shardID == shard.BeaconChainShardID {
		block = node.Beaconchain().CurrentBlock()
	} else {
		block = node.Blockchain().CurrentBlock()
	}

	latest := block.Number().Uint64()

	if desiredBlockNum > latest {
		return nil, errors.Wrapf(
			errDoNotHaveDesiredBlockNum,
			"%d %d", desiredBlockNum, latest,
		)
	}

	if shardID == shard.BeaconChainShardID {
		block = node.Beaconchain().GetBlockByNumber(desiredBlockNum)
	} else {
		block = node.Blockchain().GetBlockByNumber(desiredBlockNum)
	}

	blocksData, err := rlp.EncodeToBytes([]*types.Block{block})

	if err != nil {
		return nil, err
	}

	return &msg_pb.Message{
		ServiceType: msg_pb.ServiceType_CLIENT_SUPPORT,
		Type:        msg_pb.MessageType_SYNC_RESPONSE_BLOCK,
		Request: &msg_pb.Message_SyncBlock{
			SyncBlock: &msg_pb.SyncBlock{
				BlockRlp: blocksData,
			},
		},
	}, nil
}

func (node *Node) syncHandlerForMsgType(t msg_pb.MessageType) syncHandler {
	switch t {

	case msg_pb.MessageType_SYNC_REQUEST_BLOCK_HEIGHT:
		return node.syncRespBlockHeightHandler
	case msg_pb.MessageType_SYNC_REQUEST_BLOCK_HEADER:
		return node.syncRespBlockHeaderHandler
	case msg_pb.MessageType_SYNC_REQUEST_BLOCK:
		return node.syncRespBlockHandler
	}

	return nil
}

func (node *Node) downloadBlocksForSync(
	ctx context.Context,
	results chan *msg_pb.Message,
) error {
	conns, err := node.host.CoreAPI.Swarm().Peers(ctx)
	if err != nil {
		return err
	}

	hmyConns, err := harmonyProtocolPeers(ctx, conns, node.host)
	if err != nil {
		return err
	}

	g, ctx := errgroup.WithContext(ctx)
	if err != nil {
		return err
	}

	var height uint64

	if node.Consensus.ShardID == shard.BeaconChainShardID {
		height = node.Beaconchain().CurrentHeader().Number().Uint64()
	} else {
		height = node.Blockchain().CurrentHeader().Number().Uint64()
	}

	for _, peerConn := range hmyConns {
		peer := peerConn.ID()
		g.Go(func() error {

			// fmt.Println("connected to", peer.Pretty(),
			// 	":I am ", node.host.IPFSNode.PeerHost.ID().Pretty(),
			// )

			handle, err := node.messageSenderForPeer(ctx, peer)
			if err != nil {
				return err
			}

			// send over my height
			reply, err := handle.SendRequest(ctx, &msg_pb.Message{
				ServiceType: msg_pb.ServiceType_CLIENT_SUPPORT,
				Type:        msg_pb.MessageType_SYNC_REQUEST_BLOCK,
				Request: &msg_pb.Message_SyncBlock{
					SyncBlock: &msg_pb.SyncBlock{
						ShardId: node.Consensus.ShardID,
						Height:  height,
					},
				},
			})

			if err != nil {
				return err
			}
			results <- reply
			return nil
		})
	}

	return g.Wait()
}

// StartBlockSyncing ..
func (node *Node) StartBlockSyncing() error {
	round := 0

	for {
		replies := make(chan *msg_pb.Message)
		var blocksPulled []*types.Block

		const maxBlocksProcess = 50

		go func() {
			for rpmes := range replies {
				if len(blocksPulled) == maxBlocksProcess {
					blocksPulled = []*types.Block{}
				}

				data := rpmes.GetSyncBlock().GetBlockRlp()
				var blocks []*types.Block
				if err := rlp.DecodeBytes(data, &blocks); err != nil {
					fmt.Println("couldn't decode from this person, why")
					panic("oops->" + err.Error())
					// continue
				}
				blocksPulled = append(blocksPulled, blocks...)
			}
		}()

		ctx, cancel := context.WithTimeout(
			context.WithValue(
				context.Background(), trieCtxKey, node.streamHandles.ReadOnlySnapshot()),
			time.Second*25,
		)

		go node.downloadBlocksForSync(ctx, replies)

		<-ctx.Done()
		cancel()
		replies = nil

		fmt.Println("downloaded->", len(blocksPulled), " blocks")

		for _, blk := range blocksPulled {
			fmt.Println("via syncing - wanted to insert ->", blk.String())
			// if blk.ShardID() == node.Consensus.ShardID {

			// 	if blk.ParentHash() == node.Blockchain().CurrentHeader().Hash() {
			// 		fmt.Println("trying to insert block")
			// 		if _, err := node.Blockchain().InsertChain(
			// 			[]*types.Block{blk}, true,
			// 		); err != nil {
			// 			fmt.Println(
			// 				"couldn't add this block oh well",
			// 				err.Error(),
			// 				blk.String(),
			// 			)
			// 		}
			// 	}

			// }

		}

		// Now safe to drop all the handles

		for iter := range node.streamHandles.Iterator(nil) {
			handle, ok := iter.Value.(*messageSender)
			if !ok {
				return errors.New("can not cast")
			}
			handle.invalidate()
		}

		node.streamHandles.Clear()
		round++
	}

	return nil
}

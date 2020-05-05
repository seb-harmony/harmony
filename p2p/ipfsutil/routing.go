package ipfsutil

import (
	"context"
	"fmt"
	"time"

	"github.com/harmony-one/harmony/p2p/tinder"
	datastore "github.com/ipfs/go-datastore"
	ipfs_p2p "github.com/ipfs/go-ipfs/core/node/libp2p"
	host "github.com/libp2p/go-libp2p-core/host"
	peer "github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/routing"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	record "github.com/libp2p/go-libp2p-record"
	"github.com/rs/zerolog"
)

// RoutingOut ..
type RoutingOut struct {
	*dht.IpfsDHT
	tinder.Service
}

// Protocol ..
const Protocol = "/hmy/0.0.1"

// NewTinderRouting ..
func NewTinderRouting(
	log *zerolog.Logger,
	rdvpeer *peer.AddrInfo,
) (ipfs_p2p.RoutingOption, <-chan *RoutingOut) {
	crout := make(chan *RoutingOut, 1)

	return func(
		ctx context.Context,
		h host.Host,
		dstore datastore.Batching,
		validator record.Validator,
	) (routing.Routing, error) {
		defer close(crout)

		dht, err := dht.New(
			ctx, h,
			dht.Datastore(dstore),
			dht.Validator(validator),
			dht.Mode(dht.ModeAuto),
			// NOTE not sure the practial effect yet
			// dht.ProtocolExtension(Protocol),
			dht.MaxRecordAge(time.Hour*12),
			// NOTE next two are both using default of 3, but
			// doing it explicitly so we know what is configurableo

			// Resiliency configures the number of
			// peers closest to a target that must
			// have responded in order for a given query
			// path to complete.
			dht.Resiliency(3),
			// Concurrency configures the number of concurrent
			// requests (alpha in the Kademlia paper) for a given query path.
			dht.Concurrency(3),
		)

		if err != nil {
			return nil, err
		}

		// drivers := []tinder.Driver{}
		// if rdvpeer != nil {
		// h.Peerstore().AddAddrs(rdvpeer.ID, rdvpeer.Addrs, libp2p_peer.PermanentAddrTTL)
		// // @FIXME(gfanton): use rand as argument
		// rdvClient := tinder.NewRendezvousDiscovery(
		// 	log, h, rdvpeer.ID, rand.New(rand.NewSource(rand.Int63())),
		// )

		// drivers = append(drivers, rdvClient)
		// }

		tinderRouting := tinder.NewRouting(
			log, dht, tinder.NewDHTDriver(dht),
		)

		// serv, err := tinder.NewService(
		// 	log,
		// 	drivers,
		// 	p2p_discovery.NewFixedBackoff(time.Minute),
		// 	p2p_discovery.WithBackoffDiscoveryReturnedChannelSize(24),
		// 	p2p_discovery.WithBackoffDiscoverySimultaneousQueryBufferSize(24),
		// )

		if err != nil {
			fmt.Println("why this busted", err.Error())
		}

		crout <- &RoutingOut{dht, tinderRouting}

		return tinderRouting, nil
	}, crout
}
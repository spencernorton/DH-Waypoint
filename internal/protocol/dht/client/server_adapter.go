package client

import (
	"context"
	"errors"
	"net/netip"
	"slices"

	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/dht"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/dht/server"
)

// maxPerReply caps how many nodes, peers or samples are taken from one reply.
// A single datagram can carry thousands; honest nodes send far fewer.
const maxPerReply = 100

type serverAdapter struct {
	nodeID protocol.ID
	server server.Server
}

func (a serverAdapter) Ping(ctx context.Context, addr netip.AddrPort) (PingResult, error) {
	res, err := a.server.Query(ctx, addr, dht.QPing, dht.MsgArgs{ID: a.nodeID})
	if err != nil {
		return PingResult{}, err
	}

	return PingResult{ID: res.Msg.R.ID, ReadOnly: res.Msg.ReadOnly}, nil
}

func (a serverAdapter) FindNode(
	ctx context.Context,
	addr netip.AddrPort,
	target protocol.ID,
) (FindNodeResult, error) {
	res, err := a.server.Query(ctx, addr, dht.QFindNode, dht.MsgArgs{ID: a.nodeID, Target: target})
	if err != nil {
		return FindNodeResult{}, err
	}

	return FindNodeResult{
		ID:       res.Msg.R.ID,
		Nodes:    extractNodes(res.Msg),
		ReadOnly: res.Msg.ReadOnly,
	}, nil
}

func (a serverAdapter) GetPeers(
	ctx context.Context,
	addr netip.AddrPort,
	infoHash protocol.ID,
) (GetPeersResult, error) {
	res, err := a.server.Query(ctx, addr, dht.QGetPeers, dht.MsgArgs{ID: a.nodeID, InfoHash: infoHash})
	if err != nil {
		return GetPeersResult{}, err
	}

	return GetPeersResult{
		ID:       res.Msg.R.ID,
		Values:   extractValues(res.Msg),
		Nodes:    extractNodes(res.Msg),
		ReadOnly: res.Msg.ReadOnly,
	}, nil
}

func (a serverAdapter) GetPeersScrape(
	ctx context.Context,
	addr netip.AddrPort,
	infoHash protocol.ID,
) (GetPeersScrapeResult, error) {
	res, err := a.server.Query(ctx, addr, dht.QGetPeers, dht.MsgArgs{ID: a.nodeID, InfoHash: infoHash, Scrape: 1})
	if err != nil {
		return GetPeersScrapeResult{}, err
	}

	if res.Msg.R.BFpe == nil || res.Msg.R.BFsd == nil {
		return GetPeersScrapeResult{}, errors.New("missing bloom filter in scrape response")
	}

	return GetPeersScrapeResult{
		ID:        res.Msg.R.ID,
		Values:    extractValues(res.Msg),
		Nodes:     extractNodes(res.Msg),
		BfPeers:   *res.Msg.R.BFpe.ToBloomFilter(),
		BfSeeders: *res.Msg.R.BFsd.ToBloomFilter(),
		ReadOnly:  res.Msg.ReadOnly,
	}, nil
}

func (a serverAdapter) SampleInfoHashes(
	ctx context.Context,
	addr netip.AddrPort,
	target protocol.ID,
) (SampleInfoHashesResult, error) {
	res, err := a.server.Query(ctx, addr, dht.QSampleInfohashes, dht.MsgArgs{ID: a.nodeID, Target: target})
	if err != nil {
		return SampleInfoHashesResult{}, err
	}

	totalNum := 0
	interval := 0

	if res.Msg.R.Num != nil {
		totalNum = int(*res.Msg.R.Num)
	}

	if res.Msg.R.Interval != nil {
		interval = int(*res.Msg.R.Interval)
	}

	return SampleInfoHashesResult{
		ID:       res.Msg.R.ID,
		Samples:  extractSamples(res.Msg),
		Nodes:    extractNodes(res.Msg),
		Num:      totalNum,
		Interval: interval,
		ReadOnly: res.Msg.ReadOnly,
	}, nil
}

func extractSamples(msg dht.Msg) []protocol.ID {
	if msg.R.Samples == nil {
		return nil
	}

	samples := *msg.R.Samples

	return slices.Clone([]protocol.ID(samples[:min(len(samples), maxPerReply)]))
}

func extractNodes(msg dht.Msg) []NodeInfo {
	if len(msg.R.Nodes) == 0 {
		return nil
	}

	nodes := make([]NodeInfo, 0, min(len(msg.R.Nodes), maxPerReply))
	for _, n := range msg.R.Nodes {
		if len(nodes) == maxPerReply {
			break
		}

		// The replier chooses these addresses; only public ones are contacted.
		if addr := n.Addr.ToAddrPort(); protocol.IsPublicAddr(addr.Addr()) {
			nodes = append(nodes, NodeInfo{ID: n.ID, Addr: addr})
		}
	}

	return nodes
}

func extractValues(msg dht.Msg) []netip.AddrPort {
	if len(msg.R.Values) == 0 {
		return nil
	}

	values := make([]netip.AddrPort, 0, min(len(msg.R.Values), maxPerReply))
	for _, v := range msg.R.Values {
		if len(values) == maxPerReply {
			break
		}

		// Unmapped, so one peer has one form whichever way a node encodes it.
		if addr := v.ToAddrPort(); protocol.IsPublicAddr(addr.Addr()) {
			values = append(values, netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port()))
		}
	}

	return values
}

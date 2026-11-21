package responder

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"net/netip"
	"slices"

	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/dht"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/dht/ktable"
)

type Responder interface {
	Respond(context.Context, dht.RecvMsg) (dht.Return, error)
}

type responder struct {
	nodeID                   protocol.ID
	nodeIDv6                 protocol.ID
	kTable                   ktable.Table
	tokenSecret              []byte
	sampleInfoHashesInterval int64
}

// maxPeersPerReply bounds a get_peers reply. UDP does not prove where a query
// came from, so a reply must stay small next to the query that asked for it.
const maxPeersPerReply = 50

var ErrMissingArguments = dht.Error{
	Code: dht.ErrorCodeProtocolError,
	Msg:  "missing arguments",
}

var ErrInvalidToken = dht.Error{
	Code: dht.ErrorCodeProtocolError,
	Msg:  "invalid token",
}

var ErrMethodUnknown = dht.Error{
	Code: dht.ErrorCodeMethodUnknown,
	Msg:  "method Unknown",
}

var ErrTooManyRequests = dht.Error{
	Code: dht.ErrorCodeGenericError,
	Msg:  "too many requests",
}

func (r responder) nodeIDFor(addr netip.Addr) protocol.ID {
	if addr.Unmap().Is6() && r.nodeIDv6 != (protocol.ID{}) {
		return r.nodeIDv6
	}
	return r.nodeID
}

// wantFamilies implements BEP 32 section 3: explicit want ("n4"/"n6") selects
// the requested address families; an absent want list defaults to the family of
// the incoming query source address.
func wantFamilies(want []dht.Want, from netip.Addr) (wantV4, wantV6 bool) {
	if len(want) == 0 {
		if from.Unmap().Is6() {
			return false, true
		}
		return true, false
	}
	return slices.Contains(want, dht.WantNodes), slices.Contains(want, dht.WantNodes6)
}

func (r responder) closestNodes6(id protocol.ID) []ktable.Node {
	if v6, ok := r.kTable.(ktable.IPv6Table); ok {
		return v6.GetClosestNodes6(id)
	}
	return nil
}

func (r responder) sampleNodes6(n int) []ktable.Node {
	if v6, ok := r.kTable.(ktable.IPv6Table); ok {
		return v6.SampleNodes6(n)
	}
	return nil
}

func (r responder) Respond(_ context.Context, msg dht.RecvMsg) (ret dht.Return, err error) {
	args := msg.Msg.A
	if args == nil {
		err = ErrMissingArguments
		return
	}

	ret.ID = r.nodeIDFor(msg.From.Addr())
	wantV4, wantV6 := wantFamilies(args.Want, msg.From.Addr())

	switch msg.Msg.Q {
	case dht.QPing:
	case dht.QFindNode:
		if args.Target == [20]byte{} {
			err = ErrMissingArguments
			return
		}

		if wantV4 {
			ret.Nodes = nodeInfosFromNodesForFamily(false, r.kTable.GetClosestNodes(args.Target)...)
		}
		if wantV6 {
			ret.Nodes6 = nodeInfosFromNodesForFamily(true, r.closestNodes6(args.Target)...)
		}
	case dht.QGetPeers:
		if args.InfoHash == [20]byte{} {
			err = ErrMissingArguments
			return
		}

		result := r.kTable.GetHashOrClosestNodes(args.InfoHash)
		closestNodes := result.ClosestNodes

		if result.Found {
			hashPeers := result.Hash.Peers()
			values := make([]dht.NodeAddr, 0, min(len(hashPeers), maxPeersPerReply))

			for _, p := range hashPeers {
				a := p.Addr.Addr().Unmap()
				if (a.Is4() && !wantV4) || (a.Is6() && !wantV6) {
					continue
				}
				values = append(values, dht.NewNodeAddrFromAddrPort(netip.AddrPortFrom(a, p.Addr.Port())))
				if len(values) >= maxPeersPerReply {
					break
				}
			}

			if len(values) > 0 {
				ret.Values = values
			} else {
				// A hash whose matching-family peers have expired (or only exist in the
				// other family) answers with closest nodes per BEP 5 / BEP 32.
				if wantV4 {
					closestNodes = r.kTable.GetClosestNodes(args.InfoHash)
				}
			}
		}

		if len(ret.Values) == 0 {
			if wantV4 {
				ret.Nodes = nodeInfosFromNodesForFamily(false, closestNodes...)
			}
			if wantV6 {
				ret.Nodes6 = nodeInfosFromNodesForFamily(true, r.closestNodes6(args.InfoHash)...)
			}
		}

		token := r.announceToken(args.InfoHash, args.ID, msg.From.Addr())
		ret.Token = &token
	case dht.QAnnouncePeer:
		if args.InfoHash == [20]byte{} {
			err = ErrMissingArguments
			return
		}

		if args.Token != r.announceToken(args.InfoHash, args.ID, msg.From.Addr()) {
			err = ErrInvalidToken
			return
		}

		r.kTable.BatchCommand(ktable.PutHash{ID: args.InfoHash, Peers: []ktable.HashPeer{{
			Addr: netip.AddrPortFrom(msg.From.Addr().Unmap(), msg.AnnouncePort()),
		}}})
	case dht.QSampleInfohashes:
		result := r.kTable.SampleHashesAndNodes()
		samples := make(dht.CompactInfohashes, 0, len(result.Hashes))

		for _, h := range result.Hashes {
			samples = append(samples, h.ID())
		}

		ret.Samples = &samples
		if wantV4 {
			ret.Nodes = nodeInfosFromNodesForFamily(false, result.Nodes...)
		}
		if wantV6 {
			ret.Nodes6 = nodeInfosFromNodesForFamily(true, r.sampleNodes6(len(result.Nodes))...)
		}
		numInt64 := int64(result.TotalHashes)
		ret.Num = &numInt64
		ret.Interval = &r.sampleInfoHashesInterval
	default:
		err = ErrMethodUnknown
		return
	}

	return
}

func (r responder) announceToken(infoHash protocol.ID, nodeID protocol.ID, nodeAddr netip.Addr) string {
	id := r.nodeIDFor(nodeAddr)
	bytes := append([]byte(nil), r.tokenSecret...)
	bytes = append(bytes, id[:]...)
	bytes = append(bytes, infoHash[:]...)
	bytes = append(bytes, nodeID[:]...)
	bytes = append(bytes, []byte(nodeAddr.Unmap().String())...)
	tokenHash := md5.Sum(bytes)

	return hex.EncodeToString(tokenHash[:])
}

func nodeInfosFromNodesForFamily(ipv6 bool, ns ...ktable.Node) []dht.NodeInfo {
	if len(ns) == 0 {
		return nil
	}

	nodes := make([]dht.NodeInfo, 0, len(ns))
	for _, n := range ns {
		a := n.Addr().Addr().Unmap()
		if ipv6 && !a.Is6() {
			continue
		}
		if !ipv6 && !a.Is4() {
			continue
		}
		nodes = append(nodes, dht.NodeInfo{
			ID:   n.ID(),
			Addr: dht.NewNodeAddrFromAddrPort(netip.AddrPortFrom(a, n.Addr().Port())),
		})
	}
	if len(nodes) == 0 {
		return nil
	}

	return nodes
}

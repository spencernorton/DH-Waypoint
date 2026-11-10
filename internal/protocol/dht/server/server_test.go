package server

import (
	"context"
	"errors"
	"net/netip"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/dht"
	"go.uber.org/zap"
)

type packet struct {
	data []byte
	addr netip.AddrPort
}

type fakeSocket struct {
	in, out chan packet
	closed  chan struct{}
}

func (*fakeSocket) Open(netip.AddrPort) error { return nil }
func (*fakeSocket) Close() error              { return nil }

func (f *fakeSocket) Send(addr netip.AddrPort, data []byte) error {
	f.out <- packet{data: slices.Clone(data), addr: addr}
	return nil
}

func (f *fakeSocket) Receive(buf []byte) (int, netip.AddrPort, error) {
	select {
	case p := <-f.in:
		return copy(buf, p.data), p.addr, nil
	case <-f.closed:
		return 0, netip.AddrPort{}, errors.New("socket closed")
	}
}

type stubResponder struct{ id protocol.ID }

func (r stubResponder) Respond(context.Context, dht.RecvMsg) (dht.Return, error) {
	return dht.Return{ID: r.id}, nil
}

func newTestServer(t *testing.T) (*server, *fakeSocket) {
	t.Helper()

	sock := &fakeSocket{in: make(chan packet, 64), out: make(chan packet, 64), closed: make(chan struct{})}
	s := &server{
		stopped:          make(chan struct{}),
		socket:           sock,
		queries:          make(map[string]pendingQuery),
		queryTimeout:     2 * time.Second,
		responder:        stubResponder{id: protocol.RandomNodeID()},
		responderTimeout: time.Second,
		idIssuer:         &variantIDIssuer{},
		logger:           zap.NewNop().Sugar(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	go s.read(ctx)
	t.Cleanup(func() {
		cancel()
		close(sock.closed)
	})

	return s, sock
}

func marshal(msg dht.Msg) []byte {
	data, err := bencode.Marshal(msg)
	if err != nil {
		panic(err)
	}

	return data
}

// Anyone can send a UDP reply. One that carries the right transaction ID but
// comes from an address that was not asked must not answer the query.
func TestQueryIgnoresReplyFromAnotherAddress(t *testing.T) {
	s, sock := newTestServer(t)
	queried := netip.MustParseAddrPort("203.0.113.10:6881")
	genuineID := protocol.RandomNodeID()

	go func() {
		var query dht.Msg
		if err := bencode.Unmarshal((<-sock.out).data, &query); err != nil {
			panic(err)
		}

		forged := dht.Msg{T: query.T, Y: dht.YResponse, R: &dht.Return{
			ID:     protocol.RandomNodeID(),
			Values: []dht.NodeAddr{{IP: []byte{127, 0, 0, 1}, Port: 5432}},
		}}
		sock.in <- packet{data: marshal(forged), addr: netip.MustParseAddrPort("198.51.100.66:6881")}
		sock.in <- packet{data: marshal(forged), addr: netip.AddrPortFrom(queried.Addr(), 1)}

		time.Sleep(50 * time.Millisecond) // let the forged replies be handled first

		genuine := dht.Msg{T: query.T, Y: dht.YResponse, R: &dht.Return{ID: genuineID}}
		sock.in <- packet{data: marshal(genuine), addr: queried}
	}()

	res, err := s.Query(context.Background(), queried, dht.QGetPeers, dht.MsgArgs{
		ID:       protocol.RandomNodeID(),
		InfoHash: protocol.RandomNodeID(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if res.From != queried || res.Msg.R.ID != genuineID || len(res.Msg.R.Values) != 0 {
		t.Fatalf("query to %s was answered by %s with values %v", queried, res.From, res.Msg.R.Values)
	}
}

// A bootstrap entry written as an IP literal reaches Query in the IPv6-mapped
// form, while the socket reports the plain IPv4 sender.
func TestQueryAcceptsReplyToMappedAddress(t *testing.T) {
	s, sock := newTestServer(t)
	plain := netip.MustParseAddrPort("203.0.113.10:6881")
	mapped := netip.AddrPortFrom(netip.AddrFrom16(plain.Addr().As16()), plain.Port())

	go func() {
		var query dht.Msg
		if err := bencode.Unmarshal((<-sock.out).data, &query); err != nil {
			panic(err)
		}

		sock.in <- packet{
			data: marshal(dht.Msg{T: query.T, Y: dht.YResponse, R: &dht.Return{ID: protocol.RandomNodeID()}}),
			addr: plain,
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := s.Query(ctx, mapped, dht.QPing, dht.MsgArgs{ID: protocol.RandomNodeID()})
	if err != nil {
		t.Fatalf("a reply from %s to a query sent to %s was not accepted: %v", plain, mapped, err)
	}

	if res.From != plain {
		t.Fatalf("reply came from %s, want %s", res.From, plain)
	}
}

// A node that repeats its reply must not leave a goroutine blocked for each
// extra copy.
func TestRepeatedReplyDoesNotBlock(t *testing.T) {
	s, _ := newTestServer(t)
	from := netip.MustParseAddrPort("203.0.113.10:6881")
	s.queries["aa"] = pendingQuery{addr: from, ch: make(chan dht.RecvMsg, 1)}

	done := make(chan struct{})

	go func() {
		for range 3 {
			s.handleResponse(dht.RecvMsg{From: from, Msg: dht.Msg{T: "aa", Y: dht.YResponse}})
		}

		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a repeated reply blocked its handler")
	}
}

// A datagram of a few bytes can declare a string of 128 MiB. Decoding must
// not allocate what the datagram merely claims to contain.
func TestReadDoesNotAllocateDeclaredLengths(t *testing.T) {
	_, sock := newTestServer(t)
	from := netip.MustParseAddrPort("203.0.113.10:6881")

	var before, after runtime.MemStats

	runtime.ReadMemStats(&before)

	for range 20 {
		sock.in <- packet{data: []byte("d134217727:xx"), addr: from}
		sock.in <- packet{data: []byte("d1:t134217727:x"), addr: from}
	}

	// The read loop is sequential, so a reply to this query proves that every
	// hostile datagram before it has been decoded.
	sock.in <- packet{
		data: marshal(dht.Msg{Q: dht.QPing, T: "aa", Y: dht.YQuery, A: &dht.MsgArgs{ID: protocol.RandomNodeID()}}),
		addr: from,
	}

	select {
	case <-sock.out:
	case <-time.After(10 * time.Second):
		t.Fatal("no reply to the ping that followed the hostile datagrams")
	}

	runtime.ReadMemStats(&after)

	if got := after.TotalAlloc - before.TotalAlloc; got > 16<<20 {
		t.Fatalf("40 hostile datagrams of at most 15 bytes allocated %d MiB", got>>20)
	}
}

func TestTransactionIDsAreDistinctAndUnpredictable(t *testing.T) {
	a, b := &variantIDIssuer{}, &variantIDIssuer{}
	seen := make(map[string]struct{})

	for range 10_000 {
		seen[a.Issue()] = struct{}{}
	}

	if len(seen) != 10_000 {
		t.Fatalf("issued %d distinct IDs out of 10000", len(seen))
	}

	if b.Issue() == (&variantIDIssuer{}).Issue() {
		t.Fatal("two fresh issuers produced the same first ID")
	}
}

package server

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/dht"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/dht/responder"
	"go.uber.org/zap"
)

// MaxOutgoingPacketBytes caps every serialized outgoing DHT UDP packet to stay
// below minimum IPv6 path MTU (1280 bytes minus IPv6+UDP headers) per BEP 32.
const MaxOutgoingPacketBytes = 1024

var (
	ErrPacketTooLarge = errors.New("dht server: outgoing packet exceeds 1024-byte UDP bound")
	ErrIPv6Disabled   = errors.New("dht server: IPv6 DHT socket is not enabled")
)

type Server interface {
	start() error
	stop()
	Query(ctx context.Context, addr netip.AddrPort, q string, args dht.MsgArgs) (dht.RecvMsg, error)
}

type server struct {
	stopped          chan struct{}
	stopOnce         sync.Once
	mutex            sync.Mutex
	localAddr        netip.AddrPort
	localAddr6       netip.AddrPort
	socket           Socket
	socket6          Socket
	queryTimeout     time.Duration
	queries          map[string]pendingQuery
	responder        responder.Responder
	responderTimeout time.Duration
	idIssuer         IDIssuer
	logger           *zap.SugaredLogger
}

// pendingQuery is a query awaiting its reply from addr.
type pendingQuery struct {
	addr netip.AddrPort
	ch   chan dht.RecvMsg
}

func (s *server) start() error {
	if err := s.socket.Open(s.localAddr); err != nil {
		return fmt.Errorf("could not open IPv4 socket: %w", err)
	}
	if s.socket6 != nil {
		if err := s.socket6.Open(s.localAddr6); err != nil {
			_ = s.socket.Close()
			return fmt.Errorf("could not open IPv6 socket: %w", err)
		}
	}

	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		go s.readSocket(ctx, s.socket)
		if s.socket6 != nil {
			go s.readSocket(ctx, s.socket6)
		}
		<-s.stopped
		cancel()

		_ = s.socket.Close()
		if s.socket6 != nil {
			_ = s.socket6.Close()
		}
	}()

	return nil
}

func (s *server) stop() {
	s.stopOnce.Do(func() {
		close(s.stopped)
	})
}

func (s *server) read(ctx context.Context) {
	s.readSocket(ctx, s.socket)
}

func (s *server) readSocket(ctx context.Context, sock Socket) {
	buffer := make([]byte, 65507)

	for {
		if ctx.Err() != nil {
			return
		}

		n, from, err := sock.Receive(buffer)
		if err != nil {
			if ctx.Err() == nil {
				panic(fmt.Errorf("socket read error: %w", err))
			}

			return
		}

		if n == 0 {
			continue
		}

		var msg dht.Msg

		err = protocol.UnmarshalBencode(buffer[:n], &msg)
		if err != nil {
			s.logger.Debugw("could not unmarshal packet data", "error", err)
			continue
		}

		recvMsg := dht.RecvMsg{
			Msg:  msg,
			From: netip.AddrPortFrom(from.Addr().Unmap(), from.Port()),
		}

		switch msg.Y {
		case dht.YQuery:
			go s.handleQuery(ctx, recvMsg)
		case dht.YResponse, dht.YError:
			go s.handleResponse(recvMsg)
		}
	}
}

func (s *server) handleQuery(ctx context.Context, msg dht.RecvMsg) {
	ctx, cancel := context.WithTimeout(ctx, s.responderTimeout)
	defer cancel()

	res := dht.Msg{
		T: msg.Msg.T,
		Y: dht.YResponse,
	}

	ret, retErr := s.responder.Respond(ctx, msg)
	if retErr != nil {
		dhtErr := &dht.Error{}
		if ok := errors.As(retErr, dhtErr); ok {
			res.E = dhtErr
		} else {
			res.E = &dht.Error{
				Code: dht.ErrorCodeServerError,
				Msg:  "server error",
			}

			s.logger.Errorw("server error", "msg", msg, "retErr", retErr)
		}
	} else {
		res.R = &ret
	}

	if sendErr := s.send(msg.From, res); sendErr != nil {
		s.logger.Debugw("could not send response", "msg", msg, "retErr", sendErr)
	}
}

func (s *server) handleResponse(msg dht.RecvMsg) {
	transactionID := msg.Msg.T

	s.mutex.Lock()
	query, ok := s.queries[transactionID]
	s.mutex.Unlock()

	if !ok || msg.From != query.addr {
		return
	}

	select {
	case query.ch <- msg:
	default:
	}
}

func (s *server) Query(
	ctx context.Context,
	addr netip.AddrPort,
	q string,
	args dht.MsgArgs,
) (r dht.RecvMsg, err error) {
	addr = netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
	if !addr.IsValid() {
		return dht.RecvMsg{}, errors.New("dht server: invalid target address")
	}

	transactionID := s.idIssuer.Issue()
	ch := make(chan dht.RecvMsg, 1)

	s.mutex.Lock()
	s.queries[transactionID] = pendingQuery{addr: addr, ch: ch}
	s.mutex.Unlock()

	defer func() {
		s.mutex.Lock()
		delete(s.queries, transactionID)
		s.mutex.Unlock()
	}()

	msg := dht.Msg{
		Q: q,
		T: transactionID,
		A: &args,
		Y: dht.YQuery,
	}
	if sendErr := s.send(addr, msg); sendErr != nil {
		s.logger.Debugw("could not send query", "msg", msg, "sendErr", sendErr)
		err = sendErr

		return
	}

	queryCtx, cancel := context.WithTimeout(ctx, s.queryTimeout)
	defer cancel()
	select {
	case <-queryCtx.Done():
		err = queryCtx.Err()
		return
	case res, ok := <-ch:
		if !ok {
			err = errors.New("channel closed")
			return
		}

		r = res

		if res.Msg.Y == dht.YError {
			err = res.Msg.E
			if err == nil {
				err = errors.New("error missing from response")
			}
		} else if r.Msg.R == nil {
			err = errors.New("return data missing from response")
		}

		return
	}
}

// encodeBounded serializes msg into <= MaxOutgoingPacketBytes. When a response
// exceeds the bound (for example carrying both nodes, nodes6, values, or
// samples), variable-length contact lists are trimmed progressively while
// preserving required KRPC framing (t, y, r.id, token).
func encodeBounded(msg dht.Msg) ([]byte, error) {
	data, err := bencode.Marshal(msg)
	if err != nil || len(data) <= MaxOutgoingPacketBytes {
		return data, err
	}
	if msg.R == nil {
		return nil, fmt.Errorf("%w (%d bytes)", ErrPacketTooLarge, len(data))
	}

	trimmed := msg
	retCopy := *msg.R
	if retCopy.Samples != nil {
		sCopy := append(dht.CompactInfohashes(nil), (*retCopy.Samples)...)
		retCopy.Samples = &sCopy
	}
	trimmed.R = &retCopy

	for len(data) > MaxOutgoingPacketBytes {
		progressed := false
		if len(retCopy.Values) > 0 {
			retCopy.Values = retCopy.Values[:len(retCopy.Values)/2]
			progressed = true
		} else if len(retCopy.Nodes6) > 0 {
			retCopy.Nodes6 = retCopy.Nodes6[:len(retCopy.Nodes6)/2]
			progressed = true
		} else if len(retCopy.Nodes) > 0 {
			retCopy.Nodes = retCopy.Nodes[:len(retCopy.Nodes)/2]
			progressed = true
		} else if retCopy.Samples != nil && len(*retCopy.Samples) > 0 {
			s := (*retCopy.Samples)[:len(*retCopy.Samples)/2]
			retCopy.Samples = &s
			progressed = true
		}
		if !progressed {
			return nil, fmt.Errorf("%w (%d bytes)", ErrPacketTooLarge, len(data))
		}
		data, err = bencode.Marshal(trimmed)
		if err != nil {
			return nil, err
		}
	}
	return data, nil
}

func (s *server) send(addr netip.AddrPort, msg dht.Msg) error {
	clean := netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
	sock := s.socket
	if clean.Addr().Is6() {
		if s.socket6 == nil {
			return ErrIPv6Disabled
		}
		sock = s.socket6
	}

	data, encodeErr := encodeBounded(msg)
	if encodeErr != nil {
		return encodeErr
	}

	return sock.Send(clean, data)
}

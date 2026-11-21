package metainforequester

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"

	utp "github.com/anacrolix/go-utp"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
)

var (
	errNonPublicAddress = errors.New("refusing to connect to non-public address")
	errUTPClosed        = errors.New("utp dialer closed")
)

// utpDialer manages lazily initialized PureGo uTP sockets per IP address family
// so opt-in uTP fallback works cleanly on both IPv4-only and dual-stack hosts
// without binding any UDP socket when uTP fallback is disabled.
type utpDialer struct {
	allowPrivateAddrs bool

	mu     sync.Mutex
	closed bool
	v4     utp.Socket
	v6     utp.Socket
}

func newUTPDialer(allowPrivateAddrs bool) *utpDialer {
	return &utpDialer{allowPrivateAddrs: allowPrivateAddrs}
}

func (d *utpDialer) socketFor(addr netip.Addr) (utp.Socket, error) {
	addr = addr.Unmap()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, errUTPClosed
	}
	if addr.Is4() {
		if d.v4 == nil {
			s, err := utp.Listen(utp.PureGo, "udp4", "0.0.0.0:0")
			if err != nil {
				return nil, fmt.Errorf("utp listen udp4: %w", err)
			}
			d.v4 = s
		}
		return d.v4, nil
	}
	if d.v6 == nil {
		s, err := utp.Listen(utp.PureGo, "udp6", "[::]:0")
		if err != nil {
			return nil, fmt.Errorf("utp listen udp6: %w", err)
		}
		d.v6 = s
	}
	return d.v6, nil
}

func (d *utpDialer) DialContext(ctx context.Context, addr netip.AddrPort) (net.Conn, error) {
	clean := netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
	if !clean.IsValid() {
		return nil, fmt.Errorf("invalid peer address %s", addr)
	}
	if !d.allowPrivateAddrs && !protocol.IsPublicAddr(clean.Addr()) {
		return nil, fmt.Errorf("%w %s", errNonPublicAddress, clean.String())
	}
	sock, err := d.socketFor(clean.Addr())
	if err != nil {
		return nil, err
	}
	conn, err := sock.DialContext(ctx, "", clean.String())
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		if dlErr := conn.SetDeadline(deadline); dlErr != nil {
			_ = conn.Close()
			return nil, dlErr
		}
	}
	return conn, nil
}

func (d *utpDialer) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	v4, v6 := d.v4, d.v6
	d.v4, d.v6 = nil, nil
	d.mu.Unlock()

	var errs []error
	if v4 != nil {
		if err := v4.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if v6 != nil {
		if err := v6.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

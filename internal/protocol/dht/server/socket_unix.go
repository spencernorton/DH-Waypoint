//go:build !windows

package server

import (
	"errors"
	"fmt"
	"net/netip"

	"golang.org/x/sys/unix"
)

func newSocket() Socket {
	fd, sockErr := unix.Socket(unix.SOCK_DGRAM, unix.AF_INET, 0)
	if sockErr != nil {
		panic(fmt.Errorf("error creating IPv4 socket: %w", sockErr))
	}

	return &socket{
		fd: fd,
	}
}

func newSocket6() Socket {
	fd, sockErr := unix.Socket(unix.AF_INET6, unix.SOCK_DGRAM, 0)
	if sockErr != nil {
		return &failedSocket{err: fmt.Errorf("error creating IPv6 socket: %w", sockErr)}
	}
	// Enforce IPV6_V6ONLY so IPv4 and IPv6 sockets can share the same UDP port
	// without OS-dependent dual-stack bind collisions or IPv4-mapped leaks.
	if optErr := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 1); optErr != nil {
		_ = unix.Close(fd)
		return &failedSocket{err: fmt.Errorf("error setting IPV6_V6ONLY: %w", optErr)}
	}

	return &socket{
		fd: fd,
	}
}

type failedSocket struct {
	err error
}

func (f *failedSocket) Open(netip.AddrPort) error                   { return f.err }
func (f *failedSocket) Close() error                                { return nil }
func (f *failedSocket) Send(netip.AddrPort, []byte) error           { return f.err }
func (f *failedSocket) Receive([]byte) (int, netip.AddrPort, error) { return 0, netip.AddrPort{}, f.err }

type socket struct {
	fd int
}

func (s *socket) Open(localAddr netip.AddrPort) error {
	sAddr, addrErr := addrPortToSockaddr(localAddr)
	if addrErr != nil {
		return addrErr
	}

	return unix.Bind(s.fd, sAddr)
}

func (s *socket) Close() error {
	return unix.Close(s.fd)
}

func (s *socket) Send(remoteAddr netip.AddrPort, data []byte) error {
	sAddr, addrErr := addrPortToSockaddr(remoteAddr)
	if addrErr != nil {
		return addrErr
	}

	return unix.Sendto(s.fd, data, 0, sAddr)
}

func (s *socket) Receive(data []byte) (int, netip.AddrPort, error) {
	n, sAddr, recvErr := unix.Recvfrom(s.fd, data, 0)
	if recvErr != nil {
		return n, netip.AddrPort{}, recvErr
	}

	addr, addrErr := sockaddrToAddrPort(sAddr)
	if addrErr != nil {
		return n, netip.AddrPort{}, addrErr
	}

	return n, addr, nil
}

func addrPortToSockaddr(addr netip.AddrPort) (unix.Sockaddr, error) {
	u := addr.Addr().Unmap()
	if u.Is4() {
		return &unix.SockaddrInet4{
			Addr: u.As4(),
			Port: int(addr.Port()),
		}, nil
	}

	if u.Is6() {
		return &unix.SockaddrInet6{
			Addr: u.As16(),
			Port: int(addr.Port()),
		}, nil
	}

	return nil, errors.New("invalid address")
}

func sockaddrToAddrPort(addr unix.Sockaddr) (netip.AddrPort, error) {
	switch addr := addr.(type) {
	case *unix.SockaddrInet4:
		return netip.AddrPortFrom(netip.AddrFrom4(addr.Addr), uint16(addr.Port)), nil
	case *unix.SockaddrInet6:
		return netip.AddrPortFrom(netip.AddrFrom16(addr.Addr).Unmap(), uint16(addr.Port)), nil
	default:
		return netip.AddrPort{}, fmt.Errorf("unsupported sockaddr type: %T", addr)
	}
}

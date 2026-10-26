package dht

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"strconv"

	"github.com/anacrolix/torrent/bencode"
)

type NodeAddr struct {
	IP   net.IP
	Port int
}

func (a NodeAddr) ToAddrPort() netip.AddrPort {
	addr, _ := netip.AddrFromSlice(a.IP)
	return netip.AddrPortFrom(addr, uint16(a.Port))
}

func NewNodeAddrFromAddrPort(f netip.AddrPort) NodeAddr {
	var me NodeAddr
	me.IP = f.Addr().AsSlice()
	me.Port = int(f.Port())

	return me
}

// A zero Port is taken to mean no port provided, per BEP 7.
func (a NodeAddr) String() string {
	return net.JoinHostPort(a.IP.String(), strconv.FormatInt(int64(a.Port), 10))
}

func (a *NodeAddr) UnmarshalBinary(b []byte) error {
	// Compact addresses contain exactly an IPv4 or IPv6 address and a
	// two-byte port. Validate before allocating or slicing untrusted input.
	if len(b) != net.IPv4len+2 && len(b) != net.IPv6len+2 {
		return fmt.Errorf("invalid compact address length: %d", len(b))
	}
	a.IP = make(net.IP, len(b)-2)
	copy(a.IP, b[:len(b)-2])
	a.Port = int(binary.BigEndian.Uint16(b[len(b)-2:]))

	return nil
}

func (a *NodeAddr) UnmarshalBencode(b []byte) (err error) {
	var _b []byte

	err = bencode.Unmarshal(b, &_b)
	if err != nil {
		return
	}

	return a.UnmarshalBinary(_b)
}

func (a NodeAddr) MarshalBinary() ([]byte, error) {
	var b bytes.Buffer
	_, _ = b.Write(a.IP)

	if err := binary.Write(&b, binary.BigEndian, uint16(a.Port)); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

func (a NodeAddr) MarshalBencode() ([]byte, error) {
	return bencodeBytesResult(a.MarshalBinary())
}

func (a NodeAddr) UDP() *net.UDPAddr {
	return &net.UDPAddr{
		IP:   a.IP,
		Port: a.Port,
	}
}

func (a *NodeAddr) FromUDPAddr(ua *net.UDPAddr) {
	a.IP = ua.IP
	a.Port = ua.Port
}

func (a NodeAddr) Equal(x NodeAddr) bool {
	return a.IP.Equal(x.IP) && a.Port == x.Port
}

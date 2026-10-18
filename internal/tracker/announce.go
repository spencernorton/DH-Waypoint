package tracker

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/anacrolix/torrent/bencode"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
)

// Announce is one parsed tracker announce request.
type Announce struct {
	InfoHash   protocol.ID
	PeerID     []byte
	Port       int
	Uploaded   int64
	Downloaded int64
	Left       int64
	Compact    bool
	Event      string // "", started, completed, stopped
	NumWant    int
	// IP is the optional client-supplied address. Callers should
	// prefer the connection remote address and only fall back to
	// this; it is never trusted for authentication.
	IP string
}

func parseNonNegative(q url.Values, key string) (int64, error) {
	v := q.Get(key)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid %s", key)
	}
	return n, nil
}

// ParseAnnounce validates the query parameters of an announce
// request. info_hash and peer_id arrive as raw 20-byte strings
// (percent-decoded by url.Values).
func ParseAnnounce(q url.Values) (Announce, error) {
	a := Announce{NumWant: DefaultNumWant, Compact: true}
	rawHash := q.Get("info_hash")
	if len(rawHash) != 20 {
		return a, errors.New("invalid info_hash")
	}
	copy(a.InfoHash[:], rawHash)
	peerID := q.Get("peer_id")
	if len(peerID) != 20 {
		return a, errors.New("invalid peer_id")
	}
	a.PeerID = []byte(peerID)
	port, err := strconv.Atoi(q.Get("port"))
	if err != nil || port < 1 || port > 65535 {
		return a, errors.New("invalid port")
	}
	a.Port = port
	if a.Uploaded, err = parseNonNegative(q, "uploaded"); err != nil {
		return a, err
	}
	if a.Downloaded, err = parseNonNegative(q, "downloaded"); err != nil {
		return a, err
	}
	if a.Left, err = parseNonNegative(q, "left"); err != nil {
		return a, err
	}
	if q.Get("compact") == "0" {
		a.Compact = false
	}
	event := strings.ToLower(q.Get("event"))
	switch event {
	case "", "started", "completed", "stopped", "paused", "resumed":
		a.Event = event
	default:
		return a, errors.New("invalid event")
	}
	if v := q.Get("numwant"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 200 {
			return a, errors.New("invalid numwant")
		}
		a.NumWant = n
	}
	a.IP = q.Get("ip")
	return a, nil
}

// Peer is one swarm member as stored and as returned to clients.
type Peer struct {
	PeerID []byte
	IP     string
	Port   int
}

// CompactPeers encodes peers in BEP-23 compact form: IPv4 as
// 4-byte address + 2-byte big-endian port, IPv6 as 16+2. Entries
// whose IP does not parse are skipped.
func CompactPeers(peers []Peer) (v4 []byte, v6 []byte) {
	for _, p := range peers {
		addr, err := netip.ParseAddr(p.IP)
		if err != nil {
			continue
		}
		port := [2]byte{byte(p.Port >> 8), byte(p.Port)}
		if addr.Is4() {
			b := addr.As4()
			v4 = append(v4, b[:]...)
			v4 = append(v4, port[:]...)
		} else {
			b := addr.As16()
			v6 = append(v6, b[:]...)
			v6 = append(v6, port[:]...)
		}
	}
	return v4, v6
}

// AnnounceResponse is the successful announce payload.
type AnnounceResponse struct {
	Interval   int
	Complete   int
	Incomplete int
	Peers      []byte // compact IPv4, when compact requested
	Peers6     []byte // compact IPv6, when compact requested
	PeerList   []Peer // dictionary form, when compact not requested
}

// EncodeAnnounce bencodes an announce response.
func EncodeAnnounce(r AnnounceResponse, compact bool) ([]byte, error) {
	m := map[string]any{
		"interval":   r.Interval,
		"complete":   r.Complete,
		"incomplete": r.Incomplete,
	}
	if compact {
		m["peers"] = string(r.Peers)
		if len(r.Peers6) > 0 {
			m["peers6"] = string(r.Peers6)
		}
	} else {
		list := make([]any, 0, len(r.PeerList))
		for _, p := range r.PeerList {
			list = append(list, map[string]any{
				"peer id": string(p.PeerID),
				"ip":      p.IP,
				"port":    p.Port,
			})
		}
		m["peers"] = list
	}
	return bencode.Marshal(m)
}

// EncodeFailure bencodes a tracker failure response.
func EncodeFailure(reason string) ([]byte, error) {
	return bencode.Marshal(map[string]any{"failure reason": reason})
}

// EncodeScrape bencodes a scrape response keyed by raw info hash.
func EncodeScrape(swams map[protocol.ID]SwarmCounts) ([]byte, error) {
	files := make(map[string]any, len(swams))
	for hash, c := range swams {
		files[string(hash.Bytes())] = map[string]any{
			"complete":   c.Seeders,
			"incomplete": c.Leechers,
			"downloaded": c.Completed,
		}
	}
	return bencode.Marshal(map[string]any{"files": files})
}

// SwarmCounts are the tracker-visible numbers for one torrent.
type SwarmCounts struct {
	Seeders   int
	Leechers  int
	Completed int
}

package protocol

import (
	"crypto/sha1"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
)

// InfoHash is a complete torrent identity, distinct from the 20-byte ID used
// for DHT nodes, peer IDs and discovery keys. The final byte tags the digest:
// zero is SHA-1 (with twelve zero padding bytes), two is SHA-256. The tag and
// padding are never serialized. Use the constructors at untrusted boundaries.
// An array keeps identities comparable for maps without allocating per lookup.
type InfoHash [33]byte

func RandomV1InfoHash() InfoHash { return InfoHashV1(RandomNodeID()) }

func MustNewInfoHash(b []byte) InfoHash {
	h, err := NewInfoHash(b)
	if err != nil {
		panic(err)
	}
	return h
}

func V1InfoHashes(ids []ID) []InfoHash {
	out := make([]InfoHash, len(ids))
	for i, id := range ids {
		out[i] = InfoHashV1(id)
	}
	return out
}

func InfoHashV1(id ID) (h InfoHash) { copy(h[:20], id[:]); return }

func InfoHashV2(sum [32]byte) (h InfoHash) {
	copy(h[:32], sum[:])
	h[32] = 2
	return
}

func NewInfoHash(b []byte) (InfoHash, error) {
	var h InfoHash
	switch len(b) {
	case 20:
		copy(h[:20], b)
	case 32:
		copy(h[:32], b)
		h[32] = 2
	default:
		return h, errors.New("torrent identity must be a full 20-byte SHA-1 or 32-byte SHA-256 digest")
	}
	return h, nil
}

func ParseInfoHash(s string) (InfoHash, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		return InfoHash{}, fmt.Errorf("parse torrent identity: %w", err)
	}
	return NewInfoHash(b)
}

func MustParseInfoHash(s string) InfoHash {
	h, err := ParseInfoHash(s)
	if err != nil {
		panic(err)
	}
	return h
}

func (h InfoHash) Validate() error {
	if h[32] == 2 {
		return nil
	}
	if h[32] != 0 {
		return errors.New("unsupported torrent hash algorithm")
	}
	for _, b := range h[20:32] {
		if b != 0 {
			return errors.New("invalid SHA-1 padding")
		}
	}
	return nil
}

func (h InfoHash) IsV2() bool   { return h[32] == 2 }
func (h InfoHash) IsZero() bool { return h == InfoHash{} }
func (h InfoHash) Algorithm() string {
	if h.IsV2() {
		return "sha256"
	}
	return "sha1"
}
func (h InfoHash) Bytes() []byte {
	if h.IsV2() {
		return h[:32]
	}
	return h[:20]
}
func (h InfoHash) String() string { return hex.EncodeToString(h.Bytes()) }

// WireID deliberately truncates v2 only at the protocol boundary. It is not a
// catalogue identity and must never be used for equality or a database key.
func (h InfoHash) WireID() (id ID) { copy(id[:], h[:20]); return }

func (h InfoHash) MatchesInfoBytes(b []byte) bool {
	if h.Validate() != nil {
		return false
	}
	if h.IsV2() {
		return h == InfoHashV2(sha256.Sum256(b))
	}
	return h == InfoHashV1(ID(sha1.Sum(b)))
}

func (h *InfoHash) Scan(v any) error {
	b, ok := v.([]byte)
	if !ok {
		return errors.New("invalid torrent hash database type")
	}
	parsed, err := NewInfoHash(b)
	if err == nil {
		*h = parsed
	}
	return err
}
func (h InfoHash) Value() (driver.Value, error) {
	if err := h.Validate(); err != nil {
		return nil, err
	}
	return h.Bytes(), nil
}
func (h InfoHash) MarshalBinary() ([]byte, error) {
	if err := h.Validate(); err != nil {
		return nil, err
	}
	return h.Bytes(), nil
}
func (h *InfoHash) UnmarshalBinary(b []byte) error { return h.Scan(b) }
func (h InfoHash) MarshalJSON() ([]byte, error)    { return json.Marshal(h.String()) }
func (h *InfoHash) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := ParseInfoHash(s)
	if err == nil {
		*h = parsed
	}
	return err
}
func (h *InfoHash) UnmarshalGQL(v any) error {
	s, ok := v.(string)
	if !ok {
		return errors.New("invalid torrent hash type")
	}
	parsed, err := ParseInfoHash(s)
	if err == nil {
		*h = parsed
	}
	return err
}
func (h InfoHash) MarshalGQL(w io.Writer) { _, _ = io.WriteString(w, `"`+h.String()+`"`) }

// ExactTopic returns BEP 9's tagged magnet identity. SHA-256 multihashes use
// code 0x12 and digest length 0x20; the complete digest is always included.
func (h InfoHash) ExactTopic() string {
	if h.IsV2() {
		return "urn:btmh:1220" + h.String()
	}
	return "urn:btih:" + h.String()
}

// TorrentIdentities contains only identities validated against the same
// original info dictionary. Names and truncated keys cannot establish a pair.
type TorrentIdentities struct {
	V1 *InfoHash `json:"v1,omitempty"`
	V2 *InfoHash `json:"v2,omitempty"`
}

func (ids TorrentIdentities) Canonical() InfoHash {
	if ids.V1 != nil {
		return *ids.V1
	}
	if ids.V2 != nil {
		return *ids.V2
	}
	return InfoHash{}
}
func (ids TorrentIdentities) Contains(h InfoHash) bool {
	return ids.V1 != nil && *ids.V1 == h || ids.V2 != nil && *ids.V2 == h
}
func (ids TorrentIdentities) Magnet(name string) string {
	var topics []string
	if ids.V1 != nil {
		topics = append(topics, "xt="+ids.V1.ExactTopic())
	}
	if ids.V2 != nil {
		topics = append(topics, "xt="+ids.V2.ExactTopic())
	}
	if name != "" {
		topics = append(topics, "dn="+url.QueryEscape(name))
	}
	return "magnet:?" + strings.Join(topics, "&")
}

// ParseMagnetIdentities parses claimed exact topics. The returned pair is
// untrusted until both digests have been checked against validated metadata.
func ParseMagnetIdentities(s string) (TorrentIdentities, error) {
	if len(s) > 64*1024 {
		return TorrentIdentities{}, errors.New("magnet URI exceeds 64 KiB")
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "magnet" {
		return TorrentIdentities{}, errors.New("invalid magnet URI")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return TorrentIdentities{}, err
	}
	var ids TorrentIdentities
	for _, xt := range q["xt"] {
		var h InfoHash
		switch {
		case strings.HasPrefix(xt, "urn:btih:"):
			digest := strings.TrimPrefix(xt, "urn:btih:")
			if len(digest) == 32 {
				b, decodeErr := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(digest))
				if decodeErr != nil {
					return ids, decodeErr
				}
				h, err = NewInfoHash(b)
			} else {
				h, err = ParseInfoHash(digest)
			}
			if err != nil || h.IsV2() {
				return ids, errors.New("invalid btih exact topic")
			}
			if ids.V1 != nil && *ids.V1 != h {
				return ids, errors.New("conflicting btih exact topics")
			}
			ids.V1 = &h
		case strings.HasPrefix(xt, "urn:btmh:"):
			digest := strings.TrimPrefix(xt, "urn:btmh:")
			if len(digest) != 68 || !strings.HasPrefix(digest, "1220") {
				return ids, errors.New("unsupported btmh multihash")
			}
			h, err = ParseInfoHash(digest[4:])
			if err != nil || !h.IsV2() {
				return ids, errors.New("invalid btmh exact topic")
			}
			if ids.V2 != nil && *ids.V2 != h {
				return ids, errors.New("conflicting btmh exact topics")
			}
			ids.V2 = &h
		}
	}
	if ids.V1 == nil && ids.V2 == nil {
		return ids, errors.New("magnet has no supported exact topic")
	}
	return ids, nil
}

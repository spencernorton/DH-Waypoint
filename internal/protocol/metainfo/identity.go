package metainfo

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/anacrolix/torrent/bencode"
	mi "github.com/anacrolix/torrent/metainfo"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
)

var ErrWrongInfoHash = errors.New("info bytes have wrong hash")
var ErrUnsupportedMetaVersion = errors.New("unsupported meta version")

// ValidatedInfo is metadata indexed from original, canonical info bytes. It
// proves the advertised torrent identities and file layout, not payload data.
// BEP 9 supplies only info bytes, so PieceLayersVerified is false until the
// outer .torrent layers have also been validated (or none are needed).
type ValidatedInfo struct {
	Info                Info
	Identities          protocol.TorrentIdentities
	InfoBytes           []byte
	Files               []mi.FileInfo
	PieceLayersVerified bool
}

// ParseInfoBytes verifies a complete expected identity before parsing.
func ParseInfoBytes(expected protocol.InfoHash, raw []byte) (ValidatedInfo, error) {
	if len(raw) > MaxMetaInfoBytes {
		return ValidatedInfo{}, ErrMetaInfoTooLarge
	}
	if !expected.MatchesInfoBytes(raw) {
		return ValidatedInfo{}, ErrWrongInfoHash
	}
	parsed, err := parseInfoBytes(raw)
	if err == nil && !parsed.Identities.Contains(expected) {
		return ValidatedInfo{}, ErrWrongInfoHash
	}
	return parsed, err
}

// ParseDiscoveredInfo resolves an untyped 20-byte DHT/handshake key. A pure v2
// SHA-1 digest is never invented: only its validated SHA-256 can match the key.
func ParseDiscoveredInfo(key protocol.ID, raw []byte) (ValidatedInfo, error) {
	if len(raw) > MaxMetaInfoBytes {
		return ValidatedInfo{}, ErrMetaInfoTooLarge
	}
	v1 := protocol.InfoHashV1(protocol.ID(sha1.Sum(raw)))
	v2 := protocol.InfoHashV2(sha256.Sum256(raw))
	if v1.WireID() != key && v2.WireID() != key {
		return ValidatedInfo{}, ErrWrongInfoHash
	}
	parsed, err := parseInfoBytes(raw)
	if err != nil {
		return ValidatedInfo{}, err
	}
	if parsed.Identities.V1 != nil && parsed.Identities.V1.WireID() == key || parsed.Identities.V2 != nil && parsed.Identities.V2.WireID() == key {
		return parsed, nil
	}
	return ValidatedInfo{}, ErrWrongInfoHash
}

func parseInfoBytes(raw []byte) (ValidatedInfo, error) {
	var result ValidatedInfo
	if len(raw) == 0 || raw[0] != 'd' {
		return result, errors.New("info must be a dictionary")
	}
	if err := validateEncoding(raw); err != nil {
		return result, err
	}
	var fields map[string]bencode.Bytes
	if err := bencode.Unmarshal(raw, &fields); err != nil {
		return result, err
	}
	if version, ok := fields["meta version"]; ok {
		var v int64
		if err := bencode.Unmarshal(version, &v); err != nil || v != 2 {
			return result, ErrUnsupportedMetaVersion
		}
	}
	var info Info
	if err := bencode.Unmarshal(raw, &info); err != nil {
		return result, fmt.Errorf("unmarshal info: %w", err)
	}
	if err := validateInfo(info); err != nil {
		return result, err
	}
	if info.NameUtf8 != "" {
		if err := validateName(info.NameUtf8); err != nil {
			return result, err
		}
	}
	if p, ok := fields["private"]; ok && !bytes.Equal(p, []byte("i0e")) && !bytes.Equal(p, []byte("i1e")) {
		return result, errors.New("private must be zero or one")
	}
	_, pieces := fields["pieces"]
	_, files := fields["files"]
	_, length := fields["length"]
	hasV1 := info.MetaVersion == 0 || pieces || files || length
	if hasV1 {
		if !pieces || files == length {
			return result, errors.New("v1 requires pieces and exactly one of length or files")
		}
		if err := validateV1(info); err != nil {
			return result, err
		}
		h := protocol.InfoHashV1(protocol.ID(sha1.Sum(raw)))
		result.Identities.V1 = &h
	}
	if info.MetaVersion == 2 {
		if info.PieceLength < 16384 || info.PieceLength&(info.PieceLength-1) != 0 {
			return result, errors.New("v2 piece length must be a power of two and at least 16 KiB")
		}
		var err error
		result.Files, err = parseFileTree(fields["file tree"])
		if err != nil {
			return result, err
		}
		if hasV1 {
			if err := validateHybrid(info, result.Files); err != nil {
				return result, err
			}
		}
		h := protocol.InfoHashV2(sha256.Sum256(raw))
		result.Identities.V2 = &h
	} else {
		if _, ok := fields["file tree"]; ok {
			return result, errors.New("file tree without meta version 2")
		}
		result.Files = info.UpvertedV1Files()
	}
	result.Info = info
	result.InfoBytes = bytes.Clone(raw)
	result.PieceLayersVerified = true
	if info.MetaVersion == 2 {
		for _, f := range result.Files {
			if f.Length > info.PieceLength {
				result.PieceLayersVerified = false
				break
			}
		}
	}
	return result, nil
}

func validateV1(info Info) error {
	if info.Length < 0 {
		return errors.New("negative file length")
	}
	var total int64
	seen := map[string]struct{}{}
	for _, f := range info.UpvertedV1Files() {
		if f.Length < 0 || f.Length > math.MaxInt64-total {
			return errors.New("file length overflows total")
		}
		total += f.Length
		if err := validatePath(f.BestPath()); err != nil {
			return err
		}
		path := strings.Join(f.BestPath(), "/")
		if _, exists := seen[path]; exists {
			return errors.New("duplicate file path")
		}
		seen[path] = struct{}{}
	}
	n := total / info.PieceLength
	if total%info.PieceLength != 0 {
		n++
	}
	if int64(len(info.Pieces))/20 != n || len(info.Pieces)%20 != 0 {
		return errors.New("v1 piece hash count does not match file lengths")
	}
	return nil
}

func parseFileTree(raw []byte) ([]mi.FileInfo, error) {
	var files []mi.FileInfo
	var total int64
	var walk func([]byte, []string) error
	walk = func(node []byte, path []string) error {
		if len(node) == 0 || node[0] != 'd' {
			return errors.New("file tree node must be a dictionary")
		}
		var entries map[string]bencode.Bytes
		if err := bencode.Unmarshal(node, &entries); err != nil {
			return err
		}
		if prop, ok := entries[""]; ok {
			if len(path) == 0 || len(entries) != 1 {
				return errors.New("file tree file has directory siblings or is the root")
			}
			var properties map[string]bencode.Bytes
			if len(prop) == 0 || prop[0] != 'd' {
				return errors.New("file properties must be a dictionary")
			}
			if err := bencode.Unmarshal(prop, &properties); err != nil {
				return err
			}
			var length int64
			if b, ok := properties["length"]; !ok {
				return errors.New("file tree file missing length")
			} else if err := bencode.Unmarshal(b, &length); err != nil {
				return err
			}
			if length < 0 || length > math.MaxInt64-total {
				return errors.New("file tree length overflows total")
			}
			total += length
			var root string
			if b, ok := properties["pieces root"]; ok {
				if err := bencode.Unmarshal(b, &root); err != nil {
					return err
				}
				if len(root) != 32 {
					return errors.New("pieces root must be 32 bytes")
				}
			}
			if length > 0 && len(root) != 32 {
				return errors.New("non-empty file missing pieces root")
			}
			if length == 0 && root != "" {
				return errors.New("empty file must not have a pieces root")
			}
			f := mi.FileInfo{Path: slices.Clone(path), PathUtf8: slices.Clone(path), Length: length}
			if root != "" {
				copy(f.PiecesRoot.Value[:], root)
				f.PiecesRoot.Ok = true
			}
			files = append(files, f)
			if len(files) > MaxFilesCount {
				return ErrTooManyFiles
			}
			return nil
		}
		if len(entries) == 0 {
			return errors.New("empty file tree directory")
		}
		keys := make([]string, 0, len(entries))
		for key := range entries {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			next := append(slices.Clone(path), key)
			if err := validatePath(next); err != nil {
				return err
			}
			if err := walk(entries[key], next); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(raw, nil); err != nil {
		return nil, err
	}
	return files, nil
}

func validateHybrid(info Info, v2 []mi.FileInfo) error {
	v1 := info.UpvertedV1Files()
	var offset int64
	j := 0
	for i, f := range v1 {
		if strings.Contains(f.Attr, "p") {
			if i == 0 || f.Length <= 0 || f.Length != (info.PieceLength-offset%info.PieceLength)%info.PieceLength {
				return errors.New("invalid hybrid padding alignment")
			}
			offset += f.Length
			continue
		}
		if j >= len(v2) || f.Length != v2[j].Length {
			return errors.New("hybrid file length or count mismatch")
		}
		path := f.BestPath()
		if len(info.Files) == 0 {
			path = []string{info.BestName()}
		}
		if !slices.Equal(path, v2[j].BestPath()) || f.Length > 0 && offset%info.PieceLength != 0 {
			return errors.New("hybrid file path, order or alignment mismatch")
		}
		offset += f.Length
		j++
	}
	if j != len(v2) {
		return errors.New("hybrid file count mismatch")
	}
	return nil
}

// ParseTorrentBytes validates a complete .torrent, hashing the original info
// substring and checking every required v2 piece layer against its file root.
// expected may be nil for a local import whose identities are derived here.
func ParseTorrentBytes(raw []byte, expected *protocol.InfoHash) (ValidatedInfo, error) {
	if len(raw) > 2*MaxMetaInfoBytes {
		return ValidatedInfo{}, ErrMetaInfoTooLarge
	}
	if err := validateEncoding(raw); err != nil {
		return ValidatedInfo{}, err
	}
	var fields map[string]bencode.Bytes
	if err := bencode.Unmarshal(raw, &fields); err != nil {
		return ValidatedInfo{}, err
	}
	infoBytes := fields["info"]
	if len(infoBytes) > MaxMetaInfoBytes {
		return ValidatedInfo{}, ErrMetaInfoTooLarge
	}
	var parsed ValidatedInfo
	var err error
	if expected != nil {
		parsed, err = ParseInfoBytes(*expected, infoBytes)
	} else {
		parsed, err = parseInfoBytes(infoBytes)
	}
	if err != nil {
		return ValidatedInfo{}, err
	}
	if parsed.Info.MetaVersion == 2 {
		var layers map[string]string
		if b, ok := fields["piece layers"]; ok {
			if err := bencode.Unmarshal(b, &layers); err != nil {
				return ValidatedInfo{}, err
			}
		}
		if err := validatePieceLayers(parsed, layers); err != nil {
			return ValidatedInfo{}, err
		}
		parsed.PieceLayersVerified = true
	}
	return parsed, nil
}

func validatePieceLayers(parsed ValidatedInfo, layers map[string]string) error {
	needed := map[string]bool{}
	for _, file := range parsed.Files {
		if file.Length <= parsed.Info.PieceLength {
			continue
		}
		root := string(file.PiecesRoot.Value[:])
		needed[root] = true
		layer, ok := layers[root]
		n := (file.Length-1)/parsed.Info.PieceLength + 1
		if !ok || n > MaxMetaInfoBytes/32 || int64(len(layer)) != n*32 {
			return errors.New("missing or wrong-sized v2 piece layer")
		}
		var padding [32]byte
		for size := int64(16384); size < parsed.Info.PieceLength; size *= 2 {
			padding = sha256.Sum256(append(slices.Clone(padding[:]), padding[:]...))
		}
		width := 1
		for int64(width) < n {
			width *= 2
		}
		hashes := make([][32]byte, width)
		for i := range width {
			if int64(i) < n {
				copy(hashes[i][:], layer[i*32:(i+1)*32])
			} else {
				hashes[i] = padding
			}
		}
		for len(hashes) > 1 {
			for i := 0; i < len(hashes)/2; i++ {
				var pair [64]byte
				copy(pair[:32], hashes[2*i][:])
				copy(pair[32:], hashes[2*i+1][:])
				hashes[i] = sha256.Sum256(pair[:])
			}
			hashes = hashes[:len(hashes)/2]
		}
		if hashes[0] != file.PiecesRoot.Value {
			return errors.New("v2 piece layer does not match pieces root")
		}
	}
	for root := range layers {
		if !needed[root] {
			return errors.New("unreferenced v2 piece layer")
		}
	}
	return nil
}

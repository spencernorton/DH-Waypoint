package metainfo

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/stretchr/testify/require"
)

func encoded(t *testing.T, value any) []byte {
	t.Helper()
	b, err := bencode.Marshal(value)
	require.NoError(t, err)
	return b
}

func v2Fixture() map[string]any {
	root := sha256.Sum256([]byte("synthetic data"))
	return map[string]any{
		"name": "Synthetic.txt", "meta version": int64(2), "piece length": int64(16384),
		"file tree": map[string]any{"Synthetic.txt": map[string]any{"": map[string]any{"length": int64(14), "pieces root": root[:]}}},
	}
}

func TestParseIdentityV1V2AndHybrid(t *testing.T) {
	for _, kind := range []string{"v1", "v2", "hybrid"} {
		t.Run(kind, func(t *testing.T) {
			info := v2Fixture()
			if kind != "v2" {
				piece := sha1.Sum([]byte("synthetic data"))
				info["length"], info["pieces"] = int64(14), piece[:]
			}
			if kind == "v1" {
				delete(info, "meta version")
				delete(info, "file tree")
			}
			raw := encoded(t, info)
			v1 := protocol.InfoHashV1(protocol.ID(sha1.Sum(raw)))
			v2 := protocol.InfoHashV2(sha256.Sum256(raw))
			expected := v1
			if kind == "v2" {
				expected = v2
			}
			parsed, err := ParseInfoBytes(expected, raw)
			require.NoError(t, err)
			require.Equal(t, kind != "v2", parsed.Identities.V1 != nil)
			require.Equal(t, kind != "v1", parsed.Identities.V2 != nil)
			require.Equal(t, expected, parsed.Identities.Canonical())
			require.Equal(t, raw, parsed.InfoBytes)
			require.Len(t, parsed.Files, 1)
			discovered, err := ParseDiscoveredInfo(expected.WireID(), raw)
			require.NoError(t, err)
			require.Equal(t, parsed.Identities, discovered.Identities)
			if kind != "v1" {
				byV2, err := ParseInfoBytes(v2, raw)
				require.NoError(t, err)
				require.Equal(t, parsed.Identities, byV2.Identities)
			}
			if kind == "v2" {
				_, err := ParseInfoBytes(v1, raw)
				require.ErrorIs(t, err, ErrWrongInfoHash)
				_, err = ParseMetaInfoBytes(v1.WireID(), raw)
				require.ErrorIs(t, err, ErrWrongInfoHash, "legacy callers must not invent a pure-v2 SHA-1 identity")
			} else {
				_, err := ParseMetaInfoBytes(v1.WireID(), raw)
				require.NoError(t, err)
			}
			wrong := expected
			wrong[19] ^= 1
			_, err = ParseInfoBytes(wrong, raw)
			require.ErrorIs(t, err, ErrWrongInfoHash)
		})
	}
}

func TestV2RejectsMalformedTreesAndHybrids(t *testing.T) {
	mutations := map[string]func(map[string]any){
		"newer version":    func(m map[string]any) { m["meta version"] = 3 },
		"small pieces":     func(m map[string]any) { m["piece length"] = 8192 },
		"non-power pieces": func(m map[string]any) { m["piece length"] = 20000 },
		"file at root":     func(m map[string]any) { m["file tree"] = map[string]any{"": map[string]any{"length": 0}} },
		"root too short": func(m map[string]any) {
			m["file tree"] = map[string]any{"file": map[string]any{"": map[string]any{"length": 1, "pieces root": "short"}}}
		},
		"missing root": func(m map[string]any) {
			m["file tree"] = map[string]any{"file": map[string]any{"": map[string]any{"length": 1}}}
		},
		"missing length": func(m map[string]any) { m["file tree"] = map[string]any{"file": map[string]any{"": map[string]any{}}} },
		"negative length": func(m map[string]any) {
			m["file tree"] = map[string]any{"file": map[string]any{"": map[string]any{"length": -1}}}
		},
		"file and directory": func(m map[string]any) {
			m["file tree"] = map[string]any{"file": map[string]any{"": map[string]any{"length": 0}, "child": map[string]any{"": map[string]any{"length": 0}}}}
		},
		"path traversal": func(m map[string]any) {
			m["file tree"] = map[string]any{"..": map[string]any{"file": map[string]any{"": map[string]any{"length": 0}}}}
		},
		"invalid utf8": func(m map[string]any) {
			m["file tree"] = map[string]any{"bad\x80": map[string]any{"": map[string]any{"length": 0}}}
		},
		"nul path": func(m map[string]any) {
			m["file tree"] = map[string]any{"bad\x00": map[string]any{"": map[string]any{"length": 0}}}
		},
		"hybrid name mismatch":   func(m map[string]any) { m["length"], m["pieces"], m["name"] = 14, make([]byte, 20), "Other.txt" },
		"hybrid length mismatch": func(m map[string]any) { m["length"], m["pieces"] = 15, make([]byte, 20) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			m := v2Fixture()
			mutate(m)
			raw := encoded(t, m)
			_, err := ParseInfoBytes(protocol.InfoHashV2(sha256.Sum256(raw)), raw)
			require.Error(t, err)
		})
	}
}

func TestOriginalBytesAndEncodingBounds(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte("d4:name1:a4:name1:be"),
		[]byte("d1:bi1e1:ai2ee"),
		[]byte("d1:ai01ee"),
		[]byte("d1:ai-0ee"),
		[]byte("d01:a1:be"),
		[]byte(strings.Repeat("l", MaxPathDepth+10) + strings.Repeat("e", MaxPathDepth+10)),
	} {
		_, err := ParseInfoBytes(protocol.InfoHashV2(sha256.Sum256(raw)), raw)
		require.Error(t, err, string(raw))
	}
	raw := encoded(t, v2Fixture())
	modified := bytes.Replace(raw, []byte("Synthetic"), []byte("synthetic"), 1)
	_, err := ParseInfoBytes(protocol.InfoHashV2(sha256.Sum256(raw)), modified)
	require.ErrorIs(t, err, ErrWrongInfoHash)
}

func TestHybridRepeatedPaddingAndAlignedFileOrder(t *testing.T) {
	root := sha256.Sum256([]byte("x"))
	file := map[string]any{"": map[string]any{"length": 1, "pieces root": root[:]}}
	v1 := []any{map[string]any{"length": 1, "path": []string{"a"}}, map[string]any{"length": 16383, "path": []string{".pad", "16383"}, "attr": "p"}, map[string]any{"length": 1, "path": []string{"b"}}, map[string]any{"length": 16383, "path": []string{".pad", "16383"}, "attr": "p"}, map[string]any{"length": 1, "path": []string{"c"}}}
	m := map[string]any{"name": "Synthetic.Collection", "piece length": 16384, "pieces": make([]byte, 60), "files": v1, "meta version": 2, "file tree": map[string]any{"a": file, "b": file, "c": file}}
	raw := encoded(t, m)
	parsed, err := ParseInfoBytes(protocol.InfoHashV2(sha256.Sum256(raw)), raw)
	require.NoError(t, err)
	require.NotNil(t, parsed.Identities.V1)
	require.Len(t, parsed.Files, 3)
	v1[1].(map[string]any)["length"] = 16382
	raw = encoded(t, m)
	_, err = ParseInfoBytes(protocol.InfoHashV2(sha256.Sum256(raw)), raw)
	require.Error(t, err, "a hybrid cannot shift a payload file off the v2 piece boundary")
	v1[1].(map[string]any)["length"] = 16383
	v1[2].(map[string]any)["path"] = []string{"c"}
	raw = encoded(t, m)
	_, err = ParseInfoBytes(protocol.InfoHashV2(sha256.Sum256(raw)), raw)
	require.Error(t, err, "file order and names must match")
}

func TestV2OuterPieceLayersAndInfoOnlyDistinction(t *testing.T) {
	// Three 16 KiB blocks exercise the zero hash used to balance the tree.
	a, b, c := sha256.Sum256([]byte("a")), sha256.Sum256([]byte("b")), sha256.Sum256([]byte("c"))
	var zero [32]byte
	ab := sha256.Sum256(append(bytes.Clone(a[:]), b[:]...))
	cz := sha256.Sum256(append(bytes.Clone(c[:]), zero[:]...))
	root := sha256.Sum256(append(bytes.Clone(ab[:]), cz[:]...))
	layer := append(append(bytes.Clone(a[:]), b[:]...), c[:]...)
	m := v2Fixture()
	m["file tree"] = map[string]any{"Synthetic.txt": map[string]any{"": map[string]any{"length": 3*16384 - 1, "pieces root": root[:]}}}
	info := encoded(t, m)
	hash := protocol.InfoHashV2(sha256.Sum256(info))
	parsed, err := ParseInfoBytes(hash, info)
	require.NoError(t, err)
	require.False(t, parsed.PieceLayersVerified)
	torrent := map[string]any{"info": bencode.Bytes(info), "piece layers": map[string]string{string(root[:]): string(layer)}}
	parsed, err = ParseTorrentBytes(encoded(t, torrent), &hash)
	require.NoError(t, err)
	require.True(t, parsed.PieceLayersVerified)
	delete(torrent, "piece layers")
	_, err = ParseTorrentBytes(encoded(t, torrent), &hash)
	require.ErrorContains(t, err, "piece layer")
	layer[0] ^= 1
	torrent["piece layers"] = map[string]string{string(root[:]): string(layer)}
	_, err = ParseTorrentBytes(encoded(t, torrent), &hash)
	require.ErrorContains(t, err, "does not match")
}

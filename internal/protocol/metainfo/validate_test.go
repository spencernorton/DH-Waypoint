package metainfo

import (
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	mi "github.com/anacrolix/torrent/metainfo"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validInfo returns a baseline Info that passes validateInfo. Tests
// mutate one field then reassert.
func validInfo() Info {
	return Info{
		Name:        "Some.Show.S01E01.1080p.WEB-DL.mkv",
		PieceLength: 262144, // 256 KiB — typical for 1080p video
		Files: []mi.FileInfo{
			{Path: []string{"Some.Show.S01E01.1080p.WEB-DL.mkv"}, Length: 1024},
		},
	}
}

func TestValidateInfo_AcceptsValid(t *testing.T) {
	assert.NoError(t, validateInfo(validInfo()))
}

func TestValidateInfo_NameRejections(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Info)
		wantErr error
	}{
		{
			name:    "empty",
			mutate:  func(i *Info) { i.Name = "" },
			wantErr: ErrEmptyName,
		},
		{
			name:    "too long",
			mutate:  func(i *Info) { i.Name = strings.Repeat("x", MaxNameLen+1) },
			wantErr: ErrNameTooLong,
		},
		{
			name:    "invalid UTF-8 (lone continuation byte)",
			mutate:  func(i *Info) { i.Name = "valid\x80suffix" },
			wantErr: ErrInvalidUTF8,
		},
		{
			name:    "NUL byte (truncation attack)",
			mutate:  func(i *Info) { i.Name = "safe\x00../etc/passwd" },
			wantErr: ErrNullByte,
		},
		{
			name:    "control char (ANSI escape)",
			mutate:  func(i *Info) { i.Name = "evil\x1b[31m" },
			wantErr: ErrControlChar,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			info := validInfo()
			tc.mutate(&info)
			err := validateInfo(info)
			assert.Error(t, err)
			assert.True(t, errors.Is(err, tc.wantErr), "got %v, want sentinel %v", err, tc.wantErr)
		})
	}
}

func TestValidateInfo_PathRejections(t *testing.T) {
	cases := []struct {
		name    string
		path    []string
		wantErr error
	}{
		{name: "empty component", path: []string{"dir", "", "file.mkv"}, wantErr: ErrEmptyPathComponent},
		{name: "dot traversal", path: []string{".", "file.mkv"}, wantErr: ErrPathTraversal},
		{name: "dotdot traversal", path: []string{"..", "etc", "passwd"}, wantErr: ErrPathTraversal},
		{name: "forward slash in component", path: []string{"a/b", "file"}, wantErr: ErrAbsolutePath},
		{name: "backslash in component", path: []string{"a\\b", "file"}, wantErr: ErrAbsolutePath},
		{name: "NUL byte in component", path: []string{"good", "bad\x00.txt"}, wantErr: ErrNullByte},
		{name: "invalid UTF-8 in component", path: []string{"good", "bad\x80name"}, wantErr: ErrInvalidUTF8},
		{name: "control char in component", path: []string{"good", "evil\x07bell"}, wantErr: ErrControlChar},
		{name: "component too long", path: []string{strings.Repeat("x", MaxPathComponentLen+1)}, wantErr: ErrPathTooLong},
		{name: "path too deep", path: deepPath(MaxPathDepth + 1), wantErr: ErrPathTooDeep},
		{name: "whole path too long", path: longPath(MaxPathLen + 1), wantErr: ErrWholePathTooLong},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			info := validInfo()
			info.Files = []mi.FileInfo{{Path: tc.path, Length: 1}}
			err := validateInfo(info)
			assert.Error(t, err)
			assert.True(t, errors.Is(err, tc.wantErr), "got %v, want sentinel %v", err, tc.wantErr)
		})
	}
}

func TestValidateInfo_PieceLengthRejections(t *testing.T) {
	// PieceLength is the most underrated DoS vector — a hostile peer can
	// claim 1 GiB pieces and downstream allocators will pre-size to match.
	cases := []struct {
		name        string
		pieceLength int64
		wantErr     error
	}{
		{name: "zero", pieceLength: 0, wantErr: ErrPieceLenOutOfRange},
		{name: "negative", pieceLength: -1, wantErr: ErrPieceLenOutOfRange},
		{name: "1 GiB attack", pieceLength: 1 << 30, wantErr: ErrPieceLenOutOfRange},
		{name: "just over MaxPieceLen", pieceLength: MaxPieceLen + 1, wantErr: ErrPieceLenOutOfRange},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			info := validInfo()
			info.PieceLength = tc.pieceLength
			err := validateInfo(info)
			assert.Error(t, err)
			assert.True(t, errors.Is(err, tc.wantErr))
		})
	}
}

func TestValidateInfo_PieceLengthAtBoundariesAccepted(t *testing.T) {
	for _, pl := range []int64{MinPieceLen, 16 * 1024, 262144, MaxPieceLen} {
		pl := pl
		t.Run("", func(t *testing.T) {
			info := validInfo()
			info.PieceLength = pl
			assert.NoError(t, validateInfo(info))
		})
	}
}

func TestValidateInfo_RejectsTabAndNewlineInFilename(t *testing.T) {
	// Tightening: legitimate filenames have no use for \t \n \r either —
	// allowing them invites log-injection and CSV-injection downstream.
	for _, bad := range []string{"name\twith\ttab", "name\nwith\nnewline", "name\rwith\rcr"} {
		bad := bad
		t.Run("", func(t *testing.T) {
			info := validInfo()
			info.Name = bad
			err := validateInfo(info)
			assert.Error(t, err)
			assert.True(t, errors.Is(err, ErrControlChar))
		})
	}
}

func TestValidateInfo_TooManyFiles(t *testing.T) {
	info := validInfo()
	info.Files = make([]mi.FileInfo, MaxFilesCount+1)
	for i := range info.Files {
		info.Files[i] = mi.FileInfo{Path: []string{"f"}, Length: 1}
	}
	err := validateInfo(info)
	assert.Error(t, err)
	assert.True(t, errors.Is(err, ErrTooManyFiles))
}

func TestValidateInfo_EmptyFilesIsValid(t *testing.T) {
	// Single-file torrents (Length set, Files empty) must validate.
	info := validInfo()
	info.Files = nil
	assert.NoError(t, validateInfo(info))
}

func TestParseMetaInfoBytes_RejectsOversizedInput(t *testing.T) {
	// Pre-unmarshal size cap fires before the hash check is even attempted.
	oversized := make([]byte, MaxMetaInfoBytes+1)
	_, err := ParseMetaInfoBytes([20]byte{}, oversized)
	assert.Error(t, err)
	assert.True(t, errors.Is(err, ErrMetaInfoTooLarge))
}

// deepPath builds a Path slice of length n with each component "d".
func deepPath(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "d"
	}
	return out
}

// longPath builds a Path that is n bytes long once joined with "/".
func longPath(n int) []string {
	const part = 200
	var out []string
	for n > part {
		out = append(out, strings.Repeat("x", part))
		n -= part + 1
	}
	return append(out, strings.Repeat("x", n))
}

func TestValidateInfo_WholePathLength(t *testing.T) {
	require.Len(t, strings.Join(longPath(MaxPathLen), "/"), MaxPathLen)

	info := validInfo()
	info.Files = []mi.FileInfo{{Path: longPath(MaxPathLen), Length: 1}}
	assert.NoError(t, validateInfo(info))

	// path.utf-8 is the stored form when present, so it is bounded as well.
	info.Files = []mi.FileInfo{{Path: []string{"a.txt"}, PathUtf8: longPath(MaxPathLen + 1), Length: 1}}
	assert.ErrorIs(t, validateInfo(info), ErrWholePathTooLong)
}

// Every parse entry point applies MaxPathLen to each form a file path can
// take: v1 path, v1 path.utf-8 and a v2 file tree.
func TestParsersApplyWholePathLength(t *testing.T) {
	piece := sha1.Sum([]byte("synthetic data"))
	root := sha256.Sum256([]byte("synthetic data"))
	v1 := func(file map[string]any) map[string]any {
		file["length"] = int64(14)
		return map[string]any{"name": "Synthetic", "piece length": int64(16384), "pieces": piece[:], "files": []any{file}}
	}
	v2 := func(path []string) map[string]any {
		tree := map[string]any{"": map[string]any{"length": int64(14), "pieces root": root[:]}}
		for i := len(path) - 1; i >= 0; i-- {
			tree = map[string]any{path[i]: tree}
		}
		return map[string]any{"name": "Synthetic", "meta version": int64(2), "piece length": int64(16384), "file tree": tree}
	}
	for _, tc := range []struct {
		name string
		info func(path []string) map[string]any
	}{
		{"v1 path", func(path []string) map[string]any { return v1(map[string]any{"path": path}) }},
		{"v1 path.utf-8", func(path []string) map[string]any {
			return v1(map[string]any{"path": []string{"a.txt"}, "path.utf-8": path})
		}},
		{"v2 file tree", v2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, length := range []int{MaxPathLen, MaxPathLen + 1} {
				raw := encoded(t, tc.info(longPath(length)))
				id := protocol.ID(sha1.Sum(raw))
				expected := protocol.InfoHashV1(id)
				isV2 := tc.name == "v2 file tree"
				if isV2 {
					expected = protocol.InfoHashV2(sha256.Sum256(raw))
				}
				_, discovered := ParseDiscoveredInfo(expected.WireID(), raw)
				_, verified := ParseInfoBytes(expected, raw)
				_, torrentFile := ParseTorrentBytes(encoded(t, map[string]any{"info": tc.info(longPath(length))}), nil)
				errs := []error{discovered, verified, torrentFile}
				if !isV2 {
					_, legacy := ParseMetaInfoBytes(id, raw)
					errs = append(errs, legacy)
				}
				for i, err := range errs {
					if length > MaxPathLen {
						require.ErrorIs(t, err, ErrWholePathTooLong, "parser %d", i)
					} else {
						require.NoError(t, err, "parser %d", i)
					}
				}
			}
		})
	}
}

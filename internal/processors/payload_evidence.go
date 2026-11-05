package processors

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	mi "github.com/anacrolix/torrent/metainfo"
	"path"
	"strings"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
)

const PayloadEvidenceContract = serving.PayloadEvidenceContract
const payloadProbeBytes = int64(2 * 1024 * 1024)
const payloadProofBytes = 16 * 1024 * 1024
const payloadInfoBytes = 2 * 1024 * 1024

var errPayloadEvidence = errors.New("invalid_payload_evidence")

func (s *Store) SetPublicationConfig(cfg serving.Config) error {
	if cfg.RequiredPayloadVerifierArtifactDigest != "" && s.cfg.PayloadVerifierToken == "" {
		return errors.New("required payload evidence needs a dedicated verifier credential")
	}
	s.publication = cfg
	return nil
}

type payloadPiece struct {
	Index int    `json:"index"`
	Data  []byte `json:"data"`
}
type payloadFileReport struct {
	Index         int    `json:"index"`
	Format        string `json:"format"`
	MediaType     string `json:"media_type"`
	DecodedFrames int    `json:"decoded_frames"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	SampleRate    int    `json:"sample_rate"`
	SampleDigest  string `json:"sample_digest"`
}
type payloadEvidence struct {
	Info   []byte              `json:"info"`
	Pieces []payloadPiece      `json:"pieces"`
	Files  []payloadFileReport `json:"files"`
	Claim  *payloadClaim       `json:"claim,omitempty"`
}
type payloadReceipt struct {
	InfoSHA256  string              `json:"info_sha256"`
	PieceSHA256 map[int]string      `json:"piece_sha256"`
	Files       []payloadFileReport `json:"files"`
	Claim       *payloadClaim       `json:"claim,omitempty"`
}

func mediaProbeFormat(name string, kind string) (format, stream string) {
	ext := strings.ToLower(path.Ext(name))
	video := kind == "movie" || kind == "tv_show" || kind == "xxx"
	audio := kind == "music" || kind == "audiobook"
	if video {
		switch ext {
		case ".mkv", ".webm":
			return "matroska", "video"
		case ".mp4", ".m4v", ".mov":
			return "mov", "video"
		case ".avi":
			return "avi", "video"
		case ".ts", ".m2ts":
			return "mpegts", "video"
		case ".mpg", ".mpeg", ".vob":
			return "mpeg", "video"
		case ".wmv":
			return "asf", "video"
		case ".flv":
			return "flv", "video"
		case ".ogm":
			return "ogg", "video"
		}
	}
	if audio {
		switch ext {
		case ".flac":
			return "flac", "audio"
		case ".mp3":
			return "mp3", "audio"
		case ".m4a", ".m4b":
			return "mov", "audio"
		case ".ogg", ".opus":
			return "ogg", "audio"
		case ".wav":
			return "wav", "audio"
		case ".aac":
			return "aac", "audio"
		}
	}
	return "", ""
}

// Accept only known container signatures in bytes authenticated by torrent
// pieces. The independent, pinned sandbox must additionally decode a frame.
func mediaProbeMagic(format string, b []byte) bool {
	if len(b) < 16 || bytes.HasPrefix(b, []byte("MZ")) || bytes.HasPrefix(b, []byte("\x7fELF")) {
		return false
	}
	switch format {
	case "matroska":
		return bytes.HasPrefix(b, []byte{0x1a, 0x45, 0xdf, 0xa3})
	case "mov":
		return slicesString([]string{"ftyp", "moov", "mdat", "free", "wide"}, string(b[4:8])) && binary.BigEndian.Uint32(b[:4]) >= 8
	case "avi":
		return string(b[:4]) == "RIFF" && string(b[8:12]) == "AVI "
	case "wav":
		return string(b[:4]) == "RIFF" && string(b[8:12]) == "WAVE"
	case "mpegts":
		return b[0] == 0x47 && len(b) > 376 && b[188] == 0x47 && b[376] == 0x47 || len(b) > 388 && b[4] == 0x47 && b[196] == 0x47 && b[388] == 0x47
	case "mpeg":
		return bytes.HasPrefix(b, []byte{0, 0, 1, 0xba})
	case "asf":
		return bytes.HasPrefix(b, []byte{0x30, 0x26, 0xb2, 0x75, 0x8e, 0x66, 0xcf, 0x11, 0xa6, 0xd9, 0, 0xaa, 0, 0x62, 0xce, 0x6c})
	case "flv":
		return string(b[:3]) == "FLV"
	case "ogg":
		return string(b[:4]) == "OggS"
	case "flac":
		return string(b[:4]) == "fLaC"
	case "mp3":
		return string(b[:3]) == "ID3" || b[0] == 0xff && b[1]&0xe0 == 0xe0
	case "aac":
		return b[0] == 0xff && b[1]&0xf6 == 0xf0
	}
	return false
}

func slicesString(values []string, value string) bool {
	for _, x := range values {
		if x == value {
			return true
		}
	}
	return false
}

func validatePayloadEvidence(hash []byte, src source, kind string, evidence payloadEvidence) (payloadReceipt, error) {
	out := payloadReceipt{PieceSHA256: map[int]string{}}
	bad := func(reason string) (payloadReceipt, error) {
		return out, fmt.Errorf("%s: %w", reason, errPayloadEvidence)
	}
	if publicationPayloadReason(src, kind) != "" {
		return bad("unsafe_payload_structure")
	}
	if err := validatePayloadClaim(src, kind, evidence.Claim, time.Now().UTC()); err != nil {
		return out, err
	}
	if len(evidence.Info) == 0 || len(evidence.Info) > payloadInfoBytes || len(evidence.Pieces) == 0 || len(evidence.Pieces) > 2048 || len(evidence.Files) == 0 || len(evidence.Files) > 32 {
		return bad("bounds")
	}
	h, err := protocol.NewInfoHash(hash)
	if err != nil {
		return bad("identity")
	}
	meta, err := metainfo.ParseInfoBytes(h, evidence.Info)
	if err != nil {
		return bad("metainfo_identity")
	}
	// Pure v2 data requires a separately qualified Merkle proof path. It stays
	// held; never substitute a truncated v2 key or an invented SHA-1 identity.
	if meta.Identities.V1 == nil {
		return bad("unsupported_v2_payload_proof")
	}
	projected := model.TorrentFromMetadata(meta, "", false, 257)
	if projected.Private || projected.Name != src.Name || int64(projected.Size) != src.Size || string(projected.FilesStatus) != src.FilesStatus || len(projected.Files) != len(src.Files) {
		return bad("source_changed")
	}
	if (src.InfoHashV2 != nil) != (meta.Identities.V2 != nil) || src.InfoHashV2 != nil && meta.Identities.V2.String() != *src.InfoHashV2 {
		return bad("hybrid_identity")
	}
	byIndex := map[int]sourceFile{}
	for _, f := range src.Files {
		if _, ok := byIndex[f.Index]; ok {
			return bad("duplicate_source_index")
		}
		byIndex[f.Index] = f
	}
	for _, f := range projected.Files {
		got, ok := byIndex[int(f.Index)]
		if !ok || got.Path != f.Path || got.Size != int64(f.Size) {
			return bad("file_tree_changed")
		}
	}
	rawFiles := meta.Info.UpvertedV1Files()
	total := int64(0)
	for _, f := range rawFiles {
		if strings.ContainsAny(f.Attr, "lx") || len(f.SymlinkPath) > 0 {
			return bad("unsafe_file_attributes")
		}
		total += f.Length
	}
	if total <= 0 || total > 1<<40 {
		return bad("payload_size")
	}
	pieces := map[int][]byte{}
	bytesUsed := 0
	for _, p := range evidence.Pieces {
		if p.Index < 0 || p.Index >= len(meta.Info.Pieces)/20 {
			return bad("piece_index")
		}
		if _, ok := pieces[p.Index]; ok {
			return bad("duplicate_piece")
		}
		want := min(meta.Info.PieceLength, total-int64(p.Index)*meta.Info.PieceLength)
		bytesUsed += len(p.Data)
		if int64(len(p.Data)) != want || bytesUsed > payloadProofBytes {
			return bad("piece_bounds")
		}
		digest := sha1.Sum(p.Data)
		if !bytes.Equal(digest[:], meta.Info.Pieces[p.Index*20:(p.Index+1)*20]) {
			return bad("piece_hash")
		}
		pieces[p.Index] = p.Data
		strong := sha256.Sum256(p.Data)
		out.PieceSHA256[p.Index] = hex.EncodeToString(strong[:])
	}
	readRange := func(offset, length int64) ([]byte, error) {
		if offset < 0 || length < 0 || offset > total || length > total-offset {
			return nil, errPayloadEvidence
		}
		result := make([]byte, 0, int(length))
		for length > 0 {
			i := int(offset / meta.Info.PieceLength)
			within := offset % meta.Info.PieceLength
			p, ok := pieces[i]
			if !ok || within >= int64(len(p)) {
				return nil, errPayloadEvidence
			}
			n := min(length, int64(len(p))-within)
			result = append(result, p[within:within+n]...)
			offset += n
			length -= n
		}
		return result, nil
	}
	reports := map[int]payloadFileReport{}
	for _, r := range evidence.Files {
		if _, ok := reports[r.Index]; ok {
			return bad("duplicate_report")
		}
		reports[r.Index] = r
	}
	checked := 0
	probeFiles, err := projectedFilesForProbe(projected, rawFiles)
	if err != nil {
		return bad("source_layout")
	}
	for _, f := range probeFiles {
		format, stream := mediaProbeFormat(f.Path, kind)
		if format == "" {
			continue
		}
		r, ok := reports[f.Index]
		if !ok || r.Format != format || r.MediaType != stream || r.DecodedFrames < 1 || r.DecodedFrames > 1000 {
			return bad("missing_decoded_media")
		}
		if stream == "video" && (r.Width < 16 || r.Width > 8192 || r.Height < 16 || r.Height > 8192) || stream == "audio" && (r.SampleRate < 8000 || r.SampleRate > 384000) {
			return bad("invalid_decoded_media")
		}
		firstLen := min(payloadProbeBytes, f.Size)
		tailStart := max(firstLen, f.Size-payloadProbeBytes)
		prefix, e := readRange(f.Offset, firstLen)
		if e != nil {
			return bad("missing_prefix")
		}
		tail, e := readRange(f.Offset+tailStart, f.Size-tailStart)
		if e != nil {
			return bad("missing_tail")
		}
		if !mediaProbeMagic(format, prefix) {
			return bad("container_signature")
		}
		sample := sha256.New()
		_ = binary.Write(sample, binary.BigEndian, uint64(f.Size))
		_ = binary.Write(sample, binary.BigEndian, uint64(tailStart))
		sample.Write(prefix)
		sample.Write(tail)
		if hex.EncodeToString(sample.Sum(nil)) != r.SampleDigest {
			return bad("sample_binding")
		}
		out.Files = append(out.Files, r)
		checked++
	}
	if checked == 0 || checked != len(reports) {
		return bad("incomplete_media_coverage")
	}
	infoDigest := sha256.Sum256(evidence.Info)
	out.InfoSHA256 = hex.EncodeToString(infoDigest[:])
	out.Claim = evidence.Claim
	return out, nil
}

type payloadProbeFile struct {
	Index        int
	Path         string
	Size, Offset int64
}

func projectedFilesForProbe(t model.Torrent, rawFiles []mi.FileInfo) ([]payloadProbeFile, error) {
	if t.FilesStatus == model.FilesStatusSingle {
		for _, f := range rawFiles {
			if strings.Contains(f.Attr, "p") {
				continue
			}
			if f.Length == int64(t.Size) && (len(f.BestPath()) == 0 || strings.Join(f.BestPath(), "/") == t.Name) {
				return []payloadProbeFile{{Index: 0, Path: t.Name, Size: int64(t.Size), Offset: f.TorrentOffset}}, nil
			}
		}
		return nil, errPayloadEvidence
	}
	rawByPath := map[string]mi.FileInfo{}
	for _, f := range rawFiles {
		rawByPath[strings.Join(f.BestPath(), "/")] = f
	}
	result := make([]payloadProbeFile, 0, len(t.Files))
	for _, f := range t.Files {
		raw, ok := rawByPath[f.Path]
		if !ok || raw.Length != int64(f.Size) {
			return nil, errPayloadEvidence
		}
		result = append(result, payloadProbeFile{Index: int(f.Index), Path: f.Path, Size: int64(f.Size), Offset: raw.TorrentOffset})
	}
	return result, nil
}

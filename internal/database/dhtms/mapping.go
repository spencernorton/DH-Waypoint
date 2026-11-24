package dhtms

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
)

// ContentTypeToCode maps DH-Waypoint content_type strings to DH-TMS u8 codes.
func ContentTypeToCode(ct string) uint8 {
	switch strings.ToLower(strings.TrimSpace(ct)) {
	case "movie":
		return 1
	case "tv_show":
		return 2
	case "music":
		return 3
	case "ebook":
		return 4
	case "comic":
		return 5
	case "audiobook":
		return 6
	case "game":
		return 7
	case "software":
		return 8
	case "xxx":
		return 9
	case "course":
		return 10
	default:
		return 0
	}
}

// CodeToContentType maps a DH-TMS u8 content_type code back to model.NullContentType.
func CodeToContentType(code uint8) model.NullContentType {
	switch code {
	case 1:
		return model.NewNullContentType(model.ContentTypeMovie)
	case 2:
		return model.NewNullContentType(model.ContentTypeTvShow)
	case 3:
		return model.NewNullContentType(model.ContentTypeMusic)
	case 4:
		return model.NewNullContentType(model.ContentTypeEbook)
	case 5:
		return model.NewNullContentType(model.ContentTypeComic)
	case 6:
		return model.NewNullContentType(model.ContentTypeAudiobook)
	case 7:
		return model.NewNullContentType(model.ContentTypeGame)
	case 8:
		return model.NewNullContentType(model.ContentTypeSoftware)
	case 9:
		return model.NewNullContentType(model.ContentTypeXxx)
	case 10:
		return model.NewNullContentType(model.ContentTypeCourse)
	default:
		return model.NullContentType{}
	}
}

// ResolutionToCode maps DH-Waypoint video_resolution strings to DH-TMS u8 codes.
func ResolutionToCode(res string) uint8 {
	switch strings.ToLower(strings.TrimSpace(res)) {
	case "v360p", "360p":
		return 1
	case "v480p", "480p":
		return 2
	case "v540p", "540p":
		return 3
	case "v576p", "576p":
		return 4
	case "v720p", "720p":
		return 5
	case "v1080p", "1080p":
		return 6
	case "v1440p", "1440p":
		return 7
	case "v2160p", "2160p", "4k", "uhd":
		return 8
	case "v4320p", "4320p", "8k":
		return 9
	default:
		return 0
	}
}

// CodeToResolution maps a DH-TMS u8 resolution code back to model.NullVideoResolution.
func CodeToResolution(code uint8) model.NullVideoResolution {
	switch code {
	case 1:
		return model.NewNullVideoResolution(model.VideoResolutionV360p)
	case 2:
		return model.NewNullVideoResolution(model.VideoResolutionV480p)
	case 3:
		return model.NewNullVideoResolution(model.VideoResolutionV540p)
	case 4:
		return model.NewNullVideoResolution(model.VideoResolutionV576p)
	case 5:
		return model.NewNullVideoResolution(model.VideoResolutionV720p)
	case 6:
		return model.NewNullVideoResolution(model.VideoResolutionV1080p)
	case 7:
		return model.NewNullVideoResolution(model.VideoResolutionV1440p)
	case 8:
		return model.NewNullVideoResolution(model.VideoResolutionV2160p)
	case 9:
		return model.NewNullVideoResolution(model.VideoResolutionV4320p)
	default:
		return model.NullVideoResolution{}
	}
}

// VideoSourceToCode maps DH-Waypoint video_source enum strings to DH-TMS u8 codes (1..=9).
func VideoSourceToCode(src string) uint8 {
	switch strings.ToUpper(strings.TrimSpace(src)) {
	case "CAM":
		return 1
	case "TELESYNC":
		return 2
	case "TELECINE":
		return 3
	case "WORKPRINT":
		return 4
	case "DVD":
		return 5
	case "TV":
		return 6
	case "WEBDL":
		return 7
	case "WEBRIP":
		return 8
	case "BLURAY":
		return 9
	default:
		return 0
	}
}

// CodeToVideoSource maps a DH-TMS u8 video_source code back to model.NullVideoSource.
func CodeToVideoSource(code uint8) model.NullVideoSource {
	switch code {
	case 1:
		return model.NewNullVideoSource(model.VideoSourceCAM)
	case 2:
		return model.NewNullVideoSource(model.VideoSourceTELESYNC)
	case 3:
		return model.NewNullVideoSource(model.VideoSourceTELECINE)
	case 4:
		return model.NewNullVideoSource(model.VideoSourceWORKPRINT)
	case 5:
		return model.NewNullVideoSource(model.VideoSourceDVD)
	case 6:
		return model.NewNullVideoSource(model.VideoSourceTV)
	case 7:
		return model.NewNullVideoSource(model.VideoSourceWEBDL)
	case 8:
		return model.NewNullVideoSource(model.VideoSourceWEBRip)
	case 9:
		return model.NewNullVideoSource(model.VideoSourceBluRay)
	default:
		return model.NullVideoSource{}
	}
}

// VideoCodecToCode maps DH-Waypoint video_codec enum strings to DH-TMS u8 codes (1..=9).
func VideoCodecToCode(codec string) uint8 {
	switch strings.ToUpper(strings.TrimSpace(codec)) {
	case "H264":
		return 1
	case "X264":
		return 2
	case "X265":
		return 3
	case "XVID":
		return 4
	case "DIVX":
		return 5
	case "MPEG2":
		return 6
	case "MPEG4":
		return 7
	case "HEVC":
		return 8
	case "AV1":
		return 9
	default:
		return 0
	}
}

// CodeToVideoCodec maps a DH-TMS u8 video_codec code back to model.NullVideoCodec.
func CodeToVideoCodec(code uint8) model.NullVideoCodec {
	switch code {
	case 1:
		return model.NewNullVideoCodec(model.VideoCodecH264)
	case 2:
		return model.NewNullVideoCodec(model.VideoCodecX264)
	case 3:
		return model.NewNullVideoCodec(model.VideoCodecX265)
	case 4:
		return model.NewNullVideoCodec(model.VideoCodecXviD)
	case 5:
		return model.NewNullVideoCodec(model.VideoCodecDivX)
	case 6:
		return model.NewNullVideoCodec(model.VideoCodecMPEG2)
	case 7:
		return model.NewNullVideoCodec(model.VideoCodecMPEG4)
	case 8:
		return model.NewNullVideoCodec(model.VideoCodecHEVC)
	case 9:
		return model.NewNullVideoCodec(model.VideoCodecAV1)
	default:
		return model.NullVideoCodec{}
	}
}

// Video3DToCode maps DH-Waypoint video_3d enum strings to DH-TMS 2-bit codes (1..=3).
func Video3DToCode(v3d string) uint8 {
	switch strings.ToUpper(strings.TrimSpace(v3d)) {
	case "V3D", "3D":
		return 1
	case "V3DSBS", "3DSBS":
		return 2
	case "V3DOU", "3DOU":
		return 3
	default:
		return 0
	}
}

// CodeToVideo3D maps a DH-TMS 2-bit video_3d code back to model.NullVideo3D.
func CodeToVideo3D(code uint8) model.NullVideo3D {
	switch code {
	case 1:
		return model.NewNullVideo3D(model.Video3DV3D)
	case 2:
		return model.NewNullVideo3D(model.Video3DV3DSBS)
	case 3:
		return model.NewNullVideo3D(model.Video3DV3DOU)
	default:
		return model.NullVideo3D{}
	}
}

// TierForPolicy maps DH-Waypoint's active serving.Policy to the matching DH-TMS PolicyTier.
func TierForPolicy(p *serving.Policy) PolicyTier {
	if p == nil {
		return TierBase
	}
	if p.RequiresPayloadEvidence() {
		return TierPayload
	}
	if p.RequiresPublicationReview() {
		return TierPub
	}
	return TierBase
}

// ParsePGLSN parses a PostgreSQL pg_lsn string ("X/Y") into a monotonic uint64 >= 1.
func ParsePGLSN(raw string) (uint64, error) {
	trimmed := strings.TrimSpace(raw)
	hiHex, loHex, ok := strings.Cut(trimmed, "/")
	if !ok {
		return 0, fmt.Errorf("invalid pg_lsn %q", raw)
	}
	hi, err := strconv.ParseUint(hiHex, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid pg_lsn high part %q: %w", raw, err)
	}
	lo, err := strconv.ParseUint(loHex, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid pg_lsn low part %q: %w", raw, err)
	}
	lsn := (hi << 32) | lo
	if lsn == 0 {
		lsn = 1
	}
	return lsn, nil
}

// GormExprToPG converts GORM '?' placeholders to pgx '$N' placeholders while
// preserving literal '?' characters inside single-quoted SQL strings.
func GormExprToPG(sqlStr string, vars []any, startIdx int) (string, []any, int) {
	var b strings.Builder
	b.Grow(len(sqlStr) + len(vars)*2)
	idx := startIdx
	inQuote := false
	for i := 0; i < len(sqlStr); i++ {
		ch := sqlStr[i]
		if ch == '\'' {
			b.WriteByte(ch)
			if inQuote && i+1 < len(sqlStr) && sqlStr[i+1] == '\'' {
				b.WriteByte(sqlStr[i+1])
				i++
			} else {
				inQuote = !inQuote
			}
			continue
		}
		if !inQuote && ch == '?' {
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(idx))
			idx++
			continue
		}
		b.WriteByte(ch)
	}
	return b.String(), vars, idx
}

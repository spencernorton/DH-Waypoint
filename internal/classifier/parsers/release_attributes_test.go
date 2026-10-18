package parsers

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/classifier/classification"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/stretchr/testify/require"
)

func TestReleaseAttributeParsingBindsOriginalNameAndPreservesHintAuthority(t *testing.T) {
	name := "Amber.Signal.2025.2160p.BDREMUX.HEVC.DV.HDR10.TrueHD.7.1.Atmos-GROUP.mkv"
	attrs, err := ParseVideoContentWithOptions(model.Torrent{Name: name}, classification.Result{}, ParseOptions{NoiseV2: true})
	require.NoError(t, err)
	require.Equal(t, model.NewNullString("Amber Signal"), attrs.BaseTitle)
	require.NotNil(t, attrs.ReleaseAttributes)
	digest := sha256.Sum256([]byte(name))
	require.Equal(t, hex.EncodeToString(digest[:]), attrs.ReleaseAttributes.SourceNameSHA256)
	cl := classification.ContentAttributes{ContentType: model.NewNullContentType(model.ContentTypeMovie), VideoCodec: model.NewNullVideoCodec(model.VideoCodecH264)}
	cl.Merge(attrs)
	require.Equal(t, model.VideoCodecH264, cl.VideoCodec.VideoCodec, "existing hinted codec remains authoritative")
	require.NotNil(t, cl.ReleaseAttributes)
	require.Equal(t, []string{"DOLBY_VISION", "HDR10"}, cl.ReleaseAttributes.HDRFormats)
}

// End to end: an AV1 + Opus release keeps its modern video codec and its
// Opus audio claim (format and channel layout) through the parser.
func TestReleaseAttributeParsingModernCodecsEndToEnd(t *testing.T) {
	name := "Amber.Signal.2025.2160p.WEB-DL.AV1.Opus.5.1-GROUP.mkv"
	attrs, err := ParseVideoContentWithOptions(model.Torrent{Name: name}, classification.Result{}, ParseOptions{NoiseV2: true})
	require.NoError(t, err)
	require.Equal(t, model.NewNullString("Amber Signal"), attrs.BaseTitle)
	require.Equal(t, model.VideoCodecAV1, attrs.VideoCodec.VideoCodec)
	require.Equal(t, model.VideoSourceWEBDL, attrs.VideoSource.VideoSource)
	require.NotNil(t, attrs.ReleaseAttributes)
	require.Equal(t, []string{"OPUS"}, attrs.ReleaseAttributes.AudioFormats)
	require.Equal(t, "5.1", *attrs.ReleaseAttributes.AudioChannels)
}

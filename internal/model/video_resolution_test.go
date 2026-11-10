package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// An exact height beats a loose word wherever each one sits; a loose word
// still counts when it is all there is.
func TestInferVideoResolution(t *testing.T) {
	t.Parallel()

	for input, want := range map[string]VideoResolution{
		"HD 1080p BluRay":       VideoResolutionV1080p,
		"UHD.BluRay.1080p.x264": VideoResolutionV1080p,
		"4K Remaster 720p":      VideoResolutionV720p,
		"(1080p) [ABCD1234]":    VideoResolutionV1080p,
		"WEB 1920x1080 AVC":     VideoResolutionV1080p,
		"2160p.UHD.BluRay":      VideoResolutionV2160p,
		"UHD BluRay x265":       VideoResolutionV2160p,
		"HD BluRay x264":        VideoResolutionV720p,
		"8K HDR":                VideoResolutionV4320p,
		"BluRay x264":           "",
	} {
		assert.Equal(t, want, InferVideoResolution(input).VideoResolution, input)
	}

	res, at := FindExactVideoResolution("Show.2160p.")
	assert.Equal(t, VideoResolutionV2160p, res.VideoResolution)
	assert.Equal(t, 5, at)

	res, at = FindExactVideoResolution("Show HD")
	assert.False(t, res.Valid)
	assert.Equal(t, -1, at)
}

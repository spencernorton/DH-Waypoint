package processors

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPayloadClaimRequiresTrustedIdentityAndEveryAvailableEpisode(t *testing.T) {
	now := time.Date(2031, 7, 10, 12, 0, 0, 0, time.UTC)
	src := source{Contents: []json.RawMessage{json.RawMessage(`{"content_type":"tv_show","content_source":"tmdb","content_id":"990003","episodes":{"1":{"1":{},"2":{}}}}`)}}
	valid := payloadClaim{Contract: payloadClaimContract, Source: "tmdb", ID: "990003", Type: "tv_show", CheckedAt: now, ProviderReceipts: []string{strings.Repeat("a", 64)}, EpisodeDates: map[int]map[int]string{1: {1: "2031-07-08", 2: "2031-07-09"}}}
	require.NoError(t, validatePayloadClaim(src, "tv_show", &valid, now))
	for _, change := range []func(*payloadClaim){
		func(c *payloadClaim) { c.ID = "990004" },
		func(c *payloadClaim) { c.Source = "caller" },
		func(c *payloadClaim) { c.CheckedAt = now.Add(-11 * time.Minute) },
		func(c *payloadClaim) { c.EpisodeDates = map[int]map[int]string{1: {1: "2031-07-08"}} },
		func(c *payloadClaim) { c.EpisodeDates = map[int]map[int]string{1: {1: "2031-07-08", 2: "2031-07-10"}} },
		func(c *payloadClaim) { c.EpisodeDates = map[int]map[int]string{1: {1: "2031-07-08", 2: "2031-07-11"}} },
	} {
		c := valid
		change(&c)
		require.ErrorIs(t, validatePayloadClaim(src, "tv_show", &c, now), errPayloadEvidence)
	}
	require.ErrorIs(t, validatePayloadClaim(src, "tv_show", nil, now), errPayloadEvidence)
	src.Contents[0] = json.RawMessage(`{"content_type":"tv_show","content_source":"tmdb","content_id":"990003","episodes":{"1":{}}}`)
	require.ErrorIs(t, validatePayloadClaim(src, "tv_show", &valid, now), errPayloadEvidence, "a partial episode receipt cannot approve a complete season")
	valid.CompleteSeasons = []int{1}
	valid.SeasonEpisodeCounts = map[int]int{1: 2}
	require.NoError(t, validatePayloadClaim(src, "tv_show", &valid, now))
	valid.SeasonEpisodeCounts[1] = 3
	require.ErrorIs(t, validatePayloadClaim(src, "tv_show", &valid, now), errPayloadEvidence)
}

func TestMovieClaimCannotUseTheatricalAvailabilityForDiscOrWebClaims(t *testing.T) {
	now := time.Date(2031, 7, 10, 12, 0, 0, 0, time.UTC)
	src := source{Contents: []json.RawMessage{json.RawMessage(`{"content_type":"movie","content_source":"tmdb","content_id":"990003","video_source":"WEBDL"}`)}}
	c := payloadClaim{Contract: payloadClaimContract, Source: "tmdb", ID: "990003", Type: "movie", CheckedAt: now, ProviderReceipts: []string{strings.Repeat("b", 64)}, ReleasedOn: "2031-07-08", ReleaseType: 3}
	require.ErrorIs(t, validatePayloadClaim(src, "movie", &c, now), errPayloadEvidence)
	c.ReleaseType = 4
	require.NoError(t, validatePayloadClaim(src, "movie", &c, now))
	src.Contents[0] = json.RawMessage(`{"content_type":"movie","content_source":"tmdb","content_id":"990003","video_source":"BluRay"}`)
	require.ErrorIs(t, validatePayloadClaim(src, "movie", &c, now), errPayloadEvidence)
	c.ReleaseType = 5
	require.NoError(t, validatePayloadClaim(src, "movie", &c, now))
	c.ReleasedOn = "2031-07-10"
	require.ErrorIs(t, validatePayloadClaim(src, "movie", &c, now), errPayloadEvidence)
}

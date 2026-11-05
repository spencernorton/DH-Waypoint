package processors

import (
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/model"
)

const payloadClaimContract = "tmdb-release-availability-v1"

type payloadClaimRequest struct {
	Source      string         `json:"source"`
	ID          string         `json:"id"`
	Type        string         `json:"type"`
	Episodes    model.Episodes `json:"episodes"`
	VideoSource string         `json:"video_source"`
}
type payloadClaim struct {
	Contract            string                 `json:"contract"`
	Source              string                 `json:"source"`
	ID                  string                 `json:"id"`
	Type                string                 `json:"type"`
	CheckedAt           time.Time              `json:"checked_at"`
	ReleasedOn          string                 `json:"released_on"`
	ReleaseType         int                    `json:"release_type"`
	EpisodeDates        map[int]map[int]string `json:"episode_dates"`
	CompleteSeasons     []int                  `json:"complete_seasons"`
	SeasonEpisodeCounts map[int]int            `json:"season_episode_counts"`
	SeriesComplete      bool                   `json:"series_complete"`
	ProviderReceipts    []string               `json:"provider_receipts"`
}

func sourceClaim(src source) (payloadClaimRequest, error) {
	var c struct {
		Source      string         `json:"content_source"`
		ID          string         `json:"content_id"`
		Type        string         `json:"content_type"`
		Episodes    model.Episodes `json:"episodes"`
		VideoSource string         `json:"video_source"`
	}
	if len(src.Contents) != 1 {
		return payloadClaimRequest{}, errPayloadEvidence
	}
	if err := json.Unmarshal(src.Contents[0], &c); err != nil {
		return payloadClaimRequest{}, err
	}
	if c.VideoSource == "" {
		if inferred := model.InferVideoSource(src.Name); inferred.Valid {
			c.VideoSource = string(inferred.VideoSource)
		}
	}
	return payloadClaimRequest{Source: c.Source, ID: c.ID, Type: c.Type, Episodes: c.Episodes, VideoSource: c.VideoSource}, nil
}

// Dates establish a trusted availability constraint, not the identity of the
// depicted content. Day-only dates are held until that UTC day has ended.
func validatePayloadClaim(src source, kind string, claim *payloadClaim, now time.Time) error {
	if kind != "movie" && kind != "tv_show" {
		return nil
	}
	expected, err := sourceClaim(src)
	if err != nil {
		return err
	}
	bad := errors.New("missing_or_stale_content_claim: invalid_payload_evidence")
	if claim == nil || claim.Contract != payloadClaimContract || claim.Source != "tmdb" || expected.Source != claim.Source || expected.ID == "" || expected.ID != claim.ID || kind != claim.Type || expected.Type != kind {
		return errors.Join(bad, errPayloadEvidence)
	}
	if claim.CheckedAt.IsZero() || claim.CheckedAt.After(now.Add(time.Minute)) || now.Sub(claim.CheckedAt) > 10*time.Minute || len(claim.ProviderReceipts) == 0 || len(claim.ProviderReceipts) > 32 {
		return errors.Join(bad, errPayloadEvidence)
	}
	for _, receipt := range claim.ProviderReceipts {
		if !jobIdentity.MatchString(receipt) {
			return errors.Join(bad, errPayloadEvidence)
		}
	}
	available := func(value string) bool {
		date, e := time.Parse("2006-01-02", value)
		return e == nil && date.Year() >= 1900 && !date.Add(24*time.Hour).After(now)
	}
	if kind == "movie" {
		if !available(claim.ReleasedOn) {
			return errors.Join(bad, errPayloadEvidence)
		}
		allowed := []int{1, 2, 3, 4, 5, 6}
		switch expected.VideoSource {
		case "WEBDL", "WEBRip":
			allowed = []int{4, 6}
		case "BluRay", "DVD":
			allowed = []int{5}
		case "TV":
			allowed = []int{6}
		case "CAM", "TELESYNC", "TELECINE", "WORKPRINT":
			allowed = []int{1, 2, 3}
		}
		if !slices.Contains(allowed, claim.ReleaseType) {
			return errors.Join(bad, errPayloadEvidence)
		}
		return nil
	}
	if len(claim.EpisodeDates) == 0 || len(claim.EpisodeDates) > 128 || len(claim.CompleteSeasons) > 128 {
		return errors.Join(bad, errPayloadEvidence)
	}
	wanted := expected.Episodes
	if len(wanted) == 0 {
		if !claim.SeriesComplete || len(claim.CompleteSeasons) != len(claim.EpisodeDates) {
			return errors.Join(bad, errPayloadEvidence)
		}
		wanted = model.Episodes{}
		for _, season := range claim.CompleteSeasons {
			wanted[season] = map[int]struct{}{}
		}
	}
	if len(wanted) > 128 {
		return errors.Join(bad, errPayloadEvidence)
	}
	for season, episodes := range wanted {
		known := claim.EpisodeDates[season]
		if season < 0 || season > 1000 || len(known) == 0 || len(known) > 5000 {
			return errors.Join(bad, errPayloadEvidence)
		}
		if len(episodes) == 0 {
			if !slices.Contains(claim.CompleteSeasons, season) || claim.SeasonEpisodeCounts[season] != len(known) {
				return errors.Join(bad, errPayloadEvidence)
			}
			for episode, date := range known {
				if episode <= 0 || !available(date) {
					return errors.Join(bad, errPayloadEvidence)
				}
			}
		} else {
			for episode := range episodes {
				if episode <= 0 || episode > 5000 || !available(known[episode]) {
					return errors.Join(bad, errPayloadEvidence)
				}
			}
		}
	}
	return nil
}

package adapter

import (
	"fmt"
	"strings"

	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/torznab"
)

func searchRequestToQueryOptions(r torznab.SearchRequest) ([]query.Option, error) {
	return searchRequestToQueryOptionsWithIdentifiers(r, true)
}

func searchRequestToQueryOptionsWithIdentifiers(r torznab.SearchRequest, includeIdentifiers bool) ([]query.Option, error) {
	var options []query.Option

	switch r.Type {
	case torznab.FunctionSearch:
	case torznab.FunctionMovie:
		options = append(options, query.Where(search.TorrentContentTypeCriteria(model.ContentTypeMovie)))
	case torznab.FunctionTV:
		options = append(options, query.Where(search.TorrentContentTypeCriteria(model.ContentTypeTvShow)))

		switch {
		case r.AirDate.IsValid():
			// daily-show date query — regular daily releases carry a date but
			// no season/episode markers, so the episodes criteria must NOT
			// also apply (it would exclude every date-only row).
			options = append(options, query.Where(search.TorrentContentReleaseDateCriteria(r.AirDate)))
		case r.Season.Valid:
			episodes := make(model.Episodes)
			if r.Episode.Valid {
				episodes = episodes.AddEpisode(r.Season.Int, r.Episode.Int)
			} else {
				episodes = episodes.AddSeason(r.Season.Int)
			}

			options = append(options, query.Where(search.TorrentContentEpisodesCriteria(episodes)))
		}
	case torznab.FunctionMusic:
		options = append(options, query.Where(search.TorrentContentTypeCriteria(model.ContentTypeMusic)))
	case torznab.FunctionBook:
		options = append(options, query.Where(search.TorrentContentTypeCriteria(
			model.ContentTypeEbook,
			model.ContentTypeComic,
			model.ContentTypeAudiobook,
		)))
	default:
		return nil, torznab.Error{
			Code:        202,
			Description: fmt.Sprintf("no such function (%s)", r.Type),
		}
	}

	if r.Query != "" {
		order := search.TorrentContentOrderByRelevance
		if r.Profile.DisableOrderByRelevance {
			order = search.TorrentContentOrderByPublishedAt
		}

		options = append(options,
			query.SearchString(r.Query),
			query.OrderBy(order.Clauses(search.OrderDirectionDescending)...))
	}

	var catsCriteria []query.Criteria

	for _, cat := range r.Cats {
		var catCriteria []query.Criteria

		switch {
		case torznab.CategoryMovies.Has(cat):
			if r.Type != torznab.FunctionMovie || torznab.CategoryMovies.ID == cat {
				catCriteria = append(
					catCriteria,
					search.TorrentContentTypeCriteria(model.ContentTypeMovie),
				)
			}

			switch cat {
			case torznab.CategoryMoviesSD.ID:
				catCriteria = append(
					catCriteria,
					search.VideoResolutionCriteria(model.VideoResolutionV480p),
				)
			case torznab.CategoryMoviesHD.ID:
				catCriteria = append(catCriteria, search.VideoResolutionCriteria(
					model.VideoResolutionV720p,
					model.VideoResolutionV1080p,
					model.VideoResolutionV1440p,
					model.VideoResolutionV2160p,
				))
			case torznab.CategoryMoviesUHD.ID:
				catCriteria = append(
					catCriteria,
					search.VideoResolutionCriteria(model.VideoResolutionV2160p),
				)
			case torznab.CategoryMovies3D.ID:
				catCriteria = append(catCriteria, search.Video3DCriteria(
					model.Video3DV3D,
					model.Video3DV3DSBS,
					model.Video3DV3DOU,
				))
			}
		case torznab.CategoryTV.Has(cat):
			if r.Type != torznab.FunctionTV || torznab.CategoryTV.ID == cat {
				catCriteria = append(
					catCriteria,
					search.TorrentContentTypeCriteria(model.ContentTypeTvShow),
				)
			}

			switch cat {
			case torznab.CategoryTVSD.ID:
				catCriteria = append(
					catCriteria,
					search.VideoResolutionCriteria(model.VideoResolutionV480p),
				)
			case torznab.CategoryTVHD.ID:
				catCriteria = append(catCriteria, search.VideoResolutionCriteria(
					model.VideoResolutionV720p,
					model.VideoResolutionV1080p,
					model.VideoResolutionV1440p,
					model.VideoResolutionV2160p,
				))
			case torznab.CategoryTVUHD.ID:
				catCriteria = append(
					catCriteria,
					search.VideoResolutionCriteria(model.VideoResolutionV2160p),
				)
			case torznab.CategoryTVAnime.ID:
				// Narrow a bare cat=5070 browse to anime (fansub-group named
				// TV). A cat=5070&q=<title> query already matches via the
				// per-result 5070 category the adapter emits on anime results.
				catCriteria = append(catCriteria, search.TorrentContentAnimeCriteria())
			}
		case torznab.CategoryXXX.Has(cat):
			catCriteria = append(catCriteria, search.TorrentContentTypeCriteria(model.ContentTypeXxx))
		case torznab.CategoryPC.Has(cat):
			catCriteria = append(catCriteria,
				search.TorrentContentTypeCriteria(model.ContentTypeSoftware, model.ContentTypeGame))
		case torznab.CategoryAudioAudiobook.Has(cat):
			catCriteria = append(catCriteria, search.TorrentContentTypeCriteria(model.ContentTypeAudiobook))
		case torznab.CategoryAudio.Has(cat):
			catCriteria = append(catCriteria, search.TorrentContentTypeCriteria(model.ContentTypeMusic))
		case torznab.CategoryBooksComics.Has(cat):
			catCriteria = append(catCriteria, search.TorrentContentTypeCriteria(model.ContentTypeComic))
		case torznab.CategoryBooks.Has(cat):
			catCriteria = append(catCriteria, search.TorrentContentTypeCriteria(
				model.ContentTypeEbook,
				model.ContentTypeComic,
				model.ContentTypeAudiobook,
			))
		}

		if len(catCriteria) > 0 {
			catsCriteria = append(catsCriteria, query.And(catCriteria...))
		}
	}

	if len(catsCriteria) > 0 {
		options = append(options, query.Where(query.Or(catsCriteria...)))
	}

	if includeIdentifiers {
		if identifiers := requestIdentifierRefs(r); len(identifiers) > 0 {
			options = append(options, query.Where(search.TorrentContentIdentifiersCriteria(identifiers...)))
		}
	}

	limit := r.Profile.DefaultLimit
	if r.Limit.Valid {
		limit = r.Limit.Uint
		if limit > r.Profile.MaxLimit {
			limit = r.Profile.MaxLimit
		}
	}

	options = append(options, query.Limit(limit))
	if r.Offset.Valid {
		options = append(options, query.Offset(r.Offset.Uint))
	}

	if len(r.Profile.Tags) > 0 {
		options = append(options, query.Where(search.TorrentTagCriteria(r.Profile.Tags...)))
	}

	return options, nil
}

func requestIdentifierRefs(r torznab.SearchRequest) []model.ContentRef {
	// supportedParams combines ID systems with OR, including unmapped IDs.
	var identifiers []model.ContentRef
	if r.IMDBID.Valid {
		id := r.IMDBID.String
		if !strings.HasPrefix(id, "tt") {
			id = "tt" + id
		}
		identifiers = append(identifiers, identifierRefs(r.Type, "imdb", id)...)
	}
	if r.TMDBID.Valid {
		identifiers = append(identifiers, identifierRefs(r.Type, "tmdb", r.TMDBID.String)...)
	}
	if r.TVDBID.Valid {
		identifiers = append(identifiers, identifierRefs(torznab.FunctionTV, "tvdb", r.TVDBID.String)...)
	}
	// A repeated (type, source, id) carries no extra OR alternative; keep the
	// ref list minimal so downstream singleton checks and bound parameter
	// counts reflect distinct identifiers only.
	seen := make(map[model.ContentRef]struct{}, len(identifiers))
	distinct := identifiers[:0]
	for _, ref := range identifiers {
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		distinct = append(distinct, ref)
	}
	return distinct
}

func identifierRefs(function, source, id string) []model.ContentRef {
	types := []model.ContentType{model.ContentTypeMovie, model.ContentTypeTvShow}
	switch function {
	case torznab.FunctionMovie:
		types = []model.ContentType{model.ContentTypeMovie}
	case torznab.FunctionTV:
		types = []model.ContentType{model.ContentTypeTvShow}
	}
	var refs []model.ContentRef
	for _, kind := range types {
		refs = append(refs, model.ContentRef{Type: kind, Source: source, ID: id})
	}
	return refs
}

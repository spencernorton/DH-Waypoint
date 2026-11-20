package search

import (
	"fmt"

	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/maps"
	"github.com/spencernorton/DH-Waypoint/internal/model"
)

// TorrentContentReleaseDateCriteria filters torrent_contents by the parsed
// per-release air date — the daily-show query path (Torznab
// season=YYYY&ep=MM/DD). The date is interpolated as an ISO literal rather
// than bound: DBCriteria.SQL is raw SQL where GORM treats `?` as a bind
// placeholder (same constraint as the episodes criteria), and the value is
// server-constructed from integers already validated by model.Date.IsValid,
// so no untrusted text can reach the literal.
func TorrentContentReleaseDateCriteria(date model.Date) query.Criteria {
	return query.DBCriteria{
		SQL: fmt.Sprintf("torrent_contents.release_date = '%s'", date.IsoDateString()),
	}
}

// TorrentContentReleaseYearCriteria filters movie/TV torrent_contents by
// release year: linked rows match content.release_year, while unlinked rows
// match either a parsed release_date within that calendar year or the four-digit
// year lexeme in their full-text vector.
func TorrentContentReleaseYearCriteria(year int) query.Criteria {
	return query.RawCriteria{
		Query: fmt.Sprintf(
			"(content.release_year = %d OR (torrent_contents.content_id IS NULL AND ((torrent_contents.release_date >= '%04d-01-01' AND torrent_contents.release_date <= '%04d-12-31') OR torrent_contents.tsv @@ to_tsquery('simple', '%d'))))",
			year, year, year, year,
		),
		Joins: maps.NewInsertMap(maps.MapEntry[string, struct{}]{Key: model.TableNameContent}),
	}
}

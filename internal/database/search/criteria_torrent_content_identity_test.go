package search

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// identityTestDBContext is the smallest query.DBContext that lets the resolved
// identifier criteria run its lookup against a sqlmock-backed GORM handle.
type identityTestDBContext struct{ q *dao.Query }

func (c identityTestDBContext) Query() *dao.Query { return c.q }
func (c identityTestDBContext) TableName() string { return model.TableNameTorrentContent }
func (c identityTestDBContext) NewSubQuery(ctx context.Context) query.SubQuery {
	return query.GenericSubQuery[dao.ITorrentContentDo]{SubQuery: c.q.TorrentContent.WithContext(ctx).ReadDB()}
}

func identityMockDB(t *testing.T) (*gorm.DB, sqlmock.Sqlmock) {
	t.Helper()
	sqlDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{})
	require.NoError(t, err)
	return db, mock
}

func anyArgs(n int) []driver.Value {
	args := make([]driver.Value, n)
	for i := range args {
		args[i] = sqlmock.AnyArg()
	}
	return args
}

// resolveCriteria runs the resolved-identifier criteria against mocked lookup
// rows and returns the frozen page criteria as a bound SQL string + args. The
// page predicate must be plain equalities on the canonical identity columns:
// it must not re-embed the identifier key subquery (VALUES ... UNION ALL over
// content_attributes), which is the shape that falls off the planner cliff on
// the 32M-row catalogue, especially once pgx prepared statements switch to a
// generic plan.
func resolveCriteria(t *testing.T, refs []model.ContentRef, keyRows *sqlmock.Rows, lookupArgCount int) query.RawCriteria {
	t.Helper()
	db, mock := identityMockDB(t)
	mock.ExpectQuery("identifier_keys").WithArgs(anyArgs(lookupArgCount)...).WillReturnRows(keyRows)
	raw, err := torrentContentResolvedIdentifiersCriteria(context.Background(), refs).
		Raw(identityTestDBContext{q: dao.Use(db)})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	return raw
}

func keyRows(rows ...[3]string) *sqlmock.Rows {
	out := sqlmock.NewRows([]string{"content_type", "content_source", "content_id"})
	for _, r := range rows {
		out.AddRow(r[0], r[1], r[2])
	}
	return out
}

func requireEqualityPageCriteria(t *testing.T, raw query.RawCriteria, wantArgs []interface{}) {
	t.Helper()
	sqlText, ok := raw.Query.(string)
	require.True(t, ok, "resolved page criteria must be a bound SQL string, got %T", raw.Query)
	for _, banned := range []string{"content_attributes", "UNION", "identifier_keys", " IN ("} {
		require.NotContains(t, sqlText, banned,
			"resolved page criteria must not re-embed the identifier key subquery:\n%s", sqlText)
	}
	require.Equal(t, wantArgs, raw.Args)
	require.Equal(t, len(wantArgs)/3, strings.Count(sqlText, "torrent_contents.content_type = ?"), sqlText)
}

// A tvsearch request carrying both tmdbid and tvdbid for the same show: the
// lookup materialises the requested tuples plus the alias target, DISTINCT
// collapses the duplicate canonical key, and the page query must become two
// equality arms on (content_type, content_source, content_id) — usable by
// torrent_contents_identity_published_page_idx under custom and generic plans
// alike. It must not keep the authoritative semijoin ANDed in alongside.
func TestResolvedDualIDSameShowUsesEqualityArmsOnly(t *testing.T) {
	refs := []model.ContentRef{
		{Type: model.ContentTypeTvShow, Source: "tmdb", ID: "119051"},
		{Type: model.ContentTypeTvShow, Source: "tvdb", ID: "83268"},
	}
	// Lookup order is unspecified; the page args must still be deterministic.
	raw := resolveCriteria(t, refs, keyRows(
		[3]string{"tv_show", "tvdb", "83268"},
		[3]string{"tv_show", "tmdb", "119051"},
	), 9)
	requireEqualityPageCriteria(t, raw, []interface{}{
		"tv_show", "tmdb", "119051",
		"tv_show", "tvdb", "83268",
	})
}

// Two distinct canonical identities (one external ID attributed to two
// contents) must keep complete OR semantics without falling back to the
// subquery semijoin: one equality arm per resolved key.
func TestResolvedDistinctCanonicalsKeepOrSemanticsWithoutSubquery(t *testing.T) {
	refs := []model.ContentRef{{Type: model.ContentTypeMovie, Source: "imdb", ID: "tt-one"}}
	raw := resolveCriteria(t, refs, keyRows(
		[3]string{"movie", "synthetic", "second"},
		[3]string{"movie", "imdb", "tt-one"},
		[3]string{"movie", "synthetic", "ordinary"},
	), 6)
	requireEqualityPageCriteria(t, raw, []interface{}{
		"movie", "imdb", "tt-one",
		"movie", "synthetic", "ordinary",
		"movie", "synthetic", "second",
	})
}

// A single resolved key is the plain identity equality — and nothing else.
// Re-adding the authoritative identifier predicate is redundant inside the
// shared repeatable-read snapshot (the lookup materialised exactly its key
// set) and reintroduces the planner cliff.
func TestResolvedSingleKeyIsIdentityEqualityOnly(t *testing.T) {
	refs := []model.ContentRef{{Type: model.ContentTypeTvShow, Source: "tvdb", ID: "83268"}}
	raw := resolveCriteria(t, refs, keyRows([3]string{"tv_show", "tmdb", "119051"}), 6)
	requireEqualityPageCriteria(t, raw, []interface{}{"tv_show", "tmdb", "119051"})
	sqlText := raw.Query.(string)
	require.NotContains(t, sqlText, " OR ")
}

// When an alias fans out to more distinct keys than the bounded page predicate
// allows, the criteria must fall back to the complete authoritative predicate
// rather than silently truncating the key set.
func TestResolvedOverflowFallsBackToAuthoritativePredicate(t *testing.T) {
	refs := []model.ContentRef{{Type: model.ContentTypeMovie, Source: "imdb", ID: "tt-many"}}
	many := keyRows()
	// One more distinct key than the bounded page predicate allows (32).
	for i := 0; i < 33; i++ {
		many.AddRow("movie", "synthetic", fmt.Sprintf("id-%02d", i))
	}
	raw := resolveCriteria(t, refs, many, 6)
	sqlText, ok := raw.Query.(string)
	require.True(t, ok, "fallback criteria must be a bound SQL string, got %T", raw.Query)
	require.Contains(t, sqlText, "content_attributes")
	require.Contains(t, sqlText, " IN (")
}

func TestTorrentContentIdentifiersCriteriaDeduplicatesRefs(t *testing.T) {
	tmdb := model.ContentRef{Type: model.ContentTypeTvShow, Source: "tmdb", ID: "119051"}

	// The same typed TMDB ref supplied twice is still the singleton fast path.
	direct, ok := TorrentContentIdentifiersCriteria(tmdb, tmdb).(query.DBCriteria)
	require.True(t, ok)
	require.Equal(t, TorrentContentIdentityCriteria(tmdb), query.Criteria(direct))

	tvdb := model.ContentRef{Type: model.ContentTypeTvShow, Source: "tvdb", ID: "83268"}
	guard, ok := TorrentContentIdentifiersCriteria(tmdb, tvdb, tvdb, tmdb).(query.DBCriteria)
	require.True(t, ok)
	// Two distinct requested tuples (6 args) plus one distinct alias branch
	// (3 args for tvdb); duplicates must not inflate the parameter list.
	require.Len(t, guard.Args, 9, guard.SQL)
	require.Contains(t, guard.SQL, "content_attributes")
}

// End-to-end on disposable PostgreSQL: two external IDs attributed to the
// same canonical content return its torrents once; when one of them is also
// attributed to a second content, both canonical identities are returned.
// This is the dual-ID request shape that previously kept the subquery
// semijoin in the page query.
func TestResolvedDualIDsSameAndDistinctCanonicalPostgres(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	ctx := context.Background()
	_, err := f.pool.Exec(ctx, `INSERT INTO content_attributes VALUES
 ('movie','synthetic','ordinary','imdb','id','tt-dual'),
 ('movie','synthetic','ordinary','tvdb','id','4242')`)
	require.NoError(t, err)
	s := f.serving(t, true)
	options := adultServingExactOptions(model.TableNameTorrentContent)
	dual := []model.ContentRef{
		{Type: model.ContentTypeMovie, Source: "imdb", ID: "tt-dual"},
		{Type: model.ContentTypeMovie, Source: "tvdb", ID: "4242"},
	}
	result, err := s.TorrentContentByIdentifiers(ctx, dual, options...)
	require.NoError(t, err)
	require.Equal(t, f.hashes[4:5], adultServingContentHashes(result.Items),
		"both IDs resolve to one canonical content: its torrents appear exactly once")
	require.EqualValues(t, 1, result.TotalCount)

	_, err = f.pool.Exec(ctx, `INSERT INTO content VALUES
 ('movie','synthetic','second','Synthetic second',false,now(),now())`)
	require.NoError(t, err)
	_, err = f.pool.Exec(ctx, `INSERT INTO torrent_contents(id,info_hash,content_type,content_source,content_id)
 VALUES ('second-content',$1,'movie','synthetic','second')`, f.hashes[5].Bytes())
	require.NoError(t, err)
	_, err = f.pool.Exec(ctx, `INSERT INTO content_attributes VALUES
 ('movie','synthetic','second','tvdb','id','4242')`)
	require.NoError(t, err)
	result, err = s.TorrentContentByIdentifiers(ctx, dual, options...)
	require.NoError(t, err)
	require.ElementsMatch(t, f.hashes[4:6], adultServingContentHashes(result.Items),
		"distinct resolved canonicals keep complete OR semantics")
	require.EqualValues(t, 2, result.TotalCount)
}

func TestTorrentContentIdentifiersCriteriaEmptyIsFalse(t *testing.T) {
	criteria, ok := TorrentContentIdentifiersCriteria().(query.DBCriteria)
	require.True(t, ok)
	require.Equal(t, "FALSE", criteria.SQL)
	require.Empty(t, criteria.Args)
}

package evidence

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencernorton/DH-Waypoint/internal/catalogueguard"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/stretchr/testify/require"
)

func privateEvidenceFixture(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("disposable PostgreSQL fixture not configured")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("evidence_private_test_%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, err)
	config, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
		admin.Close()
	})
	// The columns and index IsPrivateInfoHash reads, as created by migrations
	// 00021 (label_evidence) and 00070 (v2 identity).
	_, err = pool.Exec(ctx, `CREATE TABLE torrents(info_hash bytea PRIMARY KEY, info_hash_v2 bytea);
 CREATE INDEX torrents_info_hash_v2 ON torrents(info_hash_v2);
 CREATE TABLE label_evidence(id bigserial PRIMARY KEY, info_hash bytea, source text NOT NULL, category text);
 CREATE INDEX label_evidence_info_hash_idx ON label_evidence(info_hash) WHERE info_hash IS NOT NULL;`)
	require.NoError(t, err)
	return NewStore(lazy.New(func() (*pgxpool.Pool, error) { return pool, nil })), pool
}

func hash20(b byte) []byte { return []byte(strings.Repeat(string(rune(b)), 20)) }

func TestIsPrivateInfoHashMatchesIdentityAliasesAndQBPrivacy(t *testing.T) {
	s, pool := privateEvidenceFixture(t)
	ctx := context.Background()
	direct, v1, v2, public, ordinary := hash20('a'), hash20('b'), []byte(strings.Repeat("c", 32)), hash20('d'), hash20('e')
	_, err := pool.Exec(ctx, `INSERT INTO torrents(info_hash, info_hash_v2) VALUES ($1, NULL), ($2, $3), ($4, NULL), ($5, NULL)`,
		direct, v1, v2, public, ordinary)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO label_evidence(info_hash, source, category) VALUES
 ($1, ' QBittorrent ', 'Private'),
 ($2, 'qbittorrent', 'bitgrab'),
 ($3, 'qbittorrent', 'movies'),
 ($4, 'sonarr', 'private'),
 (NULL, 'qbittorrent', 'private')`, direct, v2, public, ordinary)
	require.NoError(t, err)

	for name, tc := range map[string]struct {
		hash []byte
		want bool
	}{
		"direct qB private evidence":             {direct, true},
		"v1 hash finds evidence on its v2 alias": {v1, true},
		"v2 hash finds its own evidence":         {v2, true},
		"qB evidence with a public category":     {public, false},
		"private category from a non-qB source":  {ordinary, false},
		"unknown hash has no evidence":           {hash20('z'), false},
	} {
		got, err := s.IsPrivateInfoHash(ctx, tc.hash)
		require.NoError(t, err, name)
		require.Equal(t, tc.want, got, name)
	}
}

func TestIsPrivateInfoHashProbesTheInfoHashIndex(t *testing.T) {
	_, pool := privateEvidenceFixture(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	// With sequential scans priced out, a plan that can probe the index uses
	// it; the former `= $1 OR IN (subquery)` form still had to scan the table.
	_, err = tx.Exec(ctx, `SET LOCAL enable_seqscan = off`)
	require.NoError(t, err)
	rows, err := tx.Query(ctx, `EXPLAIN `+isPrivateInfoHashSQL, hash20('a'), catalogueguard.TagWhitespace)
	require.NoError(t, err)
	var plan []string
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		plan = append(plan, line)
	}
	require.NoError(t, rows.Err())
	text := strings.Join(plan, "\n")
	require.NotContains(t, text, "Seq Scan on label_evidence", text)
	require.Contains(t, text, "label_evidence_info_hash_idx", text)
}

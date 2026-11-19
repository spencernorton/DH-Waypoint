package storagecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spencernorton/DH-Waypoint/internal/database/filebundles"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	migrationssql "github.com/spencernorton/DH-Waypoint/migrations"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"
)

func TestCompactFilesFlags(t *testing.T) {
	res, err := New(Params{Pool: lazy.New(func() (*pgxpool.Pool, error) { return nil, nil })})
	require.NoError(t, err)
	require.Equal(t, "storage", res.Command.Name)
	require.Len(t, res.Command.Subcommands, 1)
	sub := res.Command.Subcommands[0]
	require.Equal(t, "compact-files", sub.Name)
	flags := make(map[string]cli.Flag, len(sub.Flags))
	for _, f := range sub.Flags {
		for _, name := range f.Names() {
			flags[name] = f
		}
	}
	require.Contains(t, flags, "fast")
	require.Contains(t, flags, "progress-interval")
	pi, ok := flags["progress-interval"].(*cli.DurationFlag)
	require.True(t, ok)
	require.Equal(t, 5*time.Second, pi.Value)
}

func TestCompactFilesStatusFastAndStderrProgressPostgres(t *testing.T) {
	dsn := os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set DH_WAYPOINT_TEST_POSTGRES_DSN to a disposable PostgreSQL")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	_, err = admin.Exec(ctx, `SELECT pg_advisory_lock(830571);CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;CREATE EXTENSION IF NOT EXISTS btree_gin WITH SCHEMA public;SELECT pg_advisory_unlock(830571)`)
	require.NoError(t, err)
	schema := fmt.Sprintf("storagecmd_%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, err)
	require.NoError(t, admin.Close(ctx))
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	db := stdlib.OpenDB(*cfg.ConnConfig)
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrationssql.FS)
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = db.Close()
		pool.Close()
		if c, e := pgx.Connect(context.Background(), dsn); e == nil {
			_, _ = c.Exec(context.Background(), `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
			_ = c.Close(context.Background())
		}
	})

	for i := byte(1); i <= 4; i++ {
		h := make([]byte, 20)
		h[0] = 0xD1
		h[1] = i
		_, err = pool.Exec(ctx, `INSERT INTO torrents(info_hash,name,size,private,files_status,files_count,created_at,updated_at) VALUES($1,'CLI.Test',100,false,'multi',1,now(),now())`, h)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO torrent_files(info_hash,"index",path,size,created_at,updated_at) VALUES($1,0,'clip.mkv',100,now(),now())`, h)
		require.NoError(t, err)
	}
	_, err = pool.Exec(ctx, `ANALYZE torrent_files, torrent_file_bundles`)
	require.NoError(t, err)

	runCLI := func(args ...string) (string, string) {
		res, e := New(Params{Pool: lazy.New(func() (*pgxpool.Pool, error) { return pool, nil })})
		require.NoError(t, e)
		var out, errBuf bytes.Buffer
		app := &cli.App{
			Name:      "dh-waypoint",
			Writer:    &out,
			ErrWriter: &errBuf,
			Commands:  []*cli.Command{res.Command},
		}
		require.NoError(t, app.RunContext(ctx, append([]string{"dh-waypoint"}, args...)))
		return out.String(), errBuf.String()
	}

	stdout, stderr := runCLI("storage", "compact-files", "--status", "--fast")
	require.Empty(t, stderr)
	var st filebundles.StatusReport
	require.NoError(t, json.Unmarshal([]byte(stdout), &st))
	require.True(t, st.Estimated)
	require.EqualValues(t, -1, st.UnmigratedHashes)
	require.EqualValues(t, 4, st.LegacyRows)

	stdout, stderr = runCLI("storage", "compact-files", "--batch-size", "2", "--pause", "5ms", "--progress-interval", "1ms", "--prune-legacy")
	var rep filebundles.BackfillReport
	require.NoError(t, json.Unmarshal([]byte(stdout), &rep), "stdout must stay pure JSON report")
	require.True(t, rep.Done)
	require.EqualValues(t, 4, rep.MigratedHashes)
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	require.NotEmpty(t, lines, "stderr must receive NDJSON progress lines")
	for _, line := range lines {
		var p filebundles.BackfillProgress
		require.NoError(t, json.Unmarshal([]byte(line), &p))
	}
}

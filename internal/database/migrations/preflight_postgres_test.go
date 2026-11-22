package migrations_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	goose "github.com/pressly/goose/v3"
	"github.com/spencernorton/DH-Waypoint/internal/database/migrations"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	migrationssql "github.com/spencernorton/DH-Waypoint/migrations"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"
	"go.uber.org/zap"
	gormpg "gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestV6PreFlightStateMachineMatrixPostgres(t *testing.T) {
	ctx := context.Background()
	dsn := strings.TrimSpace(os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("disposable PostgreSQL not configured")
	}

	adminCfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	adminDB := stdlib.OpenDB(*adminCfg)
	t.Cleanup(func() { _ = adminDB.Close() })

	tempDBName := fmt.Sprintf("dhw_preflight_test_%d", time.Now().UnixNano())
	quotedDB := pgx.Identifier{tempDBName}.Sanitize()
	_, err = adminDB.ExecContext(ctx, "CREATE DATABASE "+quotedDB)
	require.NoError(t, err)

	testCfg := adminCfg.Copy()
	testCfg.Database = tempDBName
	db := stdlib.OpenDB(*testCfg)
	t.Cleanup(func() {
		_ = db.Close()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = adminDB.ExecContext(cleanupCtx, "DROP DATABASE IF EXISTS "+quotedDB+" WITH (FORCE)")
	})

	gdb, err := gorm.Open(gormpg.New(gormpg.Config{Conn: db}), &gorm.Config{})
	require.NoError(t, err)

	mRes := migrations.New(migrations.Params{
		DB:     lazy.New(func() (*gorm.DB, error) { return gdb, nil }),
		Logger: zap.NewNop().Sugar(),
	})
	m, err := mRes.Migrator.Get()
	require.NoError(t, err)

	assertExit78 := func(err error, msgSubstr string) {
		t.Helper()
		require.Error(t, err)
		var exitErr cli.ExitCoder
		require.True(t, errors.As(err, &exitErr), "expected ExitCoder, got %T: %v", err, err)
		require.Equal(t, migrations.ExitCodeIncompatibleSchema, exitErr.ExitCode())
		if msgSubstr != "" {
			require.Contains(t, err.Error(), msgSubstr)
		}
	}

	// Matrix 1: Goose table missing BUT public.torrents exists -> MUST NOT treat as empty DB!
	_, err = db.ExecContext(ctx, "CREATE TABLE public.torrents (info_hash bytea primary key)")
	require.NoError(t, err)
	_, err = m.UpWithConfirm(ctx, "")
	assertExit78(err, "public.torrents exists without public.goose_db_version")
	_, err = db.ExecContext(ctx, "DROP TABLE public.torrents")
	require.NoError(t, err)

	// Matrix 2: Old incompatible v5.8 database (Goose 70 < 81) -> unconditional exit 78 even with matching fingerprint.
	goose.SetBaseFS(migrationssql.FS)
	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.UpToContext(ctx, db, ".", 70))

	repOld, err := migrations.InspectV6Readiness(ctx, db)
	require.NoError(t, err)
	require.False(t, repOld.Ready)
	require.Equal(t, int64(70), repOld.GooseVersion)
	_, err = m.UpWithConfirm(ctx, repOld.Fingerprint)
	assertExit78(err, "below v6 direct-upgrade floor")

	var verAfter int64
	require.NoError(t, db.QueryRowContext(ctx, "SELECT MAX(version_id) FROM goose_db_version WHERE is_applied").Scan(&verAfter))
	require.Equal(t, int64(70), verAfter)

	// Matrix 3: Compatible v5.12 (Goose 81) is accepted as ready when acknowledged.
	require.NoError(t, goose.UpToContext(ctx, db, ".", 81))
	rep81, err := migrations.InspectV6Readiness(ctx, db)
	require.NoError(t, err)
	require.True(t, rep81.Ready, "blockers: %v", rep81.Blockers)

	// Matrix 4: Advance to v5.15.0 (Goose 83) and test inconsistent states (84, 99, 100 without schema_major, schema_major=6 at 83, schema_major=7).
	require.NoError(t, goose.UpToContext(ctx, db, ".", migrations.MaxV5SchemaVersion))

	for _, badVer := range []int64{84, 99, 100} {
		_, err = db.ExecContext(ctx, "INSERT INTO goose_db_version (version_id, is_applied) VALUES ($1, true)", badVer)
		require.NoError(t, err)
		_, err = m.UpWithConfirm(ctx, "")
		assertExit78(err, "")
		_, err = db.ExecContext(ctx, "DELETE FROM goose_db_version WHERE version_id = $1", badVer)
		require.NoError(t, err)
	}

	// schema_major=6 at Goose 83 -> inconsistent, exit 78!
	_, err = db.ExecContext(ctx, "INSERT INTO key_values (key, value, created_at, updated_at) VALUES ('schema_major', '6', now(), now())")
	require.NoError(t, err)
	_, err = m.UpWithConfirm(ctx, "")
	assertExit78(err, "inconsistent database state")

	// schema_major=7 -> newer major, exit 78!
	_, err = db.ExecContext(ctx, "UPDATE key_values SET value = '7' WHERE key = 'schema_major'")
	require.NoError(t, err)
	_, err = m.UpWithConfirm(ctx, "")
	assertExit78(err, "schema_major=7")
	_, err = db.ExecContext(ctx, "DELETE FROM key_values WHERE key = 'schema_major'")
	require.NoError(t, err)

	// Matrix 5: Non-superuser role fallback for fingerprint (pg_control_system denied -> goose genesis fallback).
	nonSuperRole := fmt.Sprintf("dhw_nonsuper_%d", time.Now().UnixNano())
	quotedRole := pgx.Identifier{nonSuperRole}.Sanitize()
	_, err = adminDB.ExecContext(ctx, fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD 'test-only-pw'", quotedRole))
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = adminDB.ExecContext(cleanupCtx, "DROP ROLE IF EXISTS "+quotedRole)
	})
	_, err = db.ExecContext(ctx, fmt.Sprintf("GRANT USAGE ON SCHEMA public TO %s; GRANT SELECT ON ALL TABLES IN SCHEMA public TO %s;", quotedRole, quotedRole))
	require.NoError(t, err)

	nsCfg := testCfg.Copy()
	nsCfg.User = nonSuperRole
	nsCfg.Password = "test-only-pw"
	nsDB := stdlib.OpenDB(*nsCfg)
	defer func() { _ = nsDB.Close() }()
	repNS, err := migrations.InspectV6Readiness(ctx, nsDB)
	require.NoError(t, err)
	require.True(t, repNS.Ready, "non-superuser blockers: %v", repNS.Blockers)
	require.NotEmpty(t, repNS.SystemIdentifier, "non-superuser must derive non-empty cluster/genesis identifier")
	require.Len(t, repNS.Fingerprint, 12)
	_ = nsDB.Close()
	_, _ = db.ExecContext(ctx, fmt.Sprintf("REVOKE ALL ON ALL TABLES IN SCHEMA public FROM %s; REVOKE USAGE ON SCHEMA public FROM %s;", quotedRole, quotedRole))

	// Matrix 6: Compatible v5.15 (Goose 83) — missing/wrong ack refused, concurrent ack serialized via advisory lock!
	repV5, err := migrations.InspectV6Readiness(ctx, db)
	require.NoError(t, err)
	require.True(t, repV5.Ready, "blockers: %v", repV5.Blockers)
	require.Contains(t, repV5.BackupCommand, "docker compose exec -T postgres pg_dump")

	t.Setenv(migrations.UpgradeConfirmEnvVar, "")
	assertExit78(m.Up(ctx), "requires backup acknowledgement")

	t.Setenv(migrations.UpgradeConfirmEnvVar, "deadbeef0000")
	assertExit78(m.Up(ctx), "requires backup acknowledgement")

	// Concurrent upgrade from two replicas: advisory lock serializes cleanly!
	var wg sync.WaitGroup
	errs := make([]error, 2)
	outcomes := make([]*migrations.PreFlightOutcome, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			outcomes[idx], errs[idx] = m.UpWithConfirm(ctx, repV5.Fingerprint)
		}(i)
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	cases := []string{outcomes[0].Case, outcomes[1].Case}
	require.ElementsMatch(t, []string{"v5_auto_upgrade", "existing_v6"}, cases)

	var v100Count int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM goose_db_version WHERE version_id = 100 AND is_applied").Scan(&v100Count))
	require.Equal(t, 1, v100Count)

	var majorVal, receiptVal string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT value FROM key_values WHERE key = $1", migrations.SchemaMajorKey).Scan(&majorVal))
	require.Equal(t, "6", majorVal)
	require.NoError(t, db.QueryRowContext(ctx, "SELECT value FROM key_values WHERE key = $1", migrations.UpgradeReceiptKey).Scan(&receiptVal))
	require.Contains(t, receiptVal, repV5.Fingerprint)
	require.Contains(t, receiptVal, `"pre_upgrade_goose_version":83`)

	// Subsequent boot without env var succeeds.
	t.Setenv(migrations.UpgradeConfirmEnvVar, "")
	outcome2, err := m.UpWithConfirm(ctx, "")
	require.NoError(t, err)
	require.Equal(t, "existing_v6", outcome2.Case)

	// Non-empty v6 down migration refuses destructive downgrade.
	_, err = db.ExecContext(ctx, `INSERT INTO torrents (info_hash, name, size, private, created_at, updated_at) VALUES (decode('a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1','hex'), 'fixture', 100, false, now(), now())`)
	require.NoError(t, err)
	err = m.Down(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "refusing destructive v6 -> v5")

	// Empty DB boots straight to v6 with zero flags.
	_, err = db.ExecContext(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public;")
	require.NoError(t, err)
	outcomeFresh, err := m.UpWithConfirm(ctx, "")
	require.NoError(t, err)
	require.Equal(t, "empty_db", outcomeFresh.Case)
	require.NoError(t, db.QueryRowContext(ctx, "SELECT value FROM key_values WHERE key = $1", migrations.SchemaMajorKey).Scan(&majorVal))
	require.Equal(t, "6", majorVal)
}

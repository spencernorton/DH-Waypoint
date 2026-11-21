package migrations_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/spencernorton/DH-Waypoint/internal/database/migrations"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"
	"go.uber.org/zap"
	gormpg "gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestV5DowngradeGuardAndV6ReadinessPostgres(t *testing.T) {
	ctx := context.Background()
	dsn := strings.TrimSpace(os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("disposable PostgreSQL not configured")
	}
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	gdb, err := gorm.Open(gormpg.New(gormpg.Config{Conn: db}), &gorm.Config{})
	require.NoError(t, err)

	// 1. Fresh/v5 migration up must succeed cleanly.
	mRes := migrations.New(migrations.Params{
		DB:     lazy.New(func() (*gorm.DB, error) { return gdb, nil }),
		Logger: zap.NewNop().Sugar(),
	})
	m, err := mRes.Migrator.Get()
	require.NoError(t, err)
	require.NoError(t, m.Up(ctx))

	// 2. InspectV6Readiness on schema 83 must report ready with stable fingerprint.
	rep1, err := migrations.InspectV6Readiness(ctx, db)
	require.NoError(t, err)
	require.True(t, rep1.Ready, "blockers: %v", rep1.Blockers)
	require.Equal(t, migrations.MaxV5SchemaVersion, rep1.GooseVersion)
	require.GreaterOrEqual(t, rep1.PostgresMajor, 16)
	require.Len(t, rep1.Fingerprint, 12)
	require.Equal(t, "UPGRADE_CONFIRM_V6_BACKUP="+rep1.Fingerprint, rep1.ConfirmEnvAssignment)

	rep2, err := migrations.InspectV6Readiness(ctx, db)
	require.NoError(t, err)
	require.Equal(t, rep1.Fingerprint, rep2.Fingerprint)

	// 3. Simulate a v6 database via key_values['schema_major']='6' and verify
	// v5.15.0 refuses to start with exit 78 BEFORE running Goose.
	_, err = db.ExecContext(ctx, "INSERT INTO public.key_values (key, value, created_at, updated_at) VALUES ($1, '6', now(), now()) ON CONFLICT (key) DO UPDATE SET value = '6'", migrations.SchemaMajorKey)
	require.NoError(t, err)
	defer func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM public.key_values WHERE key = $1", migrations.SchemaMajorKey)
	}()

	err = m.Up(ctx)
	require.Error(t, err)
	var exitErr cli.ExitCoder
	require.True(t, errors.As(err, &exitErr))
	require.Equal(t, migrations.ExitCodeIncompatibleSchema, exitErr.ExitCode())

	// 4. Remove schema_major and simulate Goose version 100 (v6 baseline) -> still exit 78.
	_, err = db.ExecContext(ctx, "DELETE FROM public.key_values WHERE key = $1", migrations.SchemaMajorKey)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "INSERT INTO public.goose_db_version (version_id, is_applied) VALUES (100, true)")
	require.NoError(t, err)
	defer func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM public.goose_db_version WHERE version_id = 100")
	}()

	err = m.Up(ctx)
	require.Error(t, err)
	require.True(t, errors.As(err, &exitErr))
	require.Equal(t, migrations.ExitCodeIncompatibleSchema, exitErr.ExitCode())
}

package migrations_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
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

func TestV6PreFlightFiveCaseStateMachinePostgres(t *testing.T) {
	ctx := context.Background()
	dsn := strings.TrimSpace(os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("disposable PostgreSQL not configured")
	}
	// Run in a disposable database so public schema state never leaks into
	// sibling packages sharing DH_WAYPOINT_TEST_POSTGRES_DSN.
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

	// Case 4: Simulate an old incompatible v5.8 database (Goose 70 < 81).
	goose.SetBaseFS(migrationssql.FS)
	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.UpToContext(ctx, db, ".", 70))

	repOld, err := migrations.InspectV6Readiness(ctx, db)
	require.NoError(t, err)
	require.False(t, repOld.Ready)
	require.Equal(t, int64(70), repOld.GooseVersion)

	// Unconditional hard stop EVEN WHEN matching fingerprint is passed!
	_, err = m.UpWithConfirm(ctx, repOld.Fingerprint)
	require.Error(t, err)
	var exitErr cli.ExitCoder
	require.True(t, errors.As(err, &exitErr))
	require.Equal(t, migrations.ExitCodeIncompatibleSchema, exitErr.ExitCode())
	// Prove ZERO writes occurred (still at Goose 70).
	var verAfter int64
	require.NoError(t, db.QueryRowContext(ctx, "SELECT MAX(version_id) FROM goose_db_version WHERE is_applied").Scan(&verAfter))
	require.Equal(t, int64(70), verAfter)

	// Case 3: Advance to compatible v5.15.0 schema (Goose 83).
	require.NoError(t, goose.UpToContext(ctx, db, ".", migrations.MaxV5SchemaVersion))
	repV5, err := migrations.InspectV6Readiness(ctx, db)
	require.NoError(t, err)
	require.True(t, repV5.Ready, "blockers: %v", repV5.Blockers)
	require.Equal(t, migrations.MaxV5SchemaVersion, repV5.GooseVersion)

	// 3a. Missing backup ack -> exit 78, zero writes!
	t.Setenv(migrations.UpgradeConfirmEnvVar, "")
	err = m.Up(ctx)
	require.Error(t, err)
	require.True(t, errors.As(err, &exitErr))
	require.Equal(t, migrations.ExitCodeIncompatibleSchema, exitErr.ExitCode())
	require.NoError(t, db.QueryRowContext(ctx, "SELECT MAX(version_id) FROM goose_db_version WHERE is_applied").Scan(&verAfter))
	require.Equal(t, migrations.MaxV5SchemaVersion, verAfter)

	// 3b. Wrong fingerprint ack -> exit 78, zero writes!
	t.Setenv(migrations.UpgradeConfirmEnvVar, "deadbeef0000")
	err = m.Up(ctx)
	require.Error(t, err)
	require.True(t, errors.As(err, &exitErr))
	require.Equal(t, migrations.ExitCodeIncompatibleSchema, exitErr.ExitCode())
	require.NoError(t, db.QueryRowContext(ctx, "SELECT MAX(version_id) FROM goose_db_version WHERE is_applied").Scan(&verAfter))
	require.Equal(t, migrations.MaxV5SchemaVersion, verAfter)

	// 3c. Matching fingerprint ack -> automatic in-binary upgrade!
	t.Setenv(migrations.UpgradeConfirmEnvVar, repV5.Fingerprint)
	outcome, err := m.UpWithConfirm(ctx, "")
	require.NoError(t, err)
	require.Equal(t, "v5_auto_upgrade", outcome.Case)
	require.NoError(t, db.QueryRowContext(ctx, "SELECT MAX(version_id) FROM goose_db_version WHERE is_applied").Scan(&verAfter))
	require.Equal(t, int64(100), verAfter)

	var majorVal, receiptVal string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT value FROM key_values WHERE key = $1", migrations.SchemaMajorKey).Scan(&majorVal))
	require.Equal(t, "6", majorVal)
	require.NoError(t, db.QueryRowContext(ctx, "SELECT value FROM key_values WHERE key = $1", migrations.UpgradeReceiptKey).Scan(&receiptVal))
	require.Contains(t, receiptVal, repV5.Fingerprint)

	// Case 2: Subsequent boot on v6 DB succeeds WITHOUT any env var!
	t.Setenv(migrations.UpgradeConfirmEnvVar, "")
	outcome2, err := m.UpWithConfirm(ctx, "")
	require.NoError(t, err)
	require.Equal(t, "existing_v6", outcome2.Case)

	// Down migration on empty torrents succeeds; on non-empty torrents refuses!
	_, err = db.ExecContext(ctx, `INSERT INTO torrents (info_hash, name, size, private, created_at, updated_at) VALUES (decode('a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1','hex'), 'fixture', 100, false, now(), now())`)
	require.NoError(t, err)
	err = m.Down(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "refusing destructive v6 -> v5")

	// Case 1: Fresh empty DB boots straight to v6 with ZERO env vars!
	_, err = db.ExecContext(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public;")
	require.NoError(t, err)
	outcomeFresh, err := m.UpWithConfirm(ctx, "")
	require.NoError(t, err)
	require.Equal(t, "empty_db", outcomeFresh.Case)
	require.NoError(t, db.QueryRowContext(ctx, "SELECT value FROM key_values WHERE key = $1", migrations.SchemaMajorKey).Scan(&majorVal))
	require.Equal(t, "6", majorVal)
}

package migrations

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	// ExitCodeIncompatibleSchema matches BSD sysexits EX_CONFIG (78) so
	// container orchestrators and scripts distinguish a deliberate major-version
	// hard stop from a transient runtime crash.
	ExitCodeIncompatibleSchema = 78

	// SchemaMajorKey stores the active major schema epoch in key_values.
	SchemaMajorKey = "schema_major"

	// MaxV5SchemaVersion is the highest Goose migration shipped by the v5.x line.
	MaxV5SchemaVersion int64 = 83

	// MinV6DirectUpgradeSchema is the lowest v5.x Goose migration supported for
	// zero-manual-step automatic in-binary upgrade into v6.0.0.
	MinV6DirectUpgradeSchema int64 = 81

	// UpgradeConfirmEnvVar is the environment variable v6.0.0 checks before
	// mutating a compatible v5.x database.
	UpgradeConfirmEnvVar = "UPGRADE_CONFIRM_V6_BACKUP"
)

// IncompatibleSchemaError signals a deliberate fail-closed stop when the binary
// cannot safely start against the target database schema epoch. It implements
// urfave/cli's ExitCoder interface via ExitCode().
type IncompatibleSchemaError struct {
	DetectedMajor        int
	DetectedGooseVersion int64
	Reason               string
	Guidance             string
}

func (e *IncompatibleSchemaError) Error() string {
	var b strings.Builder
	b.WriteString("INCOMPATIBLE DATABASE SCHEMA (exit 78): ")
	b.WriteString(e.Reason)
	if e.Guidance != "" {
		b.WriteString("\n")
		b.WriteString(e.Guidance)
	}
	return b.String()
}

// ExitCode implements cli.ExitCoder so the CLI exits cleanly with EX_CONFIG (78).
func (e *IncompatibleSchemaError) ExitCode() int {
	return ExitCodeIncompatibleSchema
}

// ComputeDBFingerprint derives a stable, instance-bound 12-hex-char token used
// to bind v6 backup acknowledgements to one exact PostgreSQL cluster, database,
// and schema version (remaining stable across live crawls and restarts).
func ComputeDBFingerprint(systemID, dbName string, gooseVersion int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("dh-waypoint-v6|%s|%s|%d", strings.TrimSpace(systemID), strings.TrimSpace(dbName), gooseVersion)))
	return hex.EncodeToString(sum[:6])
}

func tableExists(ctx context.Context, db *sql.DB, regclass string) (bool, error) {
	var name sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT to_regclass($1)", regclass).Scan(&name); err != nil {
		return false, err
	}
	return name.Valid && name.String != "", nil
}

func readSchemaMajor(ctx context.Context, db *sql.DB) (int, bool, error) {
	ok, err := tableExists(ctx, db, "public.key_values")
	if err != nil || !ok {
		return 0, false, err
	}
	var raw string
	err = db.QueryRowContext(ctx, "SELECT value FROM public.key_values WHERE key = $1", SchemaMajorKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	cleaned := strings.Trim(strings.TrimSpace(raw), `"`)
	major, parseErr := strconv.Atoi(cleaned)
	if parseErr != nil {
		return 0, true, fmt.Errorf("parse %s=%q: %w", SchemaMajorKey, raw, parseErr)
	}
	return major, true, nil
}

func readGooseVersion(ctx context.Context, db *sql.DB) (int64, bool, error) {
	ok, err := tableExists(ctx, db, "public.goose_db_version")
	if err != nil || !ok {
		return 0, false, err
	}
	var v int64
	if err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version_id), 0) FROM public.goose_db_version WHERE is_applied").Scan(&v); err != nil {
		return 0, false, err
	}
	return v, true, nil
}

// CheckV5Compatibility runs strictly read-only BEFORE Goose applies any DDL so
// a v5.x binary can never accidentally start against or corrupt a v6+ database.
func CheckV5Compatibility(ctx context.Context, db *sql.DB) error {
	major, hasMajor, err := readSchemaMajor(ctx, db)
	if err != nil {
		return &IncompatibleSchemaError{
			Reason:   fmt.Sprintf("invalid %s metadata: %v", SchemaMajorKey, err),
			Guidance: "Inspect key_values.schema_major before starting DH-Waypoint.",
		}
	}
	if hasMajor && major > 5 {
		return &IncompatibleSchemaError{
			DetectedMajor: major,
			Reason:        fmt.Sprintf("database is marked schema_major=%d, which cannot be opened by a v5.x binary", major),
			Guidance:      "Run a matching v6.x binary (use DH_TMS_READ_MODE=off on v6 for instant PG read fallback), or restore a pre-v6 PostgreSQL backup to revert to v5.x.",
		}
	}

	gooseVer, hasGoose, err := readGooseVersion(ctx, db)
	if err != nil {
		return err
	}
	if hasGoose && gooseVer > MaxV5SchemaVersion {
		return &IncompatibleSchemaError{
			DetectedMajor:        6,
			DetectedGooseVersion: gooseVer,
			Reason:               fmt.Sprintf("database Goose schema version %d exceeds v5.x maximum (%d)", gooseVer, MaxV5SchemaVersion),
			Guidance:             "This database has already been upgraded past v5.x. Start a matching v6.x binary or restore a verified pre-upgrade PostgreSQL backup.",
		}
	}
	return nil
}

// V6ReadinessReport summarizes a read-only pre-flight inspection for upgrading
// an existing v5.x installation to v6.0.0 without manual PostgreSQL migration.
type V6ReadinessReport struct {
	PostgresMajor              int
	PostgresVersion            string
	DatabaseName               string
	SystemIdentifier           string
	SchemaMajor                int
	HasSchemaMajor             bool
	GooseVersion               int64
	EstimatedTorrents          int64
	HasLegacyTorrentFiles      bool
	EstimatedLegacyFiles       int64
	ActiveBuiltinLLMRows       int
	LegacyContractRegistryRows int
	LegacyRootMountDetected    bool
	Fingerprint                string
	ConfirmEnvAssignment       string
	BackupCommand              string
	Blockers                   []string
	Advisories                 []string
	Ready                      bool
}

// InspectV6Readiness executes read-only PostgreSQL catalog probes to verify
// whether an instance is ready for automatic in-binary v6.0.0 upgrade.
func InspectV6Readiness(ctx context.Context, db *sql.DB) (*V6ReadinessReport, error) {
	rep := &V6ReadinessReport{}

	var verNum int
	if err := db.QueryRowContext(ctx, "SELECT current_database(), current_setting('server_version'), current_setting('server_version_num')::int").
		Scan(&rep.DatabaseName, &rep.PostgresVersion, &verNum); err != nil {
		return nil, fmt.Errorf("inspect postgres identity: %w", err)
	}
	rep.PostgresMajor = verNum / 10000

	// pg_control_system() is readable by default roles on PG 16+; fall back
	// gracefully if a restricted role lacks permission.
	var sysID sql.NullString
	_ = db.QueryRowContext(ctx, "SELECT system_identifier::text FROM pg_control_system()").Scan(&sysID)
	rep.SystemIdentifier = sysID.String

	major, hasMajor, err := readSchemaMajor(ctx, db)
	if err != nil {
		rep.Blockers = append(rep.Blockers, fmt.Sprintf("invalid key_values.%s entry: %v", SchemaMajorKey, err))
	}
	rep.SchemaMajor = major
	rep.HasSchemaMajor = hasMajor

	gooseVer, hasGoose, err := readGooseVersion(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("inspect goose_db_version: %w", err)
	}
	rep.GooseVersion = gooseVer

	if hasMajor && major >= 6 {
		rep.Advisories = append(rep.Advisories, fmt.Sprintf("database is already upgraded to schema_major=%d", major))
	} else if !hasGoose || gooseVer == 0 {
		rep.Advisories = append(rep.Advisories, "empty database detected; v6.0.0 will initialize automatically with zero manual steps")
	} else if gooseVer < MinV6DirectUpgradeSchema {
		rep.Blockers = append(rep.Blockers, fmt.Sprintf("Goose schema version %d is below v6 direct-upgrade floor (%d); run v5.15.0 once to advance schema to %d first", gooseVer, MinV6DirectUpgradeSchema, MaxV5SchemaVersion))
	}

	if rep.PostgresMajor < 16 {
		rep.Blockers = append(rep.Blockers, fmt.Sprintf("PostgreSQL %d is unsupported; v6.0.0 requires PostgreSQL 16 or 18 (either works directly with DH-TMS — no PG 16->18 dump/restore needed)", rep.PostgresMajor))
	}

	if ok, err := tableExists(ctx, db, "public.torrents"); err == nil && ok {
		var est float64
		if err := db.QueryRowContext(ctx, "SELECT COALESCE(reltuples, 0) FROM pg_class WHERE oid = to_regclass('public.torrents')").Scan(&est); err == nil && est > 0 {
			rep.EstimatedTorrents = int64(est)
		}
	}

	if ok, err := tableExists(ctx, db, "public.torrent_files"); err == nil && ok {
		var exists bool
		if err := db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM public.torrent_files LIMIT 1)").Scan(&exists); err == nil && exists {
			rep.HasLegacyTorrentFiles = true
			var est float64
			if err := db.QueryRowContext(ctx, "SELECT COALESCE(reltuples, 0) FROM pg_class WHERE oid = to_regclass('public.torrent_files')").Scan(&est); err == nil && est > 0 {
				rep.EstimatedLegacyFiles = int64(est)
			}
			rep.Advisories = append(rep.Advisories,
				fmt.Sprintf("legacy torrent_files rows remain (est. %d); optionally run `dh-waypoint storage compact-files --prune-legacy` online before v6 to shorten first-boot upgrade", rep.EstimatedLegacyFiles))
		}
	}

	if ok, err := tableExists(ctx, db, "public.processor_registry"); err == nil && ok {
		var hasDriver bool
		_ = db.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema='public' AND table_name='processor_registry' AND column_name='driver'
		)`).Scan(&hasDriver)
		if hasDriver {
			_ = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM public.processor_registry WHERE driver = 'builtin/llm' AND state <> 'paused'").Scan(&rep.ActiveBuiltinLLMRows)
			if rep.ActiveBuiltinLLMRows > 0 {
				rep.Blockers = append(rep.Blockers, fmt.Sprintf("%d active retired builtin/llm registration(s) found; pause or replace with adapters/llm over HTTP before upgrading", rep.ActiveBuiltinLLMRows))
			}
		}
		_ = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM public.processor_registry WHERE manifest->>'contract' = 'bitagent.processor.v1' AND state <> 'paused'").Scan(&rep.LegacyContractRegistryRows)
		if rep.LegacyContractRegistryRows > 0 {
			rep.Blockers = append(rep.Blockers, fmt.Sprintf("%d active legacy bitagent.processor.v1 registration(s) found; re-register under dh-waypoint.processor.v1 or pause before upgrading", rep.LegacyContractRegistryRows))
		}
	}

	for _, legacyPath := range []string{"/root/.config/bitmagnet", "/root/.local/share/bitmagnet"} {
		if _, statErr := os.Stat(legacyPath); statErr == nil {
			rep.LegacyRootMountDetected = true
			break
		}
	}
	if rep.LegacyRootMountDetected {
		rep.Advisories = append(rep.Advisories, "legacy /root/.*bitmagnet volume path detected; v6.0.0 runs non-root (UID 1000:1000) under /var/lib/dh-waypoint (check docs/migration-v6.md when updating Compose volumes)")
	}

	rep.Fingerprint = ComputeDBFingerprint(rep.SystemIdentifier, rep.DatabaseName, rep.GooseVersion)
	rep.ConfirmEnvAssignment = fmt.Sprintf("%s=%s", UpgradeConfirmEnvVar, rep.Fingerprint)
	rep.BackupCommand = fmt.Sprintf("pg_dump -Fc -d %s > dh-waypoint-pre-v6-$(date +%%F).dump", rep.DatabaseName)
	rep.Ready = len(rep.Blockers) == 0
	return rep, nil
}

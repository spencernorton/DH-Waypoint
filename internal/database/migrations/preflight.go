package migrations

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/adrg/xdg"
)

const (
	// ExitCodeIncompatibleSchema matches BSD sysexits EX_CONFIG (78) so
	// orchestrators and scripts distinguish a deliberate major-version hard
	// stop from a transient runtime failure.
	ExitCodeIncompatibleSchema = 78

	// SchemaMajorKey stores the active major schema epoch in key_values.
	SchemaMajorKey = "schema_major"

	// UpgradeReceiptKey stores the durable v6 upgrade receipt JSON in key_values.
	UpgradeReceiptKey = "v6_upgrade_receipt"

	// CurrentSchemaMajor is the major schema epoch enforced by v6.x binaries.
	CurrentSchemaMajor = 6

	// MinV6SchemaVersion is the first Goose migration belonging to the v6.x epoch.
	MinV6SchemaVersion int64 = 100

	// MaxV5SchemaVersion is the highest Goose migration shipped by the v5.x line.
	MaxV5SchemaVersion int64 = 83

	// MinV6DirectUpgradeSchema is the lowest v5.x Goose migration supported for
	// zero-manual-step automatic in-binary upgrade into v6.0.0.
	MinV6DirectUpgradeSchema int64 = 81

	// UpgradeConfirmEnvVar is the DB-fingerprint-bound acknowledgement v6.0.0
	// requires once before mutating a compatible v5.12-v5.15 database.
	UpgradeConfirmEnvVar = "UPGRADE_CONFIRM_V6_BACKUP"

	// migrationAdvisoryLockKey serializes concurrent replicas across pre-flight,
	// Goose migrations (including NO TRANSACTION CONCURRENTLY steps), and receipt
	// persistence. Derived from ASCII "DHW_MIG6".
	migrationAdvisoryLockKey int64 = 0x4448575f4d494736
)

// IncompatibleSchemaError signals a deliberate fail-closed stop when the binary
// cannot safely start against the target database schema epoch. It implements
// urfave/cli's ExitCoder interface via ExitCode().
type IncompatibleSchemaError struct {
	DetectedMajor        int
	DetectedGooseVersion int64
	Fingerprint          string
	Reason               string
	Guidance             string
}

func (e *IncompatibleSchemaError) Error() string {
	var b strings.Builder
	b.WriteString("INCOMPATIBLE DATABASE SCHEMA — HARD STOP (exit 78): ")
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

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
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

// PreFlightOutcome describes the read-only pre-flight classification before
// Goose executes any DDL statement.
type PreFlightOutcome struct {
	Case                  string
	NeedsUpgradeReceipt   bool
	PreUpgradeGooseVer    int64
	PreUpgradeFingerprint string
	Report                *V6ReadinessReport
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
	HasGooseTable              bool
	HasTorrentsTable           bool
	EstimatedTorrents          int64
	HasLegacyTorrentFiles      bool
	EstimatedLegacyFiles       int64
	ActiveBuiltinLLMRows       int
	LegacyContractRegistryRows int
	LegacyRootMountDetected    bool
	UnwritableStateDirs        []string
	Fingerprint                string
	ConfirmEnvAssignment       string
	BackupCommand              string
	Blockers                   []string
	Advisories                 []string
	Ready                      bool
}

// WriteReadinessDetails formats blockers, advisories, backup, and ack commands
// consistently across `doctor --check-v6` and `upgrade --check`.
func WriteReadinessDetails(w io.Writer, rep *V6ReadinessReport, indent string) {
	for _, b := range rep.Blockers {
		_, _ = fmt.Fprintf(w, "%sBLOCKER:  %s\n", indent, b)
	}
	for _, a := range rep.Advisories {
		_, _ = fmt.Fprintf(w, "%sADVISORY: %s\n", indent, a)
	}
	_, _ = fmt.Fprintf(w, "%sBACKUP:   %s\n", indent, rep.BackupCommand)
	_, _ = fmt.Fprintf(w, "%sCONFIRM:  %s\n", indent, rep.ConfirmEnvAssignment)
}

// InspectV6Readiness executes strictly read-only PostgreSQL catalog probes.
func InspectV6Readiness(ctx context.Context, db *sql.DB) (*V6ReadinessReport, error) {
	rep := &V6ReadinessReport{}

	var verNum int
	if err := db.QueryRowContext(ctx, "SELECT current_database(), current_setting('server_version'), current_setting('server_version_num')::int").
		Scan(&rep.DatabaseName, &rep.PostgresVersion, &verNum); err != nil {
		return nil, fmt.Errorf("inspect postgres identity: %w", err)
	}
	rep.PostgresMajor = verNum / 10000

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
	rep.HasGooseTable = hasGoose

	hasTorrents, err := tableExists(ctx, db, "public.torrents")
	if err != nil {
		return nil, fmt.Errorf("inspect torrents table: %w", err)
	}
	rep.HasTorrentsTable = hasTorrents

	// Resolve a cluster/instance-unique identifier for the backup fingerprint.
	// Standard installs succeed with pg_control_system() (matching v5.15.0's
	// doctor output); restricted non-superuser roles fall back deterministically
	// to the initial Goose migration timestamp rather than colliding on "".
	var sysID sql.NullString
	if sysErr := db.QueryRowContext(ctx, "SELECT system_identifier::text FROM pg_control_system()").Scan(&sysID); sysErr == nil && strings.TrimSpace(sysID.String) != "" {
		rep.SystemIdentifier = strings.TrimSpace(sysID.String)
	} else if hasGoose && gooseVer > 0 {
		var genesis sql.NullString
		if gErr := db.QueryRowContext(ctx,
			"SELECT to_char(MIN(tstamp) AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"') FROM public.goose_db_version WHERE version_id > 0",
		).Scan(&genesis); gErr == nil && strings.TrimSpace(genesis.String) != "" {
			rep.SystemIdentifier = "goose-genesis:" + strings.TrimSpace(genesis.String)
			rep.Advisories = append(rep.Advisories, "pg_control_system() unavailable to this PostgreSQL role; using deterministic goose_db_version genesis timestamp for backup fingerprint")
		} else {
			rep.Blockers = append(rep.Blockers, "unable to derive a cluster-unique backup fingerprint (pg_control_system() and goose_db_version timestamp probes both failed)")
		}
	}

	if rep.PostgresMajor < 16 {
		rep.Blockers = append(rep.Blockers, fmt.Sprintf("PostgreSQL %d is unsupported; v6.0.0 requires PostgreSQL 16 or 18 (either works directly — no manual PG 16->18 dump/restore needed)", rep.PostgresMajor))
	}

	// Single consistent state classification across schema_major, goose_db_version, and torrents.
	switch {
	case hasMajor && major > CurrentSchemaMajor:
		rep.Blockers = append(rep.Blockers, fmt.Sprintf("database is marked schema_major=%d, newer than this v6 binary", major))
	case hasMajor && major == CurrentSchemaMajor && gooseVer >= MinV6SchemaVersion:
		rep.Advisories = append(rep.Advisories, "database is already on v6 schema epoch (schema_major=6)")
	case hasMajor && major == CurrentSchemaMajor && gooseVer < MinV6SchemaVersion:
		rep.Blockers = append(rep.Blockers, fmt.Sprintf("inconsistent database state: key_values.schema_major=%d but Goose version is %d (< %d); restore a verified backup before starting", major, gooseVer, MinV6SchemaVersion))
	case hasMajor && major < CurrentSchemaMajor:
		rep.Blockers = append(rep.Blockers, fmt.Sprintf("unexpected key_values.schema_major=%d (expected unset for v5 or %d for v6); inspect key_values before upgrading", major, CurrentSchemaMajor))
	case !hasGoose && !hasTorrents:
		rep.Advisories = append(rep.Advisories, "empty database detected; v6.0.0 will initialize automatically with zero manual steps")
	case !hasGoose && hasTorrents:
		rep.Blockers = append(rep.Blockers, "inconsistent database state: public.torrents exists without public.goose_db_version; refusing to treat populated database as empty")
	case gooseVer == 0 && !hasTorrents:
		rep.Advisories = append(rep.Advisories, "empty database detected; v6.0.0 will initialize automatically with zero manual steps")
	case gooseVer == 0 && hasTorrents:
		rep.Blockers = append(rep.Blockers, "inconsistent database state: public.torrents exists at Goose version 0; refusing to overwrite existing catalogue")
	case gooseVer < MinV6DirectUpgradeSchema:
		rep.Blockers = append(rep.Blockers, fmt.Sprintf("Goose schema version %d is below v6 direct-upgrade floor (%d); back up and step through v5.15.0 (bridge LTS) first, or start fresh on v6.0.0 and stream NDJSON via POST /import", gooseVer, MinV6DirectUpgradeSchema))
	case gooseVer > MaxV5SchemaVersion && gooseVer < MinV6SchemaVersion:
		rep.Blockers = append(rep.Blockers, fmt.Sprintf("unexpected Goose schema version %d (between v5 max %d and v6 epoch %d); inspect goose_db_version before starting", gooseVer, MaxV5SchemaVersion, MinV6SchemaVersion))
	case gooseVer >= MinV6SchemaVersion:
		rep.Blockers = append(rep.Blockers, fmt.Sprintf("inconsistent database state: Goose version is %d (>= %d) without key_values.schema_major=%d; inspect key_values or restore a verified backup", gooseVer, MinV6SchemaVersion, CurrentSchemaMajor))
	}

	if hasTorrents {
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
				fmt.Sprintf("legacy torrent_files rows remain (est. %d); v6 reads them cleanly and `dh-waypoint storage compact-files --prune-legacy` compacts them online", rep.EstimatedLegacyFiles))
		}
	}

	if ok, err := tableExists(ctx, db, "public.processor_registry"); err == nil && ok {
		var hasDriver bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema='public' AND table_name='processor_registry' AND column_name='driver'
		)`).Scan(&hasDriver); err != nil {
			rep.Advisories = append(rep.Advisories, fmt.Sprintf("could not inspect processor_registry schema: %v", err))
		} else if hasDriver {
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM public.processor_registry WHERE driver = 'builtin/llm' AND state <> 'paused'").Scan(&rep.ActiveBuiltinLLMRows); err != nil {
				rep.Advisories = append(rep.Advisories, fmt.Sprintf("could not check retired builtin/llm registrations: %v", err))
			} else if rep.ActiveBuiltinLLMRows > 0 {
				rep.Advisories = append(rep.Advisories, fmt.Sprintf("%d active retired builtin/llm registration(s) found (already refused at runtime; replace with adapters/llm over HTTP)", rep.ActiveBuiltinLLMRows))
			}
		}
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM public.processor_registry WHERE manifest->>'contract' = 'bitagent.processor.v1' AND state <> 'paused'").Scan(&rep.LegacyContractRegistryRows); err != nil {
			rep.Advisories = append(rep.Advisories, fmt.Sprintf("could not check legacy processor_registry contracts: %v", err))
		} else if rep.LegacyContractRegistryRows > 0 {
			rep.Advisories = append(rep.Advisories, fmt.Sprintf("%d active legacy bitagent.processor.v1 registration(s) found (re-register under dh-waypoint.processor.v1 when convenient)", rep.LegacyContractRegistryRows))
		}
	}

	rep.LegacyRootMountDetected = detectLegacyRootMount()
	if rep.LegacyRootMountDetected {
		rep.Advisories = append(rep.Advisories, "legacy /root/.*bitmagnet volume mount detected; v6.0.0 runs non-root (UID 1000:1000) under /var/lib/dh-waypoint — update Compose volume targets per docs/migration-v6.md")
	}

	rep.UnwritableStateDirs = detectUnwritableStateDirs()
	if len(rep.UnwritableStateDirs) > 0 {
		rep.Advisories = append(rep.Advisories, fmt.Sprintf(
			"state directory not writable by UID %d (%s); if reusing root-owned v5 Docker volumes, fix ownership once: docker run --rm -u 0 -v dh-waypoint-config:/cfg -v dh-waypoint-data:/data alpine chown -R 1000:1000 /cfg /data",
			os.Getuid(), strings.Join(rep.UnwritableStateDirs, ", "),
		))
	}

	rep.Fingerprint = ComputeDBFingerprint(rep.SystemIdentifier, rep.DatabaseName, rep.GooseVersion)
	rep.ConfirmEnvAssignment = fmt.Sprintf("%s=%s", UpgradeConfirmEnvVar, rep.Fingerprint)
	rep.BackupCommand = fmt.Sprintf("docker compose exec -T postgres pg_dump -Fc -U bitmagnet -d %s > dh-waypoint-pre-v6-$(date +%%F).dump", shellSingleQuote(rep.DatabaseName))
	rep.Ready = len(rep.Blockers) == 0
	return rep, nil
}

// CheckV6PreFlight enforces the fail-closed major-version state machine
// strictly read-only BEFORE Goose executes a single DDL statement.
func CheckV6PreFlight(ctx context.Context, db *sql.DB, explicitConfirmToken string) (*PreFlightOutcome, error) {
	rep, err := InspectV6Readiness(ctx, db)
	if err != nil {
		return nil, err
	}

	// Any structural/version/inconsistency blocker fails closed BEFORE any success path.
	if len(rep.Blockers) > 0 {
		detectedMajor := rep.SchemaMajor
		if !rep.HasSchemaMajor && rep.GooseVersion > 0 && rep.GooseVersion <= MaxV5SchemaVersion {
			detectedMajor = 5
		}
		var lines []string
		lines = append(lines, "Resolve these blocking items before starting DH-Waypoint v6.0.0 (zero database writes were performed):")
		for _, b := range rep.Blockers {
			lines = append(lines, "  - "+b)
		}
		if rep.GooseVersion > 0 && rep.GooseVersion < MinV6DirectUpgradeSchema && !rep.HasSchemaMajor {
			lines = append(lines, "")
			lines = append(lines, "Safe paths for pre-v5.12 databases:")
			lines = append(lines, "  1. Back up PostgreSQL, upgrade to bridge release v5.15.0 (ghcr.io/spencernorton/dh-waypoint:v5.15.0),")
			lines = append(lines, "     verify `dh-waypoint doctor --check-v6`, then upgrade to v6.0.0 (automatic, zero SQL).")
			lines = append(lines, "  2. Start a fresh v6.0.0 stack and stream NDJSON via POST /import.")
		}
		lines = append(lines, "Reference: docs/migration-v6.md")
		return nil, &IncompatibleSchemaError{
			DetectedMajor:        detectedMajor,
			DetectedGooseVersion: rep.GooseVersion,
			Fingerprint:          rep.Fingerprint,
			Reason:               fmt.Sprintf("pre-flight check blocked startup (%d blocker(s); zero database writes were performed)", len(rep.Blockers)),
			Guidance:             strings.Join(lines, "\n"),
		}
	}

	// Case 2: Already initialized cleanly on v6 schema epoch (schema_major=6 AND Goose >= 100).
	if rep.HasSchemaMajor && rep.SchemaMajor == CurrentSchemaMajor && rep.GooseVersion >= MinV6SchemaVersion {
		return &PreFlightOutcome{Case: "existing_v6", Report: rep}, nil
	}

	// Case 1: Truly empty database (!HasGooseTable && !HasTorrentsTable, or Goose==0 && !HasTorrentsTable).
	if (!rep.HasGooseTable || rep.GooseVersion == 0) && !rep.HasTorrentsTable {
		return &PreFlightOutcome{Case: "empty_db", Report: rep}, nil
	}

	// Case 3: Compatible v5.12-v5.15 database (schema_major unset AND Goose in [81, 83]).
	if !rep.HasSchemaMajor && rep.GooseVersion >= MinV6DirectUpgradeSchema && rep.GooseVersion <= MaxV5SchemaVersion {
		token := strings.TrimSpace(explicitConfirmToken)
		if token == "" {
			token = strings.TrimSpace(os.Getenv(UpgradeConfirmEnvVar))
		}
		if token != rep.Fingerprint {
			var lines []string
			lines = append(lines, fmt.Sprintf("Detected existing v5.x database: postgres=%s (major=%d), goose=%d, est_torrents=%d, fingerprint=%s",
				rep.PostgresVersion, rep.PostgresMajor, rep.GooseVersion, rep.EstimatedTorrents, rep.Fingerprint))
			lines = append(lines, "DH-Waypoint v6.0.0 is a major release; downgrading back to v5.x requires restoring a PostgreSQL backup.")
			lines = append(lines, "NO manual PostgreSQL 16 -> 18 migration or SQL scripts are required — once you confirm a backup, v6 upgrades automatically.")
			for _, a := range rep.Advisories {
				lines = append(lines, "  NOTE: "+a)
			}
			lines = append(lines, "")
			lines = append(lines, "Step 1 — Create and verify a PostgreSQL backup:")
			lines = append(lines, "  "+rep.BackupCommand)
			lines = append(lines, "Step 2 — Acknowledge your backup once (either option):")
			lines = append(lines, "  Option A (Compose / env): set "+rep.ConfirmEnvAssignment+" in .env and restart")
			lines = append(lines, "  Option B (CLI):           docker compose run --rm dh-waypoint upgrade --confirm-backup="+rep.Fingerprint)
			lines = append(lines, "After upgrade completes, schema_major=6 is recorded and subsequent restarts never require this variable.")
			lines = append(lines, "Reference: docs/migration-v6.md")
			return nil, &IncompatibleSchemaError{
				DetectedMajor:        5,
				DetectedGooseVersion: rep.GooseVersion,
				Fingerprint:          rep.Fingerprint,
				Reason:               "existing v5.x database requires backup acknowledgement before automatic v6.0.0 upgrade (zero database writes were performed)",
				Guidance:             strings.Join(lines, "\n"),
			}
		}

		return &PreFlightOutcome{
			Case:                  "v5_auto_upgrade",
			NeedsUpgradeReceipt:   true,
			PreUpgradeGooseVer:    rep.GooseVersion,
			PreUpgradeFingerprint: rep.Fingerprint,
			Report:                rep,
		}, nil
	}

	// Defensive fail-closed catch-all for any unanticipated state.
	return nil, &IncompatibleSchemaError{
		DetectedMajor:        rep.SchemaMajor,
		DetectedGooseVersion: rep.GooseVersion,
		Fingerprint:          rep.Fingerprint,
		Reason:               fmt.Sprintf("unrecognized database schema state (schema_major=%d, set=%v, goose=%d)", rep.SchemaMajor, rep.HasSchemaMajor, rep.GooseVersion),
		Guidance:             "Inspect goose_db_version and key_values or restore a verified backup. Reference: docs/migration-v6.md",
	}
}

// RecordUpgradeReceipt stores the completed v5 -> v6 upgrade provenance in key_values.
func RecordUpgradeReceipt(ctx context.Context, db *sql.DB, outcome *PreFlightOutcome) error {
	if outcome == nil || !outcome.NeedsUpgradeReceipt {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"schema_major":              CurrentSchemaMajor,
		"upgraded_from_major":       5,
		"pre_upgrade_goose_version": outcome.PreUpgradeGooseVer,
		"db_fingerprint":            outcome.PreUpgradeFingerprint,
		"upgraded_at":               time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO public.key_values (key, value, created_at, updated_at)
		 VALUES ($1, $2, now(), now())
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		UpgradeReceiptKey, string(payload),
	)
	return err
}

func detectLegacyRootMount() bool {
	legacyPaths := []string{"/root/.config/bitmagnet", "/root/.local/share/bitmagnet"}
	for _, legacyPath := range legacyPaths {
		if _, err := os.Stat(legacyPath); err == nil {
			return true
		}
	}
	if raw, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		return mountInfoHasLegacyRootMount(string(raw))
	}
	return false
}

// mountInfoHasLegacyRootMount detects Docker bind/volume mounts under /root/
// even when running as non-root UID 1000 where /root (0700) returns EACCES.
func mountInfoHasLegacyRootMount(mountInfo string) bool {
	for _, line := range strings.Split(mountInfo, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mp := fields[4]
		if mp == "/root/.config/bitmagnet" || mp == "/root/.local/share/bitmagnet" {
			return true
		}
	}
	return false
}

func detectUnwritableStateDirs() []string {
	if os.Getuid() == 0 {
		return nil
	}
	var unwritable []string
	for _, dir := range []string{
		filepath.Join(xdg.ConfigHome, "bitmagnet"),
		filepath.Join(xdg.DataHome, "bitmagnet"),
	} {
		info, err := os.Stat(dir)
		if err != nil {
			if !os.IsNotExist(err) {
				unwritable = append(unwritable, dir)
			}
			continue
		}
		if !info.IsDir() {
			continue
		}
		f, err := os.CreateTemp(dir, ".dhw-writecheck-*")
		if err != nil {
			unwritable = append(unwritable, dir)
			continue
		}
		name := f.Name()
		_ = f.Close()
		_ = os.Remove(name)
	}
	return unwritable
}

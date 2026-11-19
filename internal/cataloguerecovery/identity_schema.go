package cataloguerecovery

import (
	"bytes"
	"encoding/json"
	"reflect"
)

// These are PostgreSQL's definitions of migration 70's two CHECKs. A different
// definition is deliberately a conflict. Never generally ignore new columns or
// constraints: recovery must prove the complete source schema before restoring.
const fullIdentityCheck = "CHECK (((octet_length(info_hash) = 20) OR ((octet_length(info_hash) = 32) AND (info_hash_v2 IS NOT NULL) AND (info_hash = info_hash_v2))))"
const v2IdentityCheck = "CHECK (((info_hash_v2 IS NULL) OR (octet_length(info_hash_v2) = 32)))"

// Exact schema definition of migration 81's additive torrent_file_bundles
// table on PostgreSQL 16 (3 constraints) and PostgreSQL 18 (which also lists
// explicit NOT NULL constraints in pg_constraint).
const bundleCardinalityCheck = "CHECK (((cardinality(indexes) = cardinality(paths)) AND (cardinality(paths) = cardinality(sizes)) AND (cardinality(sizes) = cardinality(extensions))))"
const bundleForeignKey = "FOREIGN KEY (info_hash) REFERENCES torrents(info_hash) ON DELETE CASCADE"
const bundlePrimaryKey = "PRIMARY KEY (info_hash)"

var bundleConstraintsPG16 = []string{
	bundleCardinalityCheck,
	bundleForeignKey,
	bundlePrimaryKey,
}

var bundleConstraintsPG18 = []string{
	bundleCardinalityCheck,
	bundleForeignKey,
	"NOT NULL created_at",
	"NOT NULL extensions",
	"NOT NULL indexes",
	"NOT NULL info_hash",
	"NOT NULL paths",
	"NOT NULL sizes",
	"NOT NULL updated_at",
	bundlePrimaryKey,
}

type snapshotColumn struct {
	Table       string   `json:"table"`
	Column      string   `json:"column"`
	Type        string   `json:"type"`
	NotNull     bool     `json:"notnull"`
	Generated   string   `json:"generated"`
	Identity    string   `json:"identity"`
	Default     *string  `json:"default"`
	Constraints []string `json:"constraints"`
}

func isMigration81BundleTable(cols []snapshotColumn) bool {
	clockDefault := "clock_timestamp()"
	want := []struct {
		col     string
		typ     string
		def     *string
	}{
		{"info_hash", "bytea", nil},
		{"indexes", "integer[]", nil},
		{"paths", "text[]", nil},
		{"sizes", "bigint[]", nil},
		{"extensions", "text[]", nil},
		{"created_at", "timestamp with time zone", &clockDefault},
		{"updated_at", "timestamp with time zone", &clockDefault},
	}
	if len(cols) != len(want) {
		return false
	}
	var expectedConstraints []string
	switch {
	case reflect.DeepEqual(cols[0].Constraints, bundleConstraintsPG16):
		expectedConstraints = bundleConstraintsPG16
	case reflect.DeepEqual(cols[0].Constraints, bundleConstraintsPG18):
		expectedConstraints = bundleConstraintsPG18
	default:
		return false
	}
	for i, w := range want {
		c := cols[i]
		if c.Table != "torrent_file_bundles" || c.Column != w.col || c.Type != w.typ || !c.NotNull || c.Generated != "" || c.Identity != "" {
			return false
		}
		if (c.Default == nil) != (w.def == nil) || (c.Default != nil && *c.Default != *w.def) {
			return false
		}
		if !reflect.DeepEqual(c.Constraints, expectedConstraints) {
			return false
		}
	}
	return true
}

func stripMigration81BundleTable(cols []snapshotColumn) ([]snapshotColumn, bool) {
	var rest, bundle []snapshotColumn
	for _, c := range cols {
		if c.Table == "torrent_file_bundles" {
			bundle = append(bundle, c)
		} else {
			rest = append(rest, c)
		}
	}
	if len(bundle) == 0 {
		return cols, true
	}
	if !isMigration81BundleTable(bundle) {
		return nil, false
	}
	return rest, true
}

func compatibleIdentitySchema(before, after []byte) bool {
	if bytes.Equal(before, after) {
		return true
	}
	var old, current []snapshotColumn
	decode := func(raw []byte, out any) error {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		return d.Decode(out)
	}
	if decode(before, &old) != nil || decode(after, &current) != nil {
		return false
	}
	oldBase, okOld := stripMigration81BundleTable(old)
	curBase, okCur := stripMigration81BundleTable(current)
	if !okOld || !okCur {
		return false
	}
	if reflect.DeepEqual(oldBase, curBase) {
		return true
	}
	if len(curBase) != len(oldBase)+1 {
		return false
	}
	for _, col := range oldBase {
		if col.Table == "torrents" && col.Column == "info_hash_v2" {
			return false
		}
	}
	var normalized []snapshotColumn
	alias := false
	for _, col := range curBase {
		if col.Table == "torrents" {
			var constraints []string
			full, v2 := false, false
			for _, c := range col.Constraints {
				switch c {
				case fullIdentityCheck:
					full = true
				case v2IdentityCheck:
					v2 = true
				default:
					constraints = append(constraints, c)
				}
			}
			if !full || !v2 {
				return false
			}
			col.Constraints = constraints
			if col.Column == "info_hash_v2" {
				if alias || col.Type != "bytea" || col.NotNull || col.Generated != "" || col.Identity != "" || col.Default != nil {
					return false
				}
				alias = true
				continue
			}
		}
		normalized = append(normalized, col)
	}
	return alias && reflect.DeepEqual(oldBase, normalized)
}

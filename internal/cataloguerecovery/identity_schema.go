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
	if decode(before, &old) != nil || decode(after, &current) != nil || len(current) != len(old)+1 {
		return false
	}
	for _, col := range old {
		if col.Table == "torrents" && col.Column == "info_hash_v2" {
			return false
		}
	}
	var normalized []snapshotColumn
	alias := false
	for _, col := range current {
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
	return alias && reflect.DeepEqual(old, normalized)
}

package csamblocklist

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
)

// SaveObservations records the double hash of each infohash in
// csam_local_observations, where Refresh picks it up. The infohash itself
// is never stored. Use Manager.Observe where a Manager is at hand: it also
// makes lookups reject the hashes at once.
func SaveObservations(ctx context.Context, pool *pgxpool.Pool, source string, infoHashes ...protocol.InfoHash) error {
	hashes := make([][]byte, len(infoHashes))
	sources := make([]string, len(infoHashes))
	for i, h := range infoHashes {
		dh := DoubleHash(h)
		hashes[i] = dh[:]
		sources[i] = source
	}
	return saveDoubleHashes(ctx, pool, hashes, sources)
}

func saveDoubleHashes(ctx context.Context, pool *pgxpool.Pool, hashes [][]byte, sources []string) error {
	if len(hashes) == 0 {
		return nil
	}
	if _, err := pool.Exec(ctx, `INSERT INTO csam_local_observations (double_hash, source)
SELECT * FROM unnest($1::bytea[], $2::text[]) ON CONFLICT (double_hash) DO NOTHING`, hashes, sources); err != nil {
		return fmt.Errorf("save csam local observations: %w", err)
	}
	return nil
}

func loadObservations(ctx context.Context, pool *pgxpool.Pool) ([][DoubleHashLen]byte, error) {
	rows, err := pool.Query(ctx, `SELECT double_hash FROM csam_local_observations`)
	if err != nil {
		return nil, fmt.Errorf("load csam local observations: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (dh [DoubleHashLen]byte, err error) {
		var b []byte
		err = row.Scan(&b)
		copy(dh[:], b)
		return dh, err
	})
	if err != nil {
		return nil, fmt.Errorf("load csam local observations: %w", err)
	}
	return out, nil
}

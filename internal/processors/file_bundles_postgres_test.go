package processors

import (
	"encoding/hex"
	"testing"

	llmwork "github.com/spencernorton/DH-Waypoint/internal/legacyresults"
	"github.com/stretchr/testify/require"
)

// The canonical source snapshot and SourceDigest must be byte-for-byte
// identical whether the same torrent's files live in torrent_files,
// torrent_file_bundles, or both, so existing approvals are never invalidated
// by file-storage migration.
func TestSourceSnapshotIdenticalAcrossFileStorageModes(t *testing.T) {
	_, pool, _ := fixture(t)
	ctx := testContext
	h := make([]byte, 20)
	h[0] = 0x77

	_, err := pool.Exec(ctx, `INSERT INTO torrents(info_hash,name,size,private,files_status,files_count,created_at,updated_at) VALUES($1,'Synthetic.Parity.2024',1015,false,'multi',3,now(),now());
INSERT INTO torrent_contents(info_hash,content_type,size,created_at,updated_at) VALUES($1,'movie',1015,now(),now())`, h)
	require.NoError(t, err)

	clear := func() {
		_, err := pool.Exec(ctx, `DELETE FROM torrent_files WHERE info_hash=$1; DELETE FROM torrent_file_bundles WHERE info_hash=$1`, h)
		require.NoError(t, err)
	}
	legacy := func() {
		_, err := pool.Exec(ctx, `INSERT INTO torrent_files(info_hash,"index",path,size,created_at,updated_at) VALUES($1,0,'Parity/film.mkv',1000,now(),now()),($1,1,'Parity/subs.srt',10,now(),now()),($1,2,'README',5,now(),now())`, h)
		require.NoError(t, err)
	}
	bundle := func() {
		_, err := pool.Exec(ctx, `INSERT INTO torrent_file_bundles(info_hash,indexes,paths,sizes,extensions) VALUES($1,ARRAY[0,1,2],ARRAY['Parity/film.mkv','Parity/subs.srt','README'],ARRAY[1000,10,5]::bigint[],ARRAY['mkv','srt',NULL]::text[])`, h)
		require.NoError(t, err)
	}
	snapshot := func(lock bool) (string, string) {
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		src, raw, err := readSource(ctx, tx, h, lock)
		require.NoError(t, err)
		require.Len(t, src.Files, 3)
		require.Nil(t, src.Files[2].Extension, "extension-free file keeps NULL extension")
		digest := llmwork.SourceDigest(src.torrent(hex.EncodeToString(h)))
		return string(raw), hex.EncodeToString(digest)
	}

	clear()
	legacy()
	wantRaw, wantDigest := snapshot(false)
	require.NotEmpty(t, wantRaw)

	clear()
	bundle()
	raw, digest := snapshot(true)
	require.Equal(t, wantRaw, raw, "bundle-only snapshot must equal the legacy snapshot byte-for-byte")
	require.Equal(t, wantDigest, digest, "bundle-only SourceDigest must equal the legacy digest")

	// Both representations present (dual write): the bundle wins and the
	// snapshot still does not change.
	legacy()
	raw, digest = snapshot(false)
	require.Equal(t, wantRaw, raw, "dual snapshot must equal the legacy snapshot byte-for-byte")
	require.Equal(t, wantDigest, digest, "dual SourceDigest must equal the legacy digest")
}

package serving

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// Go's regex parser constructs the prefilter, but PostgreSQL evaluates it.
// Exercise both expressions there, including optional text and Unicode edges.
func TestCandidatePostgresPreservesTheAuthoritativeMatch(t *testing.T) {
	dsn := os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set DH_WAYPOINT_TEST_POSTGRES_DSN to disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close(context.Background())) }()

	var names []string
	for _, word := range []string{"", "ordinary", "siterip", "SITERIP", "siteripReference", "privategold", "private.gold", "handjob", "handjobs", "gloryhole", "mygloryholes", "redstudio", "blue.studio", "abbbc", "café", "東京", "hot", "KIT", "kit", "İ"} {
		for _, prefix := range []string{"", "film.", "/", "π", "١"} {
			for _, suffix := range []string{"", ".mkv", "Ω", "١", "\n"} {
				names = append(names, prefix+word+suffix)
			}
		}
	}
	for i, group := range [][]string{
		{"siterip", "private ?gold", "handjobs?", "*gloryhole*", "(red|blue) ?studio", "ab+c", "kit"},
		{"siterip", "(hot)?"}, // An unconstrained alternative disables the prefilter.
		{"(hot)?"},
	} {
		t.Run(fmt.Sprintf("rules_%d", i), func(t *testing.T) {
			p, e := NewPolicy(Config{ExcludeAdult: true}, group, []string{"mkv"})
			require.NoError(t, e)
			coarse := p.candidatePattern
			if coarse == "" {
				coarse = ".*"
			}
			rows, e := db.Query(ctx, `SELECT value,
 COALESCE(value ~* $1, FALSE),
 COALESCE(CASE WHEN value ~* $2 THEN value ~* $1 ELSE FALSE END, FALSE)
 FROM (SELECT unnest($3::text[]) AS value UNION ALL SELECT NULL) samples`, p.strongPattern, coarse, names)
			require.NoError(t, e)
			defer rows.Close()
			count := 0
			for rows.Next() {
				var value *string
				var original, filtered bool
				require.NoError(t, rows.Scan(&value, &original, &filtered))
				require.Equal(t, original, filtered, "input: %v", value)
				count++
			}
			require.NoError(t, rows.Err())
			require.Equal(t, len(names)+1, count)
		})
	}
}

package classifier

import (
	"context"
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/classifier/classification"
	classifier_mocks "github.com/spencernorton/DH-Waypoint/internal/classifier/mocks"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/tmdb"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// With TMDB left on and no key, the default source turns the TMDB steps off
// and the core workflow still types a movie and a TV episode. This runs the
// real TMDB client, which makes no request without a key.
func TestEmptyTMDBKeyTurnsTMDBOff(t *testing.T) {
	t.Parallel()

	tmdbConfig := tmdb.NewDefaultConfig()
	require.True(t, tmdbConfig.Enabled)
	require.Empty(t, tmdbConfig.APIKey)

	source, err := newSourceProvider(NewDefaultConfig(), tmdbConfig).source()
	require.NoError(t, err)

	search := classifier_mocks.NewLocalSearch(t)
	search.On("ContentBySearch", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(model.Content{}, classification.ErrUnmatched)
	tmdbClient, err := tmdb.New(tmdb.Params{Config: tmdbConfig, Logger: zap.NewNop().Sugar()}).Client.Get()
	require.NoError(t, err)
	workflow, err := compiler{
		options:      []compilerOption{compilerFeatures(defaultFeatures), celEnvOption},
		dependencies: dependencies{search: search, tmdbClient: tmdbClient},
	}.Compile(source)
	require.NoError(t, err)

	for name, want := range map[string]model.ContentType{
		"The Sample Movie (2000).mkv":      model.ContentTypeMovie,
		"The Sample Show S01E02 1080p.mkv": model.ContentTypeTvShow,
	} {
		result, err := workflow.Run(context.Background(), "default", nil, model.Torrent{
			Name: name, FilesStatus: model.FilesStatusSingle, Extension: model.NewNullString("mkv"), Size: 1_000_000_000,
		})
		require.NoError(t, err, name)
		require.Equal(t, model.NewNullContentType(want), result.ContentType, name)
	}
	require.Equal(t, false, source.Flags["tmdb_enabled"])

	// A key leaves the TMDB steps on.
	tmdbConfig.APIKey = "set"
	source, err = newSourceProvider(NewDefaultConfig(), tmdbConfig).source()
	require.NoError(t, err)
	require.Equal(t, true, source.Flags["tmdb_enabled"])
}

func TestConfigSourceProviderDeleteContentTypesFlag(t *testing.T) {
	t.Parallel()

	cfg := NewDefaultConfig()
	cfg.DeleteContentTypes = []string{"music", "ebook"}

	src, err := configSourceProvider{config: cfg, tmdbEnabled: true}.source()
	require.NoError(t, err)
	require.Equal(t, []any{"music", "ebook"}, src.Flags["delete_content_types"])
}

func TestConfigSourceProviderDeleteContentTypesUnsetWhenEmpty(t *testing.T) {
	t.Parallel()

	src, err := configSourceProvider{config: NewDefaultConfig(), tmdbEnabled: true}.source()
	require.NoError(t, err)

	_, ok := src.Flags["delete_content_types"]
	require.False(t, ok)
}

func TestDeleteContentTypesFlagDecodesAsContentTypeList(t *testing.T) {
	t.Parallel()

	cfg := NewDefaultConfig()
	cfg.DeleteContentTypes = []string{"music", "ebook", "audiobook", "comic", "game", "software"}

	src, err := configSourceProvider{config: cfg, tmdbEnabled: true}.source()
	require.NoError(t, err)

	_, err = FlagTypeContentTypeList.celVal(src.Flags["delete_content_types"])
	require.NoError(t, err)
}

func TestDeleteContentTypesFlagRejectsInvalidTypeAtDecode(t *testing.T) {
	t.Parallel()

	cfg := NewDefaultConfig()
	cfg.DeleteContentTypes = []string{"musik"}

	src, err := configSourceProvider{config: cfg, tmdbEnabled: true}.source()
	require.NoError(t, err)

	_, err = FlagTypeContentTypeList.celVal(src.Flags["delete_content_types"])
	require.ErrorContains(t, err, "could not parse content type")
}

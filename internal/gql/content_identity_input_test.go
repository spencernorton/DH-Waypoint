package gql

import (
	"context"
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/stretchr/testify/require"
)

func TestContentIdentityInputBindsTheWholeNamespace(t *testing.T) {
	var ec executionContext
	input := map[string]any{"contentIdentity": map[string]any{
		"type": "tv_show", "source": "synthetic", "id": "123",
	}}
	got, err := ec.unmarshalInputTorrentContentSearchQueryInput(context.Background(), input)
	require.NoError(t, err)
	require.NotNil(t, got.ContentIdentity)
	require.Equal(t, model.ContentTypeTvShow, got.ContentIdentity.Type)
	require.Equal(t, "synthetic", got.ContentIdentity.Source)
	require.Equal(t, "123", got.ContentIdentity.ID)
}

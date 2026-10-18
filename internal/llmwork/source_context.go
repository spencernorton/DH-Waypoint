package llmwork

import (
	"context"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/namepolicy"
)

type sourceKey struct{}

func WithSourceTorrent(ctx context.Context, t model.Torrent) context.Context {
	return context.WithValue(namepolicy.WithSource(ctx, t.InfoHash, t.Name, ""), sourceKey{}, t)
}
func SourceTorrent(ctx context.Context) (model.Torrent, bool) {
	t, ok := ctx.Value(sourceKey{}).(model.Torrent)
	return t, ok
}

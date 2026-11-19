package search

import (
	"context"

	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"gorm.io/gen/field"
)

func HydrateTorrentContentContent() query.Option {
	return query.HydrateHasOne[TorrentContentResultItem, model.Content, model.ContentRef](
		torrentContentContentHydrator{},
	)
}

// HydrateTorrentContentContentAttributesOnly preloads Content and its external
// identifier Attributes without querying MetadataSource or Collections.
func HydrateTorrentContentContentAttributesOnly() query.Option {
	return query.HydrateHasOne[TorrentContentResultItem, model.Content, model.ContentRef](
		torrentContentContentHydrator{attributesOnly: true},
	)
}

type torrentContentContentHydrator struct {
	attributesOnly bool
}

func (torrentContentContentHydrator) RootToSubID(root TorrentContentResultItem) (model.ContentRef, bool) {
	ref := root.ContentRef()
	return ref.Val, ref.Valid
}

func (h torrentContentContentHydrator) GetSubs(
	ctx context.Context,
	dbCtx query.DBContext,
	ids []model.ContentRef,
) ([]model.Content, error) {
	opts := []query.Option{query.Where(ContentCanonicalIdentifierCriteria(ids...))}
	if h.attributesOnly {
		opts = append(opts, query.Preload(func(q *dao.Query) []field.RelationField {
			return []field.RelationField{q.Content.Attributes.RelationField}
		}))
	} else {
		opts = append(opts, ContentDefaultPreload(), ContentDefaultHydrate())
	}
	contentResult, contentErr := search{q: dbCtx.Query()}.Content(ctx, opts...)
	if contentErr != nil {
		return nil, contentErr
	}

	content := make([]model.Content, 0, len(contentResult.Items))
	for _, c := range contentResult.Items {
		content = append(content, c.Content)
	}

	return content, nil
}

func (torrentContentContentHydrator) SubID(item model.Content) model.ContentRef {
	return item.Ref()
}

func (torrentContentContentHydrator) Hydrate(root *TorrentContentResultItem, sub model.Content) {
	root.Content = sub
}

func (torrentContentContentHydrator) MustSucceed() bool {
	return true
}

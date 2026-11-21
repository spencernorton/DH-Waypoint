package search

import (
	"fmt"
	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/leanmode"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
	"go.uber.org/fx"
	"time"
)

type Search interface {
	ContentSearch
	QueueJobSearch
	TorrentSearch
	TorrentContentSearch
	TorrentFilesSearch
}

// ServingSearch is the mandatory consumer view; internal processing retains Search.
type ServingSearch interface{ Search }

type search struct {
	q               *dao.Query
	adultPolicy     *serving.Policy
	leanStore       *leanmode.Store
	unlinkedTimeout time.Duration
}

type Params struct {
	fx.In
	Query         lazy.Lazy[*dao.Query]
	ServingPolicy *serving.Policy
	LeanStore     *leanmode.Store `optional:"true"`
}

type Result struct {
	fx.Out
	Search        lazy.Lazy[Search]
	ServingSearch lazy.Lazy[ServingSearch]
}

func New(params Params) Result {
	return Result{
		ServingSearch: lazy.New(func() (ServingSearch, error) {
			if params.ServingPolicy == nil {
				return nil, fmt.Errorf("consumer serving policy is required")
			}
			q, err := params.Query.Get()
			if err != nil {
				return nil, err
			}
			return &search{q: q, adultPolicy: params.ServingPolicy, leanStore: params.LeanStore}, nil
		}),
		Search: lazy.New(func() (Search, error) {
			q, err := params.Query.Get()
			if err != nil {
				return nil, err
			}
			return &search{
				q: q,
			}, nil
		}),
	}
}

package httpserver

import (
	"github.com/gin-gonic/gin"
	"github.com/spencernorton/DH-Waypoint/internal/httpserver"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
	"github.com/spencernorton/DH-Waypoint/internal/torznab"
)

func New(lazyClient lazy.Lazy[torznab.Client], config torznab.Config, metrics *Metrics) httpserver.Option {
	return NewWithReview(lazyClient, config, metrics, serving.Config{})
}

// NewWithReview advertises the mandatory consumer policy on this exact feed.
func NewWithReview(lazyClient lazy.Lazy[torznab.Client], config torznab.Config, metrics *Metrics, review serving.Config) httpserver.Option {
	return builder{
		lazyClient:      lazyClient,
		config:          config,
		metrics:         metrics,
		reviewRequired:  review.RequiredProcessor != "",
		payloadRequired: review.RequiredPayloadVerifierArtifactDigest != "",
	}
}

type builder struct {
	lazyClient      lazy.Lazy[torznab.Client]
	config          torznab.Config
	metrics         *Metrics
	reviewRequired  bool
	payloadRequired bool
}

func (builder) Key() string {
	return "torznab"
}

func (b builder) Apply(e *gin.Engine) error {
	client, err := b.lazyClient.Get()
	if err != nil {
		return err
	}

	h := handler{
		config:  b.config,
		client:  client,
		metrics: b.metrics,
	}
	e.GET("/torznab/*any", func(c *gin.Context) {
		if b.reviewRequired {
			c.Header("X-Publication-Review", serving.PublicationReviewContract)
			c.Header("Cache-Control", "no-store")
		}
		if b.payloadRequired {
			c.Header("X-Publication-Payload", serving.PayloadEvidenceContract)
		}
		h.handleRequest(c)
	})

	return nil
}

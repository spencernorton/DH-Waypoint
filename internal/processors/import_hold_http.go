package processors

import (
	"context"
	"crypto/subtle"
	"time"

	"github.com/gin-gonic/gin"
)

// applyImportHoldHTTP gives an import guard permission to hold an exact
// existing public torrent. It exposes neither release nor admin operations.
func (s *Store) applyImportHoldHTTP(e *gin.Engine) {
	e.POST("/api/publication/v1/import-hold/:hash", func(c *gin.Context) {
		if s.cfg.ImportHoldToken == "" {
			c.Status(503)
			return
		}
		expected := "Bearer " + string(s.cfg.ImportHoldToken)
		if subtle.ConstantTimeCompare([]byte(c.GetHeader("Authorization")), []byte(expected)) != 1 {
			c.Status(401)
			return
		}
		c.Header("Cache-Control", "no-store")
		var request struct {
			RequestID string `json:"request_id"`
			Reason    string `json:"reason"`
		}
		if !publicationBody(c, &request, 1024) {
			return
		}
		if !jobIdentity.MatchString(request.RequestID) ||
			(request.Reason != "unsafe_media_extension" && request.Reason != "unsafe_file_attributes") {
			apiError(c, ErrControl)
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
		defer cancel()
		hash, err := s.publicationHash(ctx, c.Param("hash"))
		if err != nil {
			apiError(c, err)
			return
		}
		result, err := s.setPublicationHold(ctx, hash, publicationHoldWrite{
			RequestID: request.RequestID, Held: true, Reason: "import_" + request.Reason,
		})
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, result)
	})
}

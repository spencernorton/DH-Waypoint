package httpserver

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// QueryBudget bounds catalogue HTTP work without imposing a short deadline on
// the separate streaming import endpoint. Database calls inherit cancellation;
// socket deadlines also bound slow request bodies and response readers.
func QueryBudget(timeout time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		deadline, _ := ctx.Deadline()
		controller := http.NewResponseController(c.Writer)
		_ = controller.SetReadDeadline(minTime(deadline, time.Now().Add(10*time.Second)))
		_ = controller.SetWriteDeadline(deadline.Add(5 * time.Second))
		defer func() {
			_ = controller.SetReadDeadline(time.Time{})
			_ = controller.SetWriteDeadline(time.Time{})
		}()
		c.Next()
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

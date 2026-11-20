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
		// Keep the read deadline aligned with the request's QueryBudget: a
		// shorter read deadline triggers net/http's backgroundRead disconnect
		// probe early on bodyless GET requests and cancels c.Request.Context()
		// before the configured QueryBudget expires.
		_ = controller.SetReadDeadline(deadline)
		_ = controller.SetWriteDeadline(deadline.Add(5 * time.Second))
		// net/http still finalizes unread bodies and buffered responses after
		// the handler returns. Keep deadlines through that work; the server
		// resets them before reusing the connection for another request.
		c.Next()
	}
}

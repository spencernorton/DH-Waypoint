package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestQueryBudgetCancelsCooperativeWork(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.GET("/query", QueryBudget(20*time.Millisecond), func(c *gin.Context) {
		<-c.Request.Context().Done()
		c.Status(http.StatusGatewayTimeout)
	})
	rec := httptest.NewRecorder()
	start := time.Now()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/query", nil))
	require.Equal(t, http.StatusGatewayTimeout, rec.Code)
	require.Less(t, time.Since(start), time.Second)
}

func TestQueryBudgetPreservesEarlierCancellationAndCleansUp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	var observed context.Context
	e.GET("/query", QueryBudget(time.Minute), func(c *gin.Context) {
		observed = c.Request.Context()
		c.Status(http.StatusOK)
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/query", nil).WithContext(ctx)
	e.ServeHTTP(httptest.NewRecorder(), request)
	deadline, ok := observed.Deadline()
	require.True(t, ok)
	want, _ := ctx.Deadline()
	require.Equal(t, want, deadline)
	require.ErrorIs(t, observed.Err(), context.Canceled)
	require.NoError(t, ctx.Err())
}

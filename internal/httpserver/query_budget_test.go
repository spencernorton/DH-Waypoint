package httpserver

import (
	"bufio"
	"context"
	"io"
	"net"
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

func TestQueryBudgetBoundsUnreadBodyAfterHandlerReturns(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.POST("/query", QueryBudget(20*time.Millisecond), func(c *gin.Context) {
		c.String(http.StatusBadRequest, "invalid")
	})
	s := httptest.NewServer(e)
	defer s.Close()
	conn, err := net.Dial("tcp", s.Listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
	// The handler declines the request without consuming its promised body.
	// net/http still has to finalize the small buffered response and body.
	_, err = io.WriteString(conn, "POST /query HTTP/1.1\r\nHost: localhost\r\nContent-Length: 1\r\n\r\n")
	require.NoError(t, err)
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err, "handler return must not remove the deadline from HTTP finalization")
	defer response.Body.Close()
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
}

func TestQueryBudgetDoesNotPoisonNextStreamingRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.GET("/query", QueryBudget(20*time.Millisecond), func(c *gin.Context) { c.String(200, "ok") })
	e.POST("/stream", func(c *gin.Context) {
		raw, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		c.String(http.StatusOK, string(raw))
	})
	s := httptest.NewServer(e)
	defer s.Close()
	conn, err := net.Dial("tcp", s.Listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
	reader := bufio.NewReader(conn)
	_, err = io.WriteString(conn, "GET /query HTTP/1.1\r\nHost: localhost\r\n\r\n")
	require.NoError(t, err)
	first, err := http.ReadResponse(reader, nil)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, first.Body)
	require.NoError(t, err)
	require.NoError(t, first.Body.Close())
	_, err = io.WriteString(conn, "POST /stream HTTP/1.1\r\nHost: localhost\r\nContent-Length: 7\r\n\r\n")
	require.NoError(t, err)
	time.Sleep(60 * time.Millisecond) // Longer than the previous query budget.
	_, err = io.WriteString(conn, "payload")
	require.NoError(t, err)
	second, err := http.ReadResponse(reader, nil)
	require.NoError(t, err)
	defer second.Body.Close()
	require.Equal(t, http.StatusOK, second.StatusCode)
	body, err := io.ReadAll(second.Body)
	require.NoError(t, err)
	require.Equal(t, "payload", string(body))
}

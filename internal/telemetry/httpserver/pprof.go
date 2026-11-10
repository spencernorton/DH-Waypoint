package httpserver

import (
	"net/http/pprof"
	"runtime"

	"github.com/gin-gonic/gin"
	pyroscope "github.com/grafana/pyroscope-go/godeltaprof/http/pprof"
)

type pprofBuilder struct{}

func (pprofBuilder) Key() string {
	return "pprof"
}

func (pprofBuilder) Apply(e *gin.Engine) error {
	runtime.SetMutexProfileFraction(5)
	runtime.SetBlockProfileRate(5)
	e.GET("/debug/pprof/", func(c *gin.Context) {
		pprof.Index(c.Writer, c.Request)
	})
	e.GET("/debug/pprof/cmdline", func(c *gin.Context) {
		pprof.Cmdline(c.Writer, c.Request)
	})
	e.GET("/debug/pprof/profile", func(c *gin.Context) {
		pprof.Profile(c.Writer, c.Request)
	})
	e.GET("/debug/pprof/symbol", func(c *gin.Context) {
		pprof.Symbol(c.Writer, c.Request)
	})
	e.GET("/debug/pprof/trace", func(c *gin.Context) {
		pprof.Trace(c.Writer, c.Request)
	})
	e.GET("/debug/pprof/delta_heap", func(c *gin.Context) {
		pyroscope.Heap(c.Writer, c.Request)
	})
	e.GET("/debug/pprof/delta_block", func(c *gin.Context) {
		pyroscope.Block(c.Writer, c.Request)
	})
	e.GET("/debug/pprof/delta_mutex", func(c *gin.Context) {
		pyroscope.Mutex(c.Writer, c.Request)
	})
	e.GET("/debug/pprof/:profile", func(c *gin.Context) {
		pprof.Handler(c.Param("profile")).ServeHTTP(c.Writer, c.Request)
	})

	return nil
}

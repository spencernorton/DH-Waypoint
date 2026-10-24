package httpserver

import (
	"bytes"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/lru"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/gin-gonic/gin"
	"github.com/spencernorton/DH-Waypoint/internal/httpserver"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/vektah/gqlparser/v2/ast"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type Params struct {
	fx.In
	Schema lazy.Lazy[graphql.ExecutableSchema]
	Logger *zap.SugaredLogger
}

type Result struct {
	fx.Out
	Option httpserver.Option `group:"http_server_options"`
}

func New(p Params) Result {
	return Result{
		Option: &builder{
			schema: p.Schema,
		},
	}
}

type builder struct {
	schema lazy.Lazy[graphql.ExecutableSchema]
}

func (builder) Key() string {
	return "graphql"
}

func (b builder) Apply(e *gin.Engine) error {
	schema, err := b.schema.Get()
	if err != nil {
		return err
	}

	gql := newServer(schema)

	e.POST("/graphql", httpserver.QueryBudget(30*time.Second), func(c *gin.Context) {
		mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
		if err != nil || mediaType != "application/json" {
			c.AbortWithStatusJSON(http.StatusUnsupportedMediaType, gin.H{"error": "application_json_required"})
			return
		}
		if c.Request.ContentLength > maxBodyBytes {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request_body_too_large"})
			return
		}
		raw, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBodyBytes+1))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "request_body_unreadable"})
			return
		}
		if len(raw) > maxBodyBytes {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request_body_too_large"})
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(raw))
		gql.ServeHTTP(c.Writer, c.Request)
	})

	return nil
}

func newServer(es graphql.ExecutableSchema) *handler.Server {
	srv := handler.New(es)

	srv.AddTransport(transport.Options{})
	srv.AddTransport(transport.POST{})

	srv.SetParserTokenLimit(maxParserTokens)
	srv.SetQueryCache(lru.New[*ast.QueryDocument](100))

	srv.Use(extension.Introspection{})
	srv.Use(extension.AutomaticPersistedQuery{
		Cache: lru.New[string](100),
	})
	srv.Use(operationLimits{})

	return srv
}

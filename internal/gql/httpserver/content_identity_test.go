package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/99designs/gqlgen/graphql"
	"github.com/gin-gonic/gin"
	"github.com/spencernorton/DH-Waypoint/internal/gql"
	"github.com/spencernorton/DH-Waypoint/internal/gql/resolvers"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/stretchr/testify/require"
)

func TestContentIdentityHTTPValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	b := builder{schema: lazy.New(func() (graphql.ExecutableSchema, error) {
		return gql.NewExecutableSchema(gql.Config{Resolvers: &resolvers.Resolver{Search: codecClaimSearch{}}}), nil
	})}
	require.NoError(t, b.Apply(engine))
	for _, tc := range []struct {
		identity string
		valid    bool
	}{
		{`{type:tv_show,source:"synthetic",id:"123"}`, true},
		{`{source:"synthetic",id:"123"}`, false},
		{`{type:tv_show,id:"123"}`, false},
		{`{type:tv_show,source:"synthetic"}`, false},
		{`{type:tv_show,source:"",id:"123"}`, false},
		{`{type:tv_show,source:"synthetic",id:" "}`, false},
		{`{type:invalid,source:"synthetic",id:"123"}`, false},
	} {
		payload, err := json.Marshal(map[string]string{"query": `{torrentContent{search(input:{contentIdentity:` + tc.identity + `}){items{title}}}}`})
		require.NoError(t, err)
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(string(payload)))
		req.Header.Set("Content-Type", "application/json")
		engine.ServeHTTP(w, req)
		var response map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		if tc.valid {
			require.Empty(t, response["errors"], w.Body.String())
		} else {
			require.NotEmpty(t, response["errors"], tc.identity)
		}
	}
}

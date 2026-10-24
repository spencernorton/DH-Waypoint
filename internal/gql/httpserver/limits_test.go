package httpserver

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/99designs/gqlgen/graphql"
	"github.com/gin-gonic/gin"
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/gql"
	"github.com/spencernorton/DH-Waypoint/internal/gql/resolvers"
	corehttp "github.com/spencernorton/DH-Waypoint/internal/httpserver"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/stretchr/testify/require"
)

type boundedSearch struct {
	search.Search
	calls    atomic.Int32
	deadline atomic.Bool
}

func (s *boundedSearch) TorrentContent(ctx context.Context, _ ...query.Option) (search.TorrentContentResult, error) {
	s.calls.Add(1)
	_, hasDeadline := ctx.Deadline()
	s.deadline.Store(hasDeadline)
	return search.TorrentContentResult{}, nil
}

func limitsEngine(t *testing.T, s *boundedSearch) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	b := builder{schema: lazy.New(func() (graphql.ExecutableSchema, error) {
		return gql.NewExecutableSchema(gql.Config{Resolvers: &resolvers.Resolver{Search: s}}), nil
	})}
	require.NoError(t, b.Apply(e))
	return e
}

func graphRequest(t *testing.T, e *gin.Engine, query string, variables map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestGraphQLRejectsUnboundedInputBeforeSearch(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		vars        map[string]any
	}{
		{name: "limit literal", input: `{limit:1001}`},
		{name: "negative limit", input: `{limit:-1}`},
		{name: "limit variable", input: `$input`, vars: map[string]any{"input": map[string]any{"limit": 1001}}},
		{name: "query string", input: `$input`, vars: map[string]any{"input": map[string]any{"queryString": strings.Repeat("x", 4097)}}},
		{name: "unlimited count budget", input: `{aggregationBudget:0}`},
		{name: "oversized count budget", input: `{aggregationBudget:1e30}`},
		{name: "hash list", input: `$input`, vars: map[string]any{"input": map[string]any{"infoHashes": strings.Fields(strings.Repeat(strings.Repeat("0", 40)+" ", 1001))}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &boundedSearch{}
			e := limitsEngine(t, s)
			prefix := ""
			if tc.vars != nil {
				prefix = "query($input:TorrentContentSearchQueryInput!)"
			}
			r := graphRequest(t, e, prefix+`{torrentContent{search(input:`+tc.input+`){items{title}}}}`, tc.vars)
			require.Contains(t, r.Body.String(), `"OPERATION_LIMIT_EXCEEDED"`)
			require.Zero(t, s.calls.Load(), "rejected operations must never reach the database")
		})
	}
}

func TestGraphQLBoundsAliasAndFragmentWork(t *testing.T) {
	var aliases strings.Builder
	aliases.WriteString("{torrentContent{")
	for i := range 40 {
		fmt.Fprintf(&aliases, "s%d:search(input:{limit:0}){totalCount}", i)
	}
	aliases.WriteString("}}")
	fragment := `{torrentContent{...F0}} `
	for i := range 12 {
		fragment += fmt.Sprintf("fragment F%d on TorrentContentQuery{...F%d ...F%d} ", i, i+1, i+1)
	}
	fragment += "fragment F12 on TorrentContentQuery{search(input:{limit:0}){totalCount}}"
	for _, document := range []string{aliases.String(), fragment} {
		s := &boundedSearch{}
		r := graphRequest(t, limitsEngine(t, s), document, nil)
		require.Contains(t, r.Body.String(), `"OPERATION_LIMIT_EXCEEDED"`)
		require.Zero(t, s.calls.Load())
	}
}

func TestGraphQLBodyLimitIncludesUnknownLength(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			payload := `{"query":"{__typename}","variables":{"unused":"` + strings.Repeat("x", 1<<20) + `"}}`
			req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(payload))
			req.Header.Set("Content-Type", "application/json")
			if unknown {
				req.ContentLength = -1
			}
			r := httptest.NewRecorder()
			limitsEngine(t, &boundedSearch{}).ServeHTTP(r, req)
			require.Equal(t, http.StatusRequestEntityTooLarge, r.Code)
		})
	}
}

func TestGraphQLJSONAndAuthorizationBoundary(t *testing.T) {
	s := &boundedSearch{}
	e := limitsEngine(t, s)
	r := graphRequest(t, e, `{torrentContent{search(input:{limit:20}){items{title}}}}`, nil)
	require.NotContains(t, r.Body.String(), `"errors"`)
	require.EqualValues(t, 1, s.calls.Load())
	require.True(t, s.deadline.Load(), "database work inherits the request deadline")
	// Browser forms must not be a second transport for this JSON-only API.
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(""))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=test")
	r = httptest.NewRecorder()
	e.ServeHTTP(r, req)
	require.Equal(t, http.StatusUnsupportedMediaType, r.Code)

	gin.SetMode(gin.TestMode)
	secured := gin.New()
	secured.Use(corehttp.APIKeyAuth("synthetic-key"))
	b := builder{schema: lazy.New(func() (graphql.ExecutableSchema, error) {
		return gql.NewExecutableSchema(gql.Config{}), nil
	})}
	require.NoError(t, b.Apply(secured))
	r = graphRequest(t, secured, `mutation{__typename}`, nil)
	require.Equal(t, http.StatusUnauthorized, r.Code, "mutations must pass the configured HTTP key")
}

func TestGraphQLCachedOperationsRevalidateVariables(t *testing.T) {
	s := &boundedSearch{}
	e := limitsEngine(t, s)
	document := `query($input:TorrentContentSearchQueryInput!){torrentContent{search(input:$input){items{title}}}}`
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(document)))
	for _, limit := range []int{20, 1001} {
		payload := map[string]any{
			"query":      document,
			"variables":  map[string]any{"input": map[string]any{"limit": limit}},
			"extensions": map[string]any{"persistedQuery": map[string]any{"version": 1, "sha256Hash": hash}},
		}
		if limit == 1001 {
			delete(payload, "query")
		}
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(string(raw)))
		req.Header.Set("Content-Type", "application/json")
		r := httptest.NewRecorder()
		e.ServeHTTP(r, req)
		if limit == 20 {
			require.NotContains(t, r.Body.String(), `"errors"`)
		} else {
			require.Contains(t, r.Body.String(), `"OPERATION_LIMIT_EXCEEDED"`)
		}
	}
	require.EqualValues(t, 1, s.calls.Load())
}

func TestGraphQLRetainsBoundedPagesAndIntrospection(t *testing.T) {
	s := &boundedSearch{}
	e := limitsEngine(t, s)
	for _, document := range []string{
		`{torrentContent{search(input:{limit:1000,aggregationBudget:5000}){items{title}}}}`,
		`{torrentContent{search(input:{limit:0,totalCount:true}){totalCount totalCountIsEstimate}}}`,
		`{__schema{queryType{name} mutationType{name}}}`,
	} {
		r := graphRequest(t, e, document, nil)
		require.Equal(t, http.StatusOK, r.Code)
		require.NotContains(t, r.Body.String(), `"errors"`)
	}
	require.EqualValues(t, 2, s.calls.Load())
}

func TestGraphQLBoundsDocumentsAndSelectionDepth(t *testing.T) {
	s := &boundedSearch{}
	e := limitsEngine(t, s)
	r := graphRequest(t, e, "{__typename}"+strings.Repeat(" ", 64<<10), nil)
	require.Contains(t, r.Body.String(), `"OPERATION_LIMIT_EXCEEDED"`)
	deep := `{__type(name:"Query"){` + strings.Repeat("ofType{", 21) + "name" + strings.Repeat("}", 21) + "}}"
	r = graphRequest(t, e, deep, nil)
	require.Contains(t, r.Body.String(), `"OPERATION_LIMIT_EXCEEDED"`)
	var wide strings.Builder
	wide.WriteString("{")
	for i := range 4000 {
		fmt.Fprintf(&wide, "f%d:__typename ", i)
	}
	wide.WriteString("}")
	r = graphRequest(t, e, wide.String(), nil)
	require.Contains(t, r.Body.String(), `"GRAPHQL_PARSE_FAILED"`)
	require.Zero(t, s.calls.Load())
}

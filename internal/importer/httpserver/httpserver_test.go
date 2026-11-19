package httpserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/spencernorton/DH-Waypoint/internal/importer"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type testImporter struct {
	ctx    context.Context
	active *testActiveImport
}

func (i *testImporter) New(ctx context.Context, _ importer.Info) importer.ActiveImport {
	i.ctx = ctx
	return i.active
}

type testActiveImport struct {
	items     []importer.Item
	closed    bool
	drained   bool
	importErr error
	closeErr  error
}

func (i *testActiveImport) Import(items ...importer.Item) error {
	if i.importErr != nil {
		return i.importErr
	}
	i.items = append(i.items, items...)
	return nil
}
func (i *testActiveImport) Drain()       { i.drained = true }
func (i *testActiveImport) Closed() bool { return i.closed }
func (i *testActiveImport) Close() error { i.closed = true; return i.closeErr }
func (i *testActiveImport) Err() error   { return i.closeErr }

func importEngine(t *testing.T, i *testImporter) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	b := builder{importer: lazy.New(func() (importer.Importer, error) { return i, nil }), logger: zap.NewNop().Sugar()}
	require.NoError(t, b.Apply(e))
	return e
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, errors.New("synthetic body read failure") }

func TestImportRejectsOversizedLinesAndReadErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     io.Reader
		code     int
		accepted int
	}{
		{"oversized line", strings.NewReader(`{"Name":"` + strings.Repeat("x", 1<<20) + `"}`), http.StatusRequestEntityTooLarge, 0},
		{"read error", io.MultiReader(strings.NewReader("{\"Name\":\"synthetic\"}\n"), brokenReader{}), http.StatusBadRequest, 1},
		{"read error with unterminated tail", io.MultiReader(strings.NewReader("{\"Name\":\"synthetic\"}"), brokenReader{}), http.StatusBadRequest, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := &testImporter{active: &testActiveImport{}}
			r := httptest.NewRecorder()
			importEngine(t, i).ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/import", tc.body))
			require.Equal(t, tc.code, r.Code)
			require.NotContains(t, r.Body.String(), "import complete")
			require.True(t, i.active.closed)
			require.True(t, i.active.drained)
			require.Len(t, i.active.items, tc.accepted)
		})
	}
}

func TestImportAlwaysClosesAfterFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		active *testActiveImport
	}{
		{"bad JSON", "{", &testActiveImport{}},
		{"import failure", `{"Name":"synthetic"}`, &testActiveImport{importErr: errors.New("synthetic import failure")}},
		{"close failure", `{"Name":"synthetic"}`, &testActiveImport{closeErr: errors.New("synthetic close failure")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := &testImporter{active: tc.active}
			r := httptest.NewRecorder()
			importEngine(t, i).ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/import", strings.NewReader(tc.body)))
			require.Equal(t, http.StatusBadRequest, r.Code)
			require.NotContains(t, r.Body.String(), "import complete")
			require.True(t, i.active.closed)
			require.True(t, i.active.drained)
		})
	}
}

func TestImportStreamsMultipleBoundedLinesAndPreservesUnicode(t *testing.T) {
	i := &testImporter{active: &testActiveImport{}}
	item := `{"Name":"synthetic"}`
	line := item + strings.Repeat(" ", (1<<20)-len(item))
	body := "\n" + line + "\r\n" + line + "\n" + "{\"Name\":\"one\u2028two\"}"
	r := httptest.NewRecorder()
	importEngine(t, i).ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/import", strings.NewReader(body)))
	require.Equal(t, http.StatusOK, r.Code)
	require.Contains(t, r.Body.String(), "3 items imported\nimport complete")
	require.Len(t, i.active.items, 3)
	require.Equal(t, "one\u2028two", i.active.items[2].Name)
	require.True(t, i.active.closed)
}

func TestImportUsesRequestCancellation(t *testing.T) {
	i := &testImporter{active: &testActiveImport{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/import", strings.NewReader("")).WithContext(ctx)
	importEngine(t, i).ServeHTTP(r, request)
	require.ErrorIs(t, i.ctx.Err(), context.Canceled)
}

func TestImportFailureAfterProgressHasNoCompletionMarker(t *testing.T) {
	i := &testImporter{active: &testActiveImport{}}
	body := strings.Repeat("{\"Name\":\"synthetic\"}\n", 1000) + "{"
	r := httptest.NewRecorder()
	importEngine(t, i).ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/import", strings.NewReader(body)))
	require.Equal(t, http.StatusOK, r.Code, "stream progress has already committed headers")
	require.Contains(t, r.Body.String(), "1000 items imported\n")
	require.Contains(t, r.Body.String(), "import failed\n")
	require.NotContains(t, r.Body.String(), "import complete")
	require.Len(t, i.active.items, 1000)
	require.True(t, i.active.closed)
}

type selectiveActiveImport struct {
	testActiveImport
	rejectName string
}

func (s *selectiveActiveImport) Import(items ...importer.Item) error {
	for _, it := range items {
		if it.Name == s.rejectName {
			return errors.New("synthetic rejected item")
		}
	}
	return s.testActiveImport.Import(items...)
}

func TestImportOnErrorSkipIsolatesLineErrorsWhileFailingStreamErrors(t *testing.T) {
	body := strings.Join([]string{
		`{"Name":"good-one"}`,
		`{bad-json`,
		`{"Name":"reject-me"}`,
		`{"Name":"good-two"}`,
	}, "\n")

	for _, useHeader := range []bool{false, true} {
		name := "query_param"
		if useHeader {
			name = "header"
		}
		t.Run(name, func(t *testing.T) {
			sel := &selectiveActiveImport{rejectName: "reject-me"}
			i := &testImporter{active: &sel.testActiveImport}
			g := gin.New()
			b := builder{importer: lazy.New(func() (importer.Importer, error) {
				return selectiveImporter{sel}, nil
			}), logger: zap.NewNop().Sugar()}
			require.NoError(t, b.Apply(g))

			target := "/import?on_error=skip"
			if useHeader {
				target = "/import"
			}
			req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
			if useHeader {
				req.Header.Set(ImportOnErrorHeader, "skip")
			}
			_ = i
			r := httptest.NewRecorder()
			g.ServeHTTP(r, req)
			require.Equal(t, http.StatusOK, r.Code)
			require.Contains(t, r.Body.String(), "2 items imported (2 skipped)\nimport complete\n")
			require.Len(t, sel.items, 2)
			require.True(t, sel.closed)
		})
	}

	t.Run("oversized line still fails closed under skip", func(t *testing.T) {
		i := &testImporter{active: &testActiveImport{}}
		r := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/import?on_error=skip", strings.NewReader(`{"Name":"`+strings.Repeat("x", 1<<20)+`"}`))
		importEngine(t, i).ServeHTTP(r, req)
		require.Equal(t, http.StatusRequestEntityTooLarge, r.Code)
	})
}

type selectiveImporter struct {
	active *selectiveActiveImport
}

func (s selectiveImporter) New(_ context.Context, _ importer.Info) importer.ActiveImport {
	return s.active
}


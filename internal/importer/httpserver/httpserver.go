package httpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/spencernorton/DH-Waypoint/internal/httpserver"
	"github.com/spencernorton/DH-Waypoint/internal/importer"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type Params struct {
	fx.In
	Importer lazy.Lazy[importer.Importer]
	Logger   *zap.SugaredLogger
}

type Result struct {
	fx.Out
	Option httpserver.Option `group:"http_server_options"`
}

func New(p Params) Result {
	return Result{
		Option: &builder{
			importer: p.Importer,
			logger:   p.Logger.Named("importer"),
		},
	}
}

const ImportIDHeader = "X-Import-Id"

const maxImportLineBytes = 1 << 20

var errImportLineTooLarge = errors.New("import line exceeds 1048576 bytes")

type builder struct {
	importer lazy.Lazy[importer.Importer]
	logger   *zap.SugaredLogger
}

func (builder) Key() string {
	return "import"
}

func (b builder) Apply(e *gin.Engine) error {
	i, err := b.importer.Get()
	if err != nil {
		return err
	}

	e.POST("/import", func(ctx *gin.Context) {
		b.handle(ctx, i)
	})
	e.POST("/import/torrent", httpserver.QueryBudget(30*time.Second), func(ctx *gin.Context) { b.handleTorrent(ctx, i) })

	return nil
}

func (b builder) handle(ctx *gin.Context, i importer.Importer) {
	importID := ctx.Request.Header.Get(ImportIDHeader)
	if importID == "" {
		importID = strconv.FormatUint(uint64(time.Now().Unix()), 10)
	}

	ai := i.New(ctx.Request.Context(), importer.Info{
		ID: importID,
	})
	closed := false
	defer func() {
		if !closed {
			ai.Drain()
			_ = ai.Close()
		}
	}()

	count := 0
	writeCount := func() {
		_, _ = fmt.Fprintf(ctx.Writer, "%d items imported\n", count)
	}
	streamErr := readImportItems(ctx.Request.Context(), ctx.Request.Body, func(item importer.Item) error {
		if err := ai.Import(item); err != nil {
			return err
		}

		count++
		if count%1_000 == 0 {
			writeCount()

			if count%10_000 == 0 {
				ctx.Writer.Flush()
			}
		}

		return nil
	})

	ai.Drain()
	closeErr := ai.Close()
	closed = true
	if streamErr == nil {
		streamErr = ctx.Request.Context().Err()
	}
	if err := errors.Join(streamErr, closeErr); err != nil {
		b.logger.Errorw("import failed", "error", err)
		status, message := http.StatusBadRequest, "import failed\n"
		if errors.Is(err, errImportLineTooLarge) {
			status, message = http.StatusRequestEntityTooLarge, errImportLineTooLarge.Error()+"\n"
		}
		ctx.Status(status)
		_, _ = ctx.Writer.WriteString(message)
		return
	}

	ctx.Status(200)
	writeCount()

	_, _ = ctx.Writer.WriteString("import complete\n")
}

// readImportItems bounds each NDJSON item while allowing the overall stream to
// contain arbitrarily many bounded lines. Only LF delimits records; Unicode
// line separators inside JSON strings are retained.
func readImportItems(ctx context.Context, r io.Reader, add func(importer.Item) error) error {
	s := bufio.NewScanner(r)
	// Leave room for the CRLF delimiter at the maximum valid line size.
	s.Buffer(make([]byte, 64<<10), maxImportLineBytes+2)
	for s.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		// A non-EOF read error must not turn an unterminated buffered tail
		// into an accepted final record.
		if err := s.Err(); err != nil {
			return err
		}
		line := s.Bytes()
		if len(line) > maxImportLineBytes {
			return errImportLineTooLarge
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var item importer.Item
		//nolint:musttag
		if err := json.Unmarshal(line, &item); err != nil {
			return fmt.Errorf("decode import item: %w", err)
		}
		if err := add(item); err != nil {
			return fmt.Errorf("import item: %w", err)
		}
	}
	if errors.Is(s.Err(), bufio.ErrTooLong) {
		return errImportLineTooLarge
	}
	if err := s.Err(); err != nil {
		return fmt.Errorf("read import stream: %w", err)
	}
	return ctx.Err()
}

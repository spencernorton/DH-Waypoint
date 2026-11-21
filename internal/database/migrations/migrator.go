package migrations

import (
	"context"
	"database/sql"

	goose "github.com/pressly/goose/v3"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	migrationssql "github.com/spencernorton/DH-Waypoint/migrations"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

type Params struct {
	fx.In
	DB     lazy.Lazy[*gorm.DB]
	Logger *zap.SugaredLogger
}

type Result struct {
	fx.Out
	Migrator lazy.Lazy[Migrator]
}

func New(p Params) Result {
	return Result{
		Migrator: lazy.New(func() (Migrator, error) {
			g, err := p.DB.Get()
			if err != nil {
				return nil, err
			}
			db, err := g.DB()
			if err != nil {
				return nil, err
			}
			logger := p.Logger.Named("migrator")
			initGoose(logger)
			return &migrator{
				db:     db,
				logger: logger,
			}, nil
		}),
	}
}

func initGoose(logger *zap.SugaredLogger) {
	goose.SetLogger(gooseLogger{logger})
	goose.SetBaseFS(migrationssql.FS)

	err := goose.SetDialect("postgres")
	if err != nil {
		panic(err)
	}
}

type Migrator interface {
	Up(ctx context.Context) error
	UpWithConfirm(ctx context.Context, confirmToken string) (*PreFlightOutcome, error)
	UpTo(ctx context.Context, version int64) error
	Down(ctx context.Context) error
	DownTo(ctx context.Context, version int64) error
}

type migrator struct {
	db     *sql.DB
	logger *zap.SugaredLogger
}

func (m *migrator) Up(ctx context.Context) error {
	_, err := m.UpWithConfirm(ctx, "")
	return err
}

func (m *migrator) UpWithConfirm(ctx context.Context, confirmToken string) (*PreFlightOutcome, error) {
	outcome, err := CheckV6PreFlight(ctx, m.db, confirmToken)
	if err != nil {
		return nil, err
	}
	if outcome.Case == "v5_auto_upgrade" {
		m.logger.Infow("v6 pre-flight backup acknowledgement verified; running automatic v5 -> v6 upgrade",
			"pre_upgrade_goose_version", outcome.PreUpgradeGooseVer,
			"fingerprint", outcome.PreUpgradeFingerprint,
		)
	}
	m.logger.Info("checking and applying migrations...")
	if err := goose.UpContext(ctx, m.db, "."); err != nil {
		return nil, err
	}
	if err := RecordUpgradeReceipt(ctx, m.db, outcome); err != nil {
		return nil, err
	}
	return outcome, nil
}

func (m *migrator) UpTo(ctx context.Context, version int64) error {
	if _, err := CheckV6PreFlight(ctx, m.db, ""); err != nil {
		return err
	}
	return goose.UpToContext(ctx, m.db, ".", version)
}

func (m *migrator) Down(ctx context.Context) error {
	return goose.DownContext(ctx, m.db, ".")
}

func (m *migrator) DownTo(ctx context.Context, version int64) error {
	return goose.DownToContext(ctx, m.db, ".", version)
}

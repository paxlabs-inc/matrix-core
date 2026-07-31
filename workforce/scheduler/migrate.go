package scheduler

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"matrix/workforce/internal/ledger"
)

func ApplyMigrations(
	ctx context.Context,
	pool *pgxpool.Pool,
	now time.Time,
) error {
	return ledger.ApplyMigrations(ctx, pool, now)
}

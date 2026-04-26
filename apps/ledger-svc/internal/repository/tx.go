package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// InTx runs fn inside a SERIALIZABLE Postgres transaction, retrying the
// entire transaction on SQLSTATE 40001 with exponential backoff.
//
// Lifecycle ownership:
//   - InTx begins, commits, and rolls back the tx. fn MUST NOT call
//     Commit or Rollback itself.
//   - On any non-nil return from fn (including domain errors), the tx is
//     rolled back. SQLSTATE 40001 errors trigger a retry of the whole fn.
//
// Why SERIALIZABLE: prevents write-skew anomalies that READ COMMITTED and
// REPEATABLE READ both allow — e.g. two concurrent transfers each reading
// a balance and both succeeding when only one should. The cost is more
// 40001 retries under contention, which is exactly what this wrapper
// absorbs.
func InTx(
	ctx context.Context,
	pool *pgxpool.Pool,
	cfg RetryConfig,
	fn func(ctx context.Context, tx pgx.Tx) error,
) error {
	return WithRetryOnSerializationFailure(ctx, cfg, func() (returnedErr error) {
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
		if err != nil {
			return fmt.Errorf("repository: begin tx: %w", err)
		}
		defer func() {
			if returnedErr != nil {
				// Best-effort rollback; ErrTxClosed means it already committed
				// or was rolled back, which is fine.
				if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
					// Don't mask the original error with the rollback error.
					returnedErr = fmt.Errorf("%w (rollback: %v)", returnedErr, rbErr)
				}
			}
		}()
		if err := fn(ctx, tx); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("repository: commit tx: %w", err)
		}
		return nil
	})
}

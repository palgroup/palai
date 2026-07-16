package coordinator

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func RunHeldCompletionWorker(
	ctx context.Context,
	store *Store,
	claim Claim,
	resultHash string,
	ready func() error,
) error {
	transaction, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin held completion: %w", err)
	}
	defer func() { _ = transaction.Rollback(context.Background()) }()
	if err := stageCompletion(ctx, transaction, claim, resultHash); err != nil {
		return err
	}
	if err := ready(); err != nil {
		return fmt.Errorf("announce staged completion: %w", err)
	}
	<-ctx.Done()
	return ctx.Err()
}

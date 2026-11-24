package backup

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type pgxConn = *pgx.Conn

// prepareAmcheck installs amcheck in every database of a restored scratch
// copy (it is thrown away afterwards) and returns how many there are.
func prepareAmcheck(ctx context.Context, connect func(context.Context, string) (pgxConn, error)) (int, error) {
	c, err := connect(ctx, "postgres")
	if err != nil {
		return 0, err
	}
	rows, err := c.Query(ctx, "SELECT datname FROM pg_database WHERE datallowconn ORDER BY 1")
	if err != nil {
		c.Close(context.Background())
		return 0, err
	}
	dbs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	c.Close(context.Background())
	if err != nil {
		return 0, err
	}
	for _, db := range dbs {
		dc, err := connect(ctx, db)
		if err != nil {
			return 0, err
		}
		_, err = dc.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS amcheck")
		dc.Close(context.Background())
		if err != nil {
			return 0, fmt.Errorf("install amcheck in %s: %w", db, err)
		}
	}
	return len(dbs), nil
}

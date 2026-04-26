package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// rowScanner is the common surface of `*sql.Row` and a single
// iteration of `*sql.Rows`: both expose `Scan(dest ...any) error`.
// Decoders take this interface so the same decoder works for
// `queryRow` and `queryRows`.
type rowScanner interface {
	Scan(dest ...any) error
}

// queryRow runs query/args via `QueryRowContext` and applies decode
// to the resulting row. If the underlying scan returns
// `sql.ErrNoRows` and missingErr is non-nil, missingErr replaces it.
// Otherwise the underlying error is returned unchanged so the caller
// can wrap it with operation-specific context.
func queryRow[T any](
	ctx context.Context,
	db *sql.DB,
	query string,
	args []any,
	missingErr error,
	decode func(rowScanner) (T, error),
) (T, error) {
	row := db.QueryRowContext(ctx, query, args...)

	value, err := decode(row)
	if errors.Is(err, sql.ErrNoRows) && missingErr != nil {
		var zero T
		return zero, missingErr
	}

	return value, err
}

// queryRows runs query/args, defers `rows.Close`, and applies decode
// to each row in turn. The caller never touches the
// `Close`/`Next`/`Err` ceremony directly.
func queryRows[T any](
	ctx context.Context,
	db *sql.DB,
	query string,
	args []any,
	decode func(rowScanner) (T, error),
) ([]T, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []T

	for rows.Next() {
		v, err := decode(rows)
		if err != nil {
			return nil, err
		}

		out = append(out, v)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return out, nil
}

// execMutation runs `ExecContext` and discards the result, returning
// only the error. Matches the dominant fire-and-forget INSERT/
// UPDATE/DELETE shape.
func execMutation(ctx context.Context, db *sql.DB, query string, args ...any) error {
	_, err := db.ExecContext(ctx, query, args...)
	return err
}

// execInsert runs `ExecContext` and returns `LastInsertId`. Used by
// AppendEvent and any other INSERT that needs the autoincrement
// row id.
func execInsert(ctx context.Context, db *sql.DB, query string, args ...any) (int64, error) {
	result, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}

	return result.LastInsertId()
}

// jsonColumn decodes a single TEXT column carrying a JSON document
// into T. Use as the `decode` argument to `queryRow` / `queryRows`
// for the `SELECT data FROM …` shape that pervades this package.
func jsonColumn[T any](r rowScanner) (T, error) {
	var (
		zero T
		data string
	)

	if err := r.Scan(&data); err != nil {
		return zero, err
	}

	var v T
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		return zero, err
	}

	return v, nil
}

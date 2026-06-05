package examplehelper

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"github.com/DeluxeOwl/chronicle/internal/assert"
	"github.com/olekukonko/tablewriter"
)

type SQLPrinter struct {
	db *sql.DB
}

func NewSQLPrinter(db *sql.DB) *SQLPrinter {
	return &SQLPrinter{
		db: db,
	}
}

func (s *SQLPrinter) Query(ctx context.Context, query string, args ...any) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		assert.Neverf("run query: %v", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		assert.Neverf("get columns: %v", err)
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.Header(cols)

	values := make([]any, len(cols))
	valuePtrs := make([]any, len(cols))
	for i := range values {
		valuePtrs[i] = &values[i]
	}

	for rows.Next() {
		if err := rows.Scan(valuePtrs...); err != nil {
			assert.Neverf("scan row: %v", err)
		}

		row := make([]string, len(cols))
		for i, val := range values {
			if val == nil {
				row[i] = "NULL"
			} else {
				row[i] = fmt.Sprintf("%v", val)
			}
		}

		err := table.Append(row)
		if err != nil {
			assert.Neverf("table append: %v", err)
		}
	}

	err = table.Render()
	if err != nil {
		assert.Neverf("table render: %v", err)
	}

	err = rows.Err()
	if err != nil {
		assert.Neverf("rows %v", err)
	}
}

package examplehelper

import (
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

func (s *SQLPrinter) Query(query string, args ...any) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		assert.Never("run query: %v", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		assert.Never("get columns: %v", err)
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
			assert.Never("scan row: %v", err)
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
			assert.Never("table append: %v", err)
		}
	}

	err = table.Render()
	if err != nil {
		assert.Never("table render: %v", err)
	}

	err = rows.Err()
	if err != nil {
		assert.Never("rows %v", err)
	}
}

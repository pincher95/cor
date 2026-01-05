/*
Copyright 2024 Elastic Scaler Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package printer

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
)

const defaultChunkRows = 200

// StreamTable prints rows as they are produced, flushing in chunks to keep memory bounded.
// Output is a bordered table (go-pretty). Sorting is not supported.
type StreamTable struct {
	out       io.Writer
	index     bool
	counter   int
	header    table.Row
	rows      []table.Row
	chunkRows int

	sortBy   string
	sortDesc bool
}

func NewStreamTable(out io.Writer, index bool, header []string) *StreamTable {
	st := &StreamTable{
		out:     out,
		index:   index,
		counter: 0,
		rows:    make([]table.Row, 0, defaultChunkRows),
		// keep configurable if needed later
		chunkRows: defaultChunkRows,
	}

	st.WriteHeader(header)
	return st
}

// Close is a convenience for defers (avoids errcheck warnings on Flush()).
// StreamTable flushes to an io.Writer; flush errors are best-effort.
func (t *StreamTable) Close() {
	_ = t.Flush()
}

func (t *StreamTable) WriteHeader(header []string) {
	if t.index {
		row := make(table.Row, 0, len(header)+1)
		row = append(row, "#")
		for _, h := range header {
			row = append(row, h)
		}
		t.header = row
	} else {
		row := make(table.Row, 0, len(header))
		for _, h := range header {
			row = append(row, h)
		}
		t.header = row
	}
}

func (t *StreamTable) WriteRow(cols ...any) {
	t.counter++

	// When sorting is enabled, we must be able to re-number the index after sorting.
	// So we store data rows WITHOUT the index column and add it at Flush().
	if t.sortBy != "" && t.index {
		row := make(table.Row, 0, len(cols))
		row = append(row, cols...)
		t.rows = append(t.rows, row)
		return
	}

	row := make(table.Row, 0, len(cols)+1)
	if t.index {
		row = append(row, t.counter)
	}
	row = append(row, cols...)
	t.rows = append(t.rows, row)

	// If sorting is enabled, we must buffer everything and sort at Flush().
	if t.sortBy != "" {
		return
	}

	if len(t.rows) >= t.chunkRows {
		_ = t.flushChunk()
	}
}

func (t *StreamTable) Flush() error {
	if t.sortBy != "" {
		return t.flushSorted()
	}
	return t.flushChunk()
}

func (t *StreamTable) flushChunk() error {
	if len(t.rows) == 0 {
		return nil
	}

	tw := table.NewWriter()
	tw.SetOutputMirror(t.out)
	tw.AppendHeader(t.header)
	tw.AppendRows(t.rows)
	tw.Style().Options.SeparateRows = true
	tw.SuppressTrailingSpaces()
	tw.Render()

	// reset buffer, keep capacity
	t.rows = t.rows[:0]
	return nil
}

// SetSort enables sorting by column name. This will buffer all rows and render once at Flush().
// Column name match is case-insensitive against the header labels.
func (t *StreamTable) SetSort(sortBy string, desc bool) {
	t.sortBy = strings.TrimSpace(sortBy)
	t.sortDesc = desc
}

func (t *StreamTable) flushSorted() error {
	if len(t.rows) == 0 {
		return nil
	}

	sortIdx, ok := t.resolveSortDataIndex(t.sortBy)
	if ok {
		sort.SliceStable(t.rows, func(i, j int) bool {
			ai := ""
			aj := ""
			if sortIdx < len(t.rows[i]) {
				ai = strings.ToLower(fmt.Sprint(t.rows[i][sortIdx]))
			}
			if sortIdx < len(t.rows[j]) {
				aj = strings.ToLower(fmt.Sprint(t.rows[j][sortIdx]))
			}
			if t.sortDesc {
				return ai > aj
			}
			return ai < aj
		})
	}

	renderRows := t.rows
	if t.index {
		// Re-number rows AFTER sorting.
		renderRows = make([]table.Row, 0, len(t.rows))
		for idx, r := range t.rows {
			row := make(table.Row, 0, len(r)+1)
			row = append(row, idx+1)
			row = append(row, r...)
			renderRows = append(renderRows, row)
		}
	}

	tw := table.NewWriter()
	tw.SetOutputMirror(t.out)
	tw.AppendHeader(t.header)
	tw.AppendRows(renderRows)
	tw.Style().Options.SeparateRows = true
	tw.SuppressTrailingSpaces()
	tw.Render()

	t.rows = t.rows[:0]
	return nil
}

func (t *StreamTable) resolveSortDataIndex(requested string) (int, bool) {
	if requested == "" {
		return 0, false
	}
	for headerIdx, h := range t.header {
		hs, ok := h.(string)
		if !ok {
			continue
		}
		if strings.EqualFold(hs, requested) {
			// For sorting, use the data-row column index.
			// When index column is enabled, header has an extra "#" prefix.
			dataIdx := headerIdx
			if t.index {
				dataIdx = headerIdx - 1
			}
			if dataIdx < 0 {
				return 0, false
			}
			return dataIdx, true
		}
	}
	return 0, false
}

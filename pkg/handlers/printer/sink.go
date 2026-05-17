/*
Copyright 2024 Cloud Orphaned Resources Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package printer

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// RowSink is the common interface for streamed table-shaped output. Both
// StreamTable (text) and the JSON / CSV implementations satisfy it.
type RowSink interface {
	SetSort(col string, desc bool)
	WriteRow(cols ...any)
	Close()
}

// NewSink constructs a RowSink for the requested format. Unknown formats
// fall back to "table".
func NewSink(format string, out io.Writer, index bool, header []string) RowSink {
	switch format {
	case "json":
		return newJSONSink(out, header)
	case "csv":
		return newCSVSink(out, index, header)
	default:
		return NewStreamTable(out, index, header)
	}
}

// jsonSink writes one JSON object per row to `out`. Newline-delimited JSON
// (NDJSON) keeps output streamable and parseable. SetSort is not honored;
// JSON output is left in producer order for downstream tools.
type jsonSink struct {
	enc    *json.Encoder
	header []string
}

func newJSONSink(out io.Writer, header []string) *jsonSink {
	return &jsonSink{enc: json.NewEncoder(out), header: header}
}

func (j *jsonSink) SetSort(col string, desc bool) {}

func (j *jsonSink) WriteRow(cols ...any) {
	obj := make(map[string]any, len(j.header))
	for i, h := range j.header {
		if i >= len(cols) {
			break
		}
		obj[h] = cols[i]
	}
	_ = j.enc.Encode(obj)
}

func (j *jsonSink) Close() {}

// csvSink writes RFC4180-style CSV. SetSort buffers rows and sorts at Close.
type csvSink struct {
	w       *csv.Writer
	index   bool
	header  []string
	rows    [][]string
	sortKey string
	sortDsc bool
	count   int
}

func newCSVSink(out io.Writer, index bool, header []string) *csvSink {
	c := &csvSink{w: csv.NewWriter(out), index: index, header: header}
	hdr := header
	if index {
		hdr = append([]string{"#"}, header...)
	}
	_ = c.w.Write(hdr)
	return c
}

func (c *csvSink) SetSort(col string, desc bool) {
	c.sortKey = strings.TrimSpace(col)
	c.sortDsc = desc
}

func (c *csvSink) WriteRow(cols ...any) {
	c.count++
	rec := make([]string, 0, len(cols)+1)
	if c.index && c.sortKey == "" {
		rec = append(rec, fmt.Sprintf("%d", c.count))
	}
	for _, v := range cols {
		rec = append(rec, formatCell(v))
	}
	if c.sortKey != "" {
		c.rows = append(c.rows, rec)
		return
	}
	_ = c.w.Write(rec)
}

func (c *csvSink) Close() {
	if c.sortKey != "" {
		idx := -1
		for i, h := range c.header {
			if strings.EqualFold(h, c.sortKey) {
				idx = i
				break
			}
		}
		if idx >= 0 {
			sort.SliceStable(c.rows, func(i, j int) bool {
				ai, aj := "", ""
				if idx < len(c.rows[i]) {
					ai = c.rows[i][idx]
				}
				if idx < len(c.rows[j]) {
					aj = c.rows[j][idx]
				}
				if c.sortDsc {
					return ai > aj
				}
				return ai < aj
			})
		}
		for i, rec := range c.rows {
			if c.index {
				rec = append([]string{fmt.Sprintf("%d", i+1)}, rec...)
			}
			_ = c.w.Write(rec)
		}
		c.rows = c.rows[:0]
	}
	c.w.Flush()
}

func formatCell(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

/*
Copyright 2024 Cloud Orphaned Resources Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package printer

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestSink_JSONEmitsOnePerRow(t *testing.T) {
	var buf bytes.Buffer
	sink := NewSink("json", &buf, true, []string{"Name", "Size"})
	sink.WriteRow("alpha", 1)
	sink.WriteRow("beta", 2)
	sink.Close()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 JSON lines, got %d:\n%s", len(lines), buf.String())
	}
	var got struct {
		Name string `json:"Name"`
		Size int    `json:"Size"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("line 0 not valid JSON: %v", err)
	}
	if got.Name != "alpha" || got.Size != 1 {
		t.Errorf("unexpected first row: %+v", got)
	}
}

func TestSink_CSVHeaderAndRows(t *testing.T) {
	var buf bytes.Buffer
	sink := NewSink("csv", &buf, true, []string{"Name", "Size"})
	sink.WriteRow("a", 1)
	sink.WriteRow("b", 2)
	sink.Close()

	out := buf.String()
	if !strings.HasPrefix(out, "#,Name,Size") {
		t.Errorf("expected indexed CSV header, got: %q", out)
	}
	if !strings.Contains(out, "1,a,1") {
		t.Errorf("expected first row, got: %q", out)
	}
}

func TestSink_TableFallbackForUnknown(t *testing.T) {
	var buf bytes.Buffer
	sink := NewSink("yaml", &buf, true, []string{"Name"})
	sink.WriteRow("x")
	sink.Close()

	if buf.Len() == 0 {
		t.Errorf("expected some output from fallback table sink")
	}
}

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
	"strings"
	"testing"
)

func TestStreamTable_SortAndRenumberIndex(t *testing.T) {
	var out strings.Builder

	st := NewStreamTable(&out, true, []string{"Name", "ID"})
	st.SetSort("Name", false)
	st.WriteRow("bbb", "2")
	st.WriteRow("aaa", "1")
	st.Close()

	s := out.String()
	if !strings.Contains(s, "aaa") || !strings.Contains(s, "bbb") {
		t.Fatalf("expected output to contain row values, got:\n%s", s)
	}

	aaaIdx := strings.Index(s, "aaa")
	bbbIdx := strings.Index(s, "bbb")
	if aaaIdx == -1 || bbbIdx == -1 {
		t.Fatalf("expected both values to be present")
	}
	if aaaIdx > bbbIdx {
		t.Fatalf("expected aaa to come before bbb after sort, got:\n%s", s)
	}

	var aaaLine, bbbLine string
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "aaa") {
			aaaLine = line
		}
		if strings.Contains(line, "bbb") {
			bbbLine = line
		}
	}
	if aaaLine == "" || bbbLine == "" {
		t.Fatalf("expected to find lines for both rows, got:\n%s", s)
	}
	if !strings.Contains(aaaLine, "1") {
		t.Fatalf("expected aaa row to be indexed as 1, got line: %q", aaaLine)
	}
	if !strings.Contains(bbbLine, "2") {
		t.Fatalf("expected bbb row to be indexed as 2, got line: %q", bbbLine)
	}
}

func TestStreamTable_UnknownSortKeyKeepsOrder(t *testing.T) {
	var out strings.Builder

	st := NewStreamTable(&out, true, []string{"Name", "ID"})
	st.SetSort("does-not-exist", false)
	st.WriteRow("first", "1")
	st.WriteRow("second", "2")
	st.Close()

	s := out.String()
	if strings.Index(s, "first") > strings.Index(s, "second") {
		t.Fatalf("expected original order when sort key is unknown, got:\n%s", s)
	}
}

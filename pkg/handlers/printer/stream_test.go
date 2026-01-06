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

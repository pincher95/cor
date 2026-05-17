/*
Copyright 2024 Cloud Orphaned Resources Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/prompter"
	"github.com/spf13/viper"
)

type fakePrompter struct {
	answer bool
	err    error
}

func (f *fakePrompter) Confirm(prompt string) (*bool, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &f.answer, nil
}

func newTestAWSCommand(out *bytes.Buffer, promp prompter.Client) *AWSCommand {
	return &AWSCommand{
		Logger:   logging.NewLogger(),
		Prompter: promp,
		Output:   out,
	}
}

// baseGlobals returns a *flags.GlobalFlags with sensible test defaults.
func baseGlobals(delete bool) *flags.GlobalFlags {
	return &flags.GlobalFlags{
		Region:     "us-east-1",
		Profile:    "default",
		AuthMethod: "AWS_CREDENTIALS_FILE",
		Delete:     delete,
		SortBy:     "",
		SortDesc:   false,
	}
}

// baseExtras returns an empty extras map.
func baseExtras() *map[string]any {
	m := map[string]any{}
	return &m
}

func TestRunOrphanPipeline_HappyPath_StreamsAllRows(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	out := &bytes.Buffer{}
	promp := &fakePrompter{answer: false}
	awsCmd := newTestAWSCommand(out, promp)

	var finalizeCalled bool

	err := runOrphanPipeline(awsCmd, context.Background(), baseGlobals(false), baseExtras(), OrphanPipeline[int, string]{
		Headers:       []string{"Index", "Value"},
		ResourceLabel: "test items",
		List: func(ctx context.Context, emit func(int) error) error {
			for i := 1; i <= 3; i++ {
				if err := emit(i); err != nil {
					return err
				}
			}
			return nil
		},
		Process: func(_ context.Context, item int) (*string, error) {
			s := strings.Repeat("x", item)
			return &s, nil
		},
		ToRow: func(r string) []any {
			return []any{len(r), r}
		},
		Finalize: func(results []string) []any {
			finalizeCalled = true
			return []any{"Total", len(results)}
		},
	})
	if err != nil {
		t.Fatalf("runOrphanPipeline returned error: %v", err)
	}
	if !finalizeCalled {
		t.Error("expected Finalize to be called")
	}
	output := out.String()
	// Each of "x", "xx", "xxx" plus Finalize "Total 3" should appear in output.
	for _, want := range []string{"x", "xx", "xxx", "Total"} {
		if !bytes.Contains([]byte(output), []byte(want)) {
			t.Errorf("expected output to contain %q; got:\n%s", want, output)
		}
	}
}

func TestRunOrphanPipeline_DeleteCalledWhenConfirmed(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	out := &bytes.Buffer{}
	promp := &fakePrompter{answer: true}
	awsCmd := newTestAWSCommand(out, promp)

	var deletes int

	err := runOrphanPipeline(awsCmd, context.Background(), baseGlobals(true), baseExtras(), OrphanPipeline[int, int]{
		Headers: []string{"Value"},
		List: func(ctx context.Context, emit func(int) error) error {
			for _, i := range []int{10, 20, 30} {
				if err := emit(i); err != nil {
					return err
				}
			}
			return nil
		},
		Process: func(_ context.Context, item int) (*int, error) {
			v := item
			return &v, nil
		},
		ToRow:  func(r int) []any { return []any{r} },
		Delete: func(ctx context.Context, r int) error { deletes++; return nil },
	})
	if err != nil {
		t.Fatalf("runOrphanPipeline returned error: %v", err)
	}
	if deletes != 3 {
		t.Errorf("expected Delete to be called 3 times, got %d", deletes)
	}
}

func TestRunOrphanPipeline_DeleteNotCalledWhenFlagFalse(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	out := &bytes.Buffer{}
	promp := &fakePrompter{answer: true}
	awsCmd := newTestAWSCommand(out, promp)

	var deletes int

	err := runOrphanPipeline(awsCmd, context.Background(), baseGlobals(false), baseExtras(), OrphanPipeline[int, int]{
		Headers: []string{"Value"},
		List: func(ctx context.Context, emit func(int) error) error {
			return emit(1)
		},
		Process: func(_ context.Context, item int) (*int, error) { v := item; return &v, nil },
		ToRow:   func(r int) []any { return []any{r} },
		Delete:  func(ctx context.Context, r int) error { deletes++; return nil },
	})
	if err != nil {
		t.Fatalf("runOrphanPipeline returned error: %v", err)
	}
	if deletes != 0 {
		t.Errorf("expected Delete NOT to be called, got %d invocations", deletes)
	}
}

func TestRunOrphanPipeline_ProcessNilSkipsItem(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	out := &bytes.Buffer{}
	promp := &fakePrompter{answer: false}
	awsCmd := newTestAWSCommand(out, promp)

	var rows int

	err := runOrphanPipeline(awsCmd, context.Background(), baseGlobals(false), baseExtras(), OrphanPipeline[int, int]{
		Headers: []string{"Value"},
		List: func(ctx context.Context, emit func(int) error) error {
			for i := 1; i <= 4; i++ {
				if err := emit(i); err != nil {
					return err
				}
			}
			return nil
		},
		Process: func(_ context.Context, item int) (*int, error) {
			if item%2 == 0 {
				return nil, nil // skip evens
			}
			v := item
			return &v, nil
		},
		ToRow: func(r int) []any { rows++; return []any{r} },
	})
	if err != nil {
		t.Fatalf("runOrphanPipeline returned error: %v", err)
	}
	if rows != 2 {
		t.Errorf("expected 2 rows (odd items only), got %d", rows)
	}
}

func TestRunOrphanPipeline_ListErrorAborts(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	out := &bytes.Buffer{}
	awsCmd := newTestAWSCommand(out, &fakePrompter{answer: false})

	sentinel := errors.New("list boom")
	err := runOrphanPipeline(awsCmd, context.Background(), baseGlobals(false), baseExtras(), OrphanPipeline[int, int]{
		Headers: []string{"Value"},
		List: func(ctx context.Context, emit func(int) error) error {
			return sentinel
		},
		Process: func(_ context.Context, item int) (*int, error) {
			t.Error("Process should not be called when List errors")
			return nil, nil
		},
		ToRow: func(r int) []any { return []any{r} },
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error, got %v", err)
	}
}

func TestRunOrphanPipeline_ProcessErrorAborts(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	out := &bytes.Buffer{}
	awsCmd := newTestAWSCommand(out, &fakePrompter{answer: false})

	sentinel := errors.New("process boom")
	err := runOrphanPipeline(awsCmd, context.Background(), baseGlobals(false), baseExtras(), OrphanPipeline[int, int]{
		Headers: []string{"Value"},
		List: func(ctx context.Context, emit func(int) error) error {
			// Emit many items; Process will fail on one of them.
			for i := range make([]int, 50) {
				if err := emit(i); err != nil {
					return err
				}
			}
			return nil
		},
		Process: func(_ context.Context, item int) (*int, error) {
			if item == 5 {
				return nil, sentinel
			}
			v := item
			return &v, nil
		},
		ToRow: func(r int) []any { return []any{r} },
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error, got %v", err)
	}
}

func TestRunOrphanPipeline_DeleteErrorAborts(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	out := &bytes.Buffer{}
	awsCmd := newTestAWSCommand(out, &fakePrompter{answer: true})

	sentinel := errors.New("delete boom")
	var deletes int
	err := runOrphanPipeline(awsCmd, context.Background(), baseGlobals(true), baseExtras(), OrphanPipeline[int, int]{
		Headers: []string{"Value"},
		List: func(ctx context.Context, emit func(int) error) error {
			for _, i := range make([]int, 3) {
				if err := emit(i); err != nil {
					return err
				}
			}
			return nil
		},
		Process: func(_ context.Context, item int) (*int, error) { v := item; return &v, nil },
		ToRow:   func(r int) []any { return []any{r} },
		Delete: func(ctx context.Context, r int) error {
			deletes++
			if deletes == 2 {
				return sentinel
			}
			return nil
		},
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error, got %v", err)
	}
	if deletes != 2 {
		t.Errorf("expected Delete to be called twice (success, then sentinel), got %d", deletes)
	}
}

func TestRunOrphanPipeline_PreScanRunsBeforeProducers(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	out := &bytes.Buffer{}
	awsCmd := newTestAWSCommand(out, &fakePrompter{answer: false})

	var preScanDone bool
	err := runOrphanPipeline(awsCmd, context.Background(), baseGlobals(false), baseExtras(), OrphanPipeline[int, int]{
		Headers: []string{"Value"},
		PreScan: func(ctx context.Context) error {
			preScanDone = true
			return nil
		},
		List: func(ctx context.Context, emit func(int) error) error {
			if !preScanDone {
				t.Error("List ran before PreScan completed")
			}
			return emit(1)
		},
		Process: func(_ context.Context, item int) (*int, error) { v := item; return &v, nil },
		ToRow:   func(r int) []any { return []any{r} },
	})
	if err != nil {
		t.Fatalf("runOrphanPipeline returned error: %v", err)
	}
	if !preScanDone {
		t.Error("PreScan never ran")
	}
}

func TestRunOrphanPipeline_PreScanErrorAborts(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	out := &bytes.Buffer{}
	awsCmd := newTestAWSCommand(out, &fakePrompter{answer: false})

	sentinel := errors.New("prescan boom")
	var listCalled bool
	err := runOrphanPipeline(awsCmd, context.Background(), baseGlobals(false), baseExtras(), OrphanPipeline[int, int]{
		Headers: []string{"Value"},
		PreScan: func(ctx context.Context) error { return sentinel },
		List: func(ctx context.Context, emit func(int) error) error {
			listCalled = true
			return emit(1)
		},
		Process: func(_ context.Context, item int) (*int, error) { v := item; return &v, nil },
		ToRow:   func(r int) []any { return []any{r} },
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error, got %v", err)
	}
	if listCalled {
		t.Error("List should not run when PreScan errors")
	}
}

func TestRunOrphanPipeline_ListsMultiProducerMerges(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	out := &bytes.Buffer{}
	awsCmd := newTestAWSCommand(out, &fakePrompter{answer: false})

	var rows int
	err := runOrphanPipeline(awsCmd, context.Background(), baseGlobals(false), baseExtras(), OrphanPipeline[int, int]{
		Headers: []string{"Value"},
		Lists: []func(context.Context, func(int) error) error{
			func(ctx context.Context, emit func(int) error) error {
				for _, v := range []int{1, 2, 3} {
					if err := emit(v); err != nil {
						return err
					}
				}
				return nil
			},
			func(ctx context.Context, emit func(int) error) error {
				for _, v := range []int{10, 20} {
					if err := emit(v); err != nil {
						return err
					}
				}
				return nil
			},
		},
		Process: func(_ context.Context, item int) (*int, error) { v := item; return &v, nil },
		ToRow:   func(r int) []any { rows++; return []any{r} },
	})
	if err != nil {
		t.Fatalf("runOrphanPipeline returned error: %v", err)
	}
	if rows != 5 {
		t.Errorf("expected 5 rows from merged producers, got %d", rows)
	}
}

func TestRunOrphanPipeline_ListAndListsBothSetIsError(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	out := &bytes.Buffer{}
	awsCmd := newTestAWSCommand(out, &fakePrompter{answer: false})

	err := runOrphanPipeline(awsCmd, context.Background(), baseGlobals(false), baseExtras(), OrphanPipeline[int, int]{
		Headers: []string{"Value"},
		List:    func(ctx context.Context, emit func(int) error) error { return nil },
		Lists:   []func(context.Context, func(int) error) error{func(ctx context.Context, emit func(int) error) error { return nil }},
		Process: func(_ context.Context, item int) (*int, error) { return nil, nil },
		ToRow:   func(r int) []any { return []any{r} },
	})
	if err == nil {
		t.Fatal("expected error when both List and Lists are set")
	}
}

func TestRunOrphanPipeline_AssumeYesSkipsPrompt(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	out := &bytes.Buffer{}
	// Prompter returning err on Confirm would fail the test if we hit it.
	promp := &fakePrompter{err: errors.New("prompt called when --yes was set")}
	awsCmd := newTestAWSCommand(out, promp)

	globals := baseGlobals(true)
	globals.AssumeYes = true

	var deletes int
	err := runOrphanPipeline(awsCmd, context.Background(), globals, baseExtras(), OrphanPipeline[int, int]{
		Headers: []string{"Value"},
		List: func(ctx context.Context, emit func(int) error) error {
			return emit(1)
		},
		Process: func(_ context.Context, item int) (*int, error) { v := item; return &v, nil },
		ToRow:   func(r int) []any { return []any{r} },
		Delete:  func(ctx context.Context, r int) error { deletes++; return nil },
	})
	if err != nil {
		t.Fatalf("runOrphanPipeline returned error: %v", err)
	}
	if deletes != 1 {
		t.Errorf("expected 1 delete, got %d", deletes)
	}
}

func TestRunOrphanPipeline_OnErrorContinueViaGlobals(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	out := &bytes.Buffer{}
	awsCmd := newTestAWSCommand(out, &fakePrompter{answer: true})

	globals := baseGlobals(true)
	globals.OnError = "continue"

	sentinel := errors.New("delete boom")
	var deletes int
	err := runOrphanPipeline(awsCmd, context.Background(), globals, baseExtras(), OrphanPipeline[int, int]{
		Headers: []string{"Value"},
		List: func(ctx context.Context, emit func(int) error) error {
			for _, i := range []int{1, 2, 3} {
				if err := emit(i); err != nil {
					return err
				}
			}
			return nil
		},
		Process: func(_ context.Context, item int) (*int, error) { v := item; return &v, nil },
		ToRow:   func(r int) []any { return []any{r} },
		Delete: func(ctx context.Context, r int) error {
			deletes++
			return sentinel
		},
	})
	if err == nil {
		t.Fatal("expected error from continue-on-error policy")
	}
	if deletes != 3 {
		t.Errorf("expected Delete to be called 3 times (continue policy), got %d", deletes)
	}
}

func TestRunOrphanPipeline_DeleteContinueOnError(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	out := &bytes.Buffer{}
	awsCmd := newTestAWSCommand(out, &fakePrompter{answer: true})

	sentinel := errors.New("delete boom")
	var deletes int
	err := runOrphanPipeline(awsCmd, context.Background(), baseGlobals(true), baseExtras(), OrphanPipeline[int, int]{
		Headers: []string{"Value"},
		List: func(ctx context.Context, emit func(int) error) error {
			for _, i := range []int{1, 2, 3, 4} {
				if err := emit(i); err != nil {
					return err
				}
			}
			return nil
		},
		Process: func(_ context.Context, item int) (*int, error) { v := item; return &v, nil },
		ToRow:   func(r int) []any { return []any{r} },
		Delete: func(ctx context.Context, r int) error {
			deletes++
			if r%2 == 0 {
				return sentinel
			}
			return nil
		},
		DeleteErrorPolicy: DeleteContinueOnError,
	})
	if err == nil {
		t.Fatal("expected joined error from continue-on-error policy")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel to be wrapped in joined error, got %v", err)
	}
	if deletes != 4 {
		t.Errorf("expected Delete to be called 4 times, got %d", deletes)
	}
}

func TestRunOrphanPipeline_StateFileSkipsAlreadyDeleted(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	dir := t.TempDir()
	path := filepath.Join(dir, "state.txt")
	if err := os.WriteFile(path, []byte("vol-1\nvol-3\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	out := &bytes.Buffer{}
	awsCmd := newTestAWSCommand(out, &fakePrompter{answer: true})

	globals := baseGlobals(true)
	globals.AssumeYes = true
	globals.StateFile = path

	var deleted []string
	err := runOrphanPipeline(awsCmd, context.Background(), globals, baseExtras(), OrphanPipeline[string, string]{
		Headers: []string{"ID"},
		List: func(ctx context.Context, emit func(string) error) error {
			for _, v := range []string{"vol-1", "vol-2", "vol-3", "vol-4"} {
				if err := emit(v); err != nil {
					return err
				}
			}
			return nil
		},
		Process: func(_ context.Context, v string) (*string, error) { x := v; return &x, nil },
		ToRow:   func(r string) []any { return []any{r} },
		Delete: func(ctx context.Context, r string) error {
			deleted = append(deleted, r)
			return nil
		},
		DedupKey: func(r string) string { return r },
	})
	if err != nil {
		t.Fatalf("runOrphanPipeline returned error: %v", err)
	}
	if len(deleted) != 2 {
		t.Errorf("expected 2 deletes (vol-2, vol-4), got %d: %v", len(deleted), deleted)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	s := string(data)
	for _, want := range []string{"vol-1", "vol-2", "vol-3", "vol-4"} {
		if !strings.Contains(s, want) {
			t.Errorf("expected state file to contain %q, got:\n%s", want, s)
		}
	}
}

func TestRunOrphanPipeline_DryRunSkipsDeletes(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	out := &bytes.Buffer{}
	awsCmd := newTestAWSCommand(out, &fakePrompter{answer: true})

	globals := baseGlobals(true)
	globals.AssumeYes = true
	globals.DryRun = true

	var deletes int
	err := runOrphanPipeline(awsCmd, context.Background(), globals, baseExtras(), OrphanPipeline[int, int]{
		Headers: []string{"Value"},
		List: func(ctx context.Context, emit func(int) error) error {
			for _, i := range []int{1, 2, 3} {
				if err := emit(i); err != nil {
					return err
				}
			}
			return nil
		},
		Process: func(_ context.Context, item int) (*int, error) { v := item; return &v, nil },
		ToRow:   func(r int) []any { return []any{r} },
		Delete: func(ctx context.Context, r int) error {
			deletes++
			return nil
		},
	})
	if err != nil {
		t.Fatalf("runOrphanPipeline returned error: %v", err)
	}
	if deletes != 0 {
		t.Errorf("expected 0 Delete invocations under --dry-run, got %d", deletes)
	}
}

func TestRunOrphanPipeline_MetricsFileWritesSummary(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	out := &bytes.Buffer{}
	awsCmd := newTestAWSCommand(out, &fakePrompter{answer: true})

	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.json")

	globals := baseGlobals(true)
	globals.MetricsFile = path

	err := runOrphanPipeline(awsCmd, context.Background(), globals, baseExtras(), OrphanPipeline[int, int]{
		Headers:       []string{"Value"},
		ResourceLabel: "things",
		List: func(ctx context.Context, emit func(int) error) error {
			for _, i := range []int{1, 2, 3} {
				if err := emit(i); err != nil {
					return err
				}
			}
			return nil
		},
		Process: func(_ context.Context, item int) (*int, error) {
			if item == 2 {
				return nil, nil
			}
			v := item
			return &v, nil
		},
		ToRow:  func(r int) []any { return []any{r} },
		Delete: func(ctx context.Context, r int) error { return nil },
	})
	if err != nil {
		t.Fatalf("runOrphanPipeline returned error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("metrics file not written: %v", err)
	}
	var m struct {
		Resource string `json:"resource"`
		Items    int64  `json:"items"`
		Results  int64  `json:"results"`
		Deletes  int64  `json:"deletes"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("metrics file is not valid JSON: %v\n%s", err, string(data))
	}
	if m.Resource != "things" {
		t.Errorf("expected resource=things, got %q", m.Resource)
	}
	if m.Items != 3 {
		t.Errorf("expected items=3, got %d", m.Items)
	}
	if m.Results != 2 {
		t.Errorf("expected results=2 (one skipped), got %d", m.Results)
	}
	if m.Deletes != 2 {
		t.Errorf("expected deletes=2, got %d", m.Deletes)
	}
}

func TestRunOrphanPipeline_DeleteConcurrencyDeletesAll(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	out := &bytes.Buffer{}
	awsCmd := newTestAWSCommand(out, &fakePrompter{answer: true})

	var (
		mu      sync.Mutex
		seen    = map[int]bool{}
		deletes int
	)
	err := runOrphanPipeline(awsCmd, context.Background(), baseGlobals(true), baseExtras(), OrphanPipeline[int, int]{
		Headers: []string{"Value"},
		List: func(ctx context.Context, emit func(int) error) error {
			for i := 1; i <= 20; i++ {
				if err := emit(i); err != nil {
					return err
				}
			}
			return nil
		},
		Process: func(_ context.Context, item int) (*int, error) { v := item; return &v, nil },
		ToRow:   func(r int) []any { return []any{r} },
		Delete: func(ctx context.Context, r int) error {
			mu.Lock()
			seen[r] = true
			deletes++
			mu.Unlock()
			return nil
		},
		DeleteConcurrency: 5,
	})
	if err != nil {
		t.Fatalf("runOrphanPipeline returned error: %v", err)
	}
	if deletes != 20 {
		t.Errorf("expected 20 deletes, got %d", deletes)
	}
	if len(seen) != 20 {
		t.Errorf("expected 20 distinct values deleted, got %d", len(seen))
	}
}

// TestRunOrphanPipeline_DeleteReceivesRootContext verifies the rootCtx invariant:
// even when the pipeline's errgroup would have cancelled its context, Delete
// must receive a non-cancelled context so post-pipeline deletes can run.
//
// We can't easily force egCtx to be cancelled WHILE the post-Wait delete loop
// runs (by construction it's already cancelled if g.Wait() returned an error
// — and in that case we don't reach the delete loop at all). The reverse
// check is still valuable: confirm Delete's context is NOT done in the
// happy-path case.
func TestRunOrphanPipeline_DeleteReceivesRootContext(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	out := &bytes.Buffer{}
	awsCmd := newTestAWSCommand(out, &fakePrompter{answer: true})

	var deleteCtxErrs []error
	err := runOrphanPipeline(awsCmd, context.Background(), baseGlobals(true), baseExtras(), OrphanPipeline[int, int]{
		Headers: []string{"Value"},
		List: func(ctx context.Context, emit func(int) error) error {
			return emit(42)
		},
		Process: func(_ context.Context, item int) (*int, error) { v := item; return &v, nil },
		ToRow:   func(r int) []any { return []any{r} },
		Delete: func(ctx context.Context, r int) error {
			deleteCtxErrs = append(deleteCtxErrs, ctx.Err())
			return nil
		},
	})
	if err != nil {
		t.Fatalf("runOrphanPipeline returned error: %v", err)
	}
	if len(deleteCtxErrs) != 1 {
		t.Fatalf("expected Delete to be called once, got %d invocations", len(deleteCtxErrs))
	}
	if deleteCtxErrs[0] != nil {
		t.Errorf("expected Delete's ctx to be non-cancelled, got err=%v", deleteCtxErrs[0])
	}
}

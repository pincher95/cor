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
	"testing"

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

// baseFlagValues returns a flagValues map with the six global flags populated
// with sensible test defaults. Tests mutate individual keys as needed.
func baseFlagValues(delete bool) *map[string]any {
	m := map[string]any{
		"region":      "us-east-1",
		"profile":     "default",
		"auth-method": "AWS_CREDENTIALS_FILE",
		"delete":      delete,
		"sort-by":     "",
		"sort-desc":   false,
	}
	return &m
}

func TestRunOrphanPipeline_HappyPath_StreamsAllRows(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	out := &bytes.Buffer{}
	promp := &fakePrompter{answer: false}
	awsCmd := newTestAWSCommand(out, promp)

	var processed int
	var finalizeCalled bool

	err := runOrphanPipeline(awsCmd, context.Background(), baseFlagValues(false), OrphanPipeline[int, string]{
		Headers: []string{"Index", "Value"},
		List: func(ctx context.Context, emit func(int) error) error {
			for i := 1; i <= 3; i++ {
				if err := emit(i); err != nil {
					return err
				}
			}
			return nil
		},
		Process: func(_ context.Context, item int) (*string, error) {
			processed++
			s := ""
			for j := 0; j < item; j++ {
				s += "x"
			}
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
	if processed != 3 {
		t.Errorf("expected Process to be called 3 times, got %d", processed)
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

	err := runOrphanPipeline(awsCmd, context.Background(), baseFlagValues(true), OrphanPipeline[int, int]{
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

	err := runOrphanPipeline(awsCmd, context.Background(), baseFlagValues(false), OrphanPipeline[int, int]{
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

	err := runOrphanPipeline(awsCmd, context.Background(), baseFlagValues(false), OrphanPipeline[int, int]{
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

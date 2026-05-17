/*
Copyright 2024 Cloud Orphaned Resources Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"golang.org/x/sync/errgroup"
)

// runMetrics captures observable counts and timing for a single pipeline run.
type runMetrics struct {
	Resource  string    `json:"resource"`
	Items     int64     `json:"items"`
	Results   int64     `json:"results"`
	Deletes   int64     `json:"deletes"`
	ElapsedMs int64     `json:"elapsed_ms"`
	StartedAt time.Time `json:"started_at"`
}

// DeleteErrorPolicy controls how the delete phase reacts to per-item errors.
type DeleteErrorPolicy int

const (
	// DeleteStopOnFirstError aborts the delete loop on the first error.
	DeleteStopOnFirstError DeleteErrorPolicy = iota
	// DeleteContinueOnError logs each failure and keeps deleting; the pipeline
	// returns a joined error after attempting every candidate.
	DeleteContinueOnError
)

// OrphanPipeline declares per-command variation for runOrphanPipeline. Item
// is the type produced by List/Lists; Result is the command-owned struct
// returned by Process. Required: Headers, Process, ToRow, and exactly one of
// {List, Lists}.
type OrphanPipeline[Item, Result any] struct {
	Headers       []string
	ResourceLabel string
	HideIndex     bool

	// PreScan runs once before producers/workers start. Use to build shared
	// usage maps that Process consults via closure. A non-nil error aborts.
	PreScan func(ctx context.Context) error

	List  func(ctx context.Context, emit func(Item) error) error
	Lists []func(ctx context.Context, emit func(Item) error) error

	// Process returns (nil, nil) to skip an item; (nil, err) to abort.
	Process  func(ctx context.Context, item Item) (*Result, error)
	ToRow    func(r Result) []any
	Finalize func(results []Result) []any

	// Delete is invoked per result with the original (pre-errgroup) context
	// so post-Wait cancellations don't block delete API calls.
	Delete func(ctx context.Context, r Result) error

	// DeleteBatch, if non-nil, replaces per-item Delete: the full result
	// slice is passed in one call. Use for APIs that natively support bulk
	// delete (ecr.BatchDeleteImage, ec2.DeleteVpcEndpoints).
	DeleteBatch func(ctx context.Context, rs []Result) error

	DeleteConcurrency int
	DeleteErrorPolicy DeleteErrorPolicy

	// DedupKey returns a stable identity per Result. Required for --state-file
	// resumable runs; nil opts out.
	DedupKey func(r Result) string
}

// runOrphanPipeline runs the shared producer → workers → collector pattern
// that the standard-shape commands in cmd/ share. See OrphanPipeline for
// the contract.
//
// The caller retains ownership of any AWS clients — the pipeline only
// touches the spec's callbacks. `globals` carries the typed root flags
// (sort-by, sort-desc, delete, ...); `extras` carries per-command flags
// and is threaded through for future use.
//
// Implemented as a top-level function (not a method on *AWSCommand)
// because Go does not permit methods with type parameters; the
// *AWSCommand receiver is threaded as the first argument instead.
func runOrphanPipeline[Item, Result any](
	a *AWSCommand,
	ctx context.Context,
	globals *flags.GlobalFlags,
	extras *map[string]any,
	spec OrphanPipeline[Item, Result],
) error {
	rootCtx := ctx

	producers, err := resolveProducers(spec)
	if err != nil {
		return err
	}

	collectDeletes := globals.Delete && (spec.Delete != nil || spec.DeleteBatch != nil)

	metrics := runMetrics{Resource: spec.ResourceLabel, StartedAt: time.Now()}
	var itemCount, resultCount, deleteCount atomic.Int64
	defer func() {
		metrics.Items = itemCount.Load()
		metrics.Results = resultCount.Load()
		metrics.Deletes = deleteCount.Load()
		metrics.ElapsedMs = time.Since(metrics.StartedAt).Milliseconds()
		emitRunMetrics(a, globals, metrics)
	}()

	if spec.PreScan != nil {
		if err := spec.PreScan(rootCtx); err != nil {
			return err
		}
	}

	itemChan := make(chan Item, 50)
	resultChan := make(chan Result, 50)

	g, egCtx := errgroup.WithContext(ctx)

	var producerWG sync.WaitGroup
	producerWG.Add(len(producers))
	for _, p := range producers {
		g.Go(func() error {
			defer producerWG.Done()
			return p(egCtx, func(item Item) error {
				select {
				case itemChan <- item:
					itemCount.Add(1)
					return nil
				case <-egCtx.Done():
					return egCtx.Err()
				}
			})
		})
	}
	go func() {
		producerWG.Wait()
		close(itemChan)
	}()

	for range NumGoroutines {
		g.Go(func() error {
			for item := range itemChan {
				if err := egCtx.Err(); err != nil {
					return err
				}
				r, err := spec.Process(egCtx, item)
				if err != nil {
					return err
				}
				if r == nil {
					continue
				}
				select {
				case resultChan <- *r:
					resultCount.Add(1)
				case <-egCtx.Done():
					return egCtx.Err()
				}
			}
			return nil
		})
	}

	collected := make([]Result, 0)
	collectorDone := make(chan struct{})
	go func() {
		defer close(collectorDone)
		stream := printer.NewSink(globals.Format, a.Output, !spec.HideIndex, spec.Headers)
		stream.SetSort(globals.SortBy, globals.SortDesc)
		defer stream.Close()
		for r := range resultChan {
			stream.WriteRow(spec.ToRow(r)...)
			collected = append(collected, r)
		}
		if spec.Finalize != nil {
			if row := spec.Finalize(collected); row != nil {
				stream.WriteRow(row...)
			}
		}
	}()

	waitErr := g.Wait()
	close(resultChan)
	<-collectorDone
	if waitErr != nil {
		return waitErr
	}

	if spec.ResourceLabel != "" {
		a.Logger.LogInfo(fmt.Sprintf("Found %d orphaned %s", len(collected), spec.ResourceLabel), nil)
	}

	if !collectDeletes || len(collected) == 0 {
		return nil
	}

	if !globals.AssumeYes {
		confirm, cErr := confirmDelete(a.Prompter, a.Logger)
		if cErr != nil {
			return cErr
		}
		if !confirm {
			return nil
		}
	} else {
		a.Logger.LogInfo("--yes provided; proceeding without prompt", map[string]any{
			"resource":   spec.ResourceLabel,
			"candidates": len(collected),
		})
	}

	if globals.OnError == "continue" {
		spec.DeleteErrorPolicy = DeleteContinueOnError
	}

	if globals.StateFile != "" && spec.DedupKey != nil {
		done, err := readStateFile(globals.StateFile)
		if err != nil {
			a.Logger.LogError("failed to read state file", err, map[string]any{"path": globals.StateFile})
		}
		filtered := collected[:0]
		skipped := 0
		for _, r := range collected {
			if done[spec.DedupKey(r)] {
				skipped++
				continue
			}
			filtered = append(filtered, r)
		}
		if skipped > 0 {
			a.Logger.LogInfo("state-file: skipping already-deleted items", map[string]any{
				"skipped":  skipped,
				"resource": spec.ResourceLabel,
			})
		}
		collected = filtered
		if len(collected) == 0 {
			return nil
		}
	}

	if globals.DryRun {
		for _, r := range collected {
			a.Logger.LogInfo("DRY RUN would delete", map[string]any{
				"resource": spec.ResourceLabel,
				"row":      spec.ToRow(r),
			})
		}
		return nil
	}

	deleted, deletePhaseErr := runDeletePhase(rootCtx, a, spec, collected)
	deleteCount.Store(int64(len(deleted)))

	if globals.StateFile != "" && spec.DedupKey != nil && len(deleted) > 0 {
		keys := make([]string, 0, len(deleted))
		for _, r := range deleted {
			keys = append(keys, spec.DedupKey(r))
		}
		if err := appendStateFile(globals.StateFile, keys); err != nil {
			a.Logger.LogError("failed to append to state file", err, map[string]any{"path": globals.StateFile})
		}
	}

	return deletePhaseErr
}

// readStateFile parses one-key-per-line state file into a set. Missing file is
// not an error — a first run has nothing to skip.
func readStateFile(path string) (map[string]bool, error) {
	out := make(map[string]bool)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if line == "" {
			continue
		}
		out[line] = true
	}
	return out, nil
}

// appendStateFile appends the given keys (one per line) to the state file,
// creating it if missing.
func appendStateFile(path string, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	for _, k := range keys {
		if _, err := f.WriteString(k + "\n"); err != nil {
			return err
		}
	}
	return nil
}

// resolveProducers returns the producer functions to run: spec.Lists when
// non-empty, else a singleton wrapping spec.List. Exactly one must be set.
func resolveProducers[Item, Result any](spec OrphanPipeline[Item, Result]) ([]func(context.Context, func(Item) error) error, error) {
	switch {
	case len(spec.Lists) > 0 && spec.List != nil:
		return nil, fmt.Errorf("pipeline misconfigured: set exactly one of List or Lists")
	case len(spec.Lists) > 0:
		return spec.Lists, nil
	case spec.List != nil:
		return []func(context.Context, func(Item) error) error{spec.List}, nil
	default:
		return nil, fmt.Errorf("pipeline misconfigured: no List or Lists")
	}
}

// runDeletePhase invokes spec.DeleteBatch (one call) or spec.Delete (per-item,
// optionally concurrent) on every collected result, respecting
// spec.DeleteErrorPolicy. Returns the slice of successfully-deleted results
// for downstream state-file persistence.
func runDeletePhase[Item, Result any](
	rootCtx context.Context,
	a *AWSCommand,
	spec OrphanPipeline[Item, Result],
	collected []Result,
) ([]Result, error) {
	if spec.DeleteBatch != nil {
		if err := spec.DeleteBatch(rootCtx, collected); err != nil {
			a.Logger.LogError("delete failed", err, map[string]any{
				"resource": spec.ResourceLabel,
			})
			return nil, err
		}
		return collected, nil
	}

	limit := max(spec.DeleteConcurrency, 1)
	sem := make(chan struct{}, limit)
	dg, dctx := errgroup.WithContext(rootCtx)
	var (
		mu        sync.Mutex
		successes []Result
		errs      []error
		stopped   atomic.Bool
	)
	for _, r := range collected {
		if stopped.Load() {
			break
		}
		select {
		case sem <- struct{}{}:
		case <-dctx.Done():
			return successes, dctx.Err()
		}
		dg.Go(func() error {
			defer func() { <-sem }()
			if stopped.Load() {
				return nil
			}
			if err := spec.Delete(dctx, r); err != nil {
				a.Logger.LogError("delete failed", err, map[string]any{
					"resource": spec.ResourceLabel,
				})
				if spec.DeleteErrorPolicy == DeleteStopOnFirstError {
					stopped.Store(true)
					return err
				}
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return nil
			}
			mu.Lock()
			successes = append(successes, r)
			mu.Unlock()
			return nil
		})
	}
	if err := dg.Wait(); err != nil {
		return successes, err
	}
	if len(errs) > 0 {
		return successes, fmt.Errorf("%d deletes failed: %w", len(errs), errors.Join(errs...))
	}
	return successes, nil
}

// emitRunMetrics writes the summary log line and, if configured, the
// --metrics-file JSON record. Errors writing the file are logged but never
// fatal — observability should never break the actual operation.
func emitRunMetrics(a *AWSCommand, globals *flags.GlobalFlags, m runMetrics) {
	a.Logger.LogInfo("pipeline summary", map[string]any{
		"resource":   m.Resource,
		"items":      m.Items,
		"results":    m.Results,
		"deletes":    m.Deletes,
		"elapsed_ms": m.ElapsedMs,
	})
	if globals.MetricsFile == "" {
		return
	}
	f, err := os.OpenFile(globals.MetricsFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		a.Logger.LogError("failed to open metrics file", err, map[string]any{"path": globals.MetricsFile})
		return
	}
	defer func() { _ = f.Close() }()
	if err := json.NewEncoder(f).Encode(m); err != nil {
		a.Logger.LogError("failed to write metrics file", err, map[string]any{"path": globals.MetricsFile})
	}
}

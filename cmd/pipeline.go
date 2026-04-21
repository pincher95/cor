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

	"github.com/pincher95/cor/pkg/handlers/printer"
	"golang.org/x/sync/errgroup"
)

// OrphanPipeline declares per-command variation for runOrphanPipeline.
//
// Item is the item type produced by List (typically an AWS SDK type such as
// ec2types.Volume). Result is the result type produced by Process — a
// command-owned struct carrying the row cells plus whatever delete
// metadata Delete needs.
//
// Required fields: Headers, List, Process, ToRow.
// Optional: Finalize (write a footer row like totals), Delete (enables
// --delete for this command; nil makes --delete a no-op).
type OrphanPipeline[Item, Result any] struct {
	Headers []string

	// HideIndex suppresses the leading "#" index column. Default false
	// (index column shown). Commands that predate this helper's
	// streaming-table defaults may opt out via HideIndex: true.
	HideIndex bool

	// List paginates or lists the resource type. It must call emit once
	// per item; emit handles the context-aware handoff to the worker
	// pool. If emit returns a non-nil error, List must propagate it
	// (the error signals that the pipeline is shutting down).
	List func(ctx context.Context, emit func(Item) error) error

	// Process performs per-item enrichment and filtering. Return (nil,
	// nil) to skip an item without signalling an error; return
	// (nil, err) to abort the whole pipeline.
	Process func(ctx context.Context, item Item) (*Result, error)

	// ToRow converts a result into the cells of a single streamed row.
	// The returned slice length must match len(Headers).
	ToRow func(r Result) []any

	// Finalize runs after all rows are streamed; if it returns a
	// non-nil slice it is written as one final row (e.g. totals).
	// Optional.
	Finalize func(results []Result) []any

	// Delete is invoked per result during the --delete phase with the
	// original, non-errgroup context so post-g.Wait() cancellations
	// don't block the delete call. Nil disables --delete for this
	// command.
	Delete func(ctx context.Context, r Result) error
}

// runOrphanPipeline runs the shared producer → workers → collector pattern
// that the standard-shape commands in cmd/ share. See OrphanPipeline for
// the contract.
//
// The caller retains ownership of any AWS clients — the pipeline only
// touches the spec's callbacks. flagValues must carry the six base flags
// populated by flags.GetFlags (sort-by, sort-desc, delete, ...).
//
// Implemented as a top-level function (not a method on *AWSCommand)
// because Go does not permit methods with type parameters; the
// *AWSCommand receiver is threaded as the first argument instead.
func runOrphanPipeline[Item, Result any](
	a *AWSCommand,
	ctx context.Context,
	flagValues *map[string]any,
	spec OrphanPipeline[Item, Result],
) error {
	rootCtx := ctx

	collectDeletes := (*flagValues)["delete"].(bool) && spec.Delete != nil

	itemChan := make(chan Item, 50)
	resultChan := make(chan Result, 50)

	g, egCtx := errgroup.WithContext(ctx)

	// Producer: drains spec.List into itemChan.
	g.Go(func() error {
		defer close(itemChan)
		return spec.List(egCtx, func(item Item) error {
			select {
			case itemChan <- item:
				return nil
			case <-egCtx.Done():
				return egCtx.Err()
			}
		})
	})

	// Workers.
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
				case <-egCtx.Done():
					return egCtx.Err()
				}
			}
			return nil
		})
	}

	// Collector: streams rows as they arrive, keeps a slice for delete.
	collected := make([]Result, 0)
	collectorDone := make(chan struct{})
	go func() {
		defer close(collectorDone)
		stream := printer.NewStreamTable(a.Output, !spec.HideIndex, spec.Headers)
		stream.SetSort((*flagValues)["sort-by"].(string), (*flagValues)["sort-desc"].(bool))
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

	err := g.Wait()
	close(resultChan)
	<-collectorDone
	if err != nil {
		return err
	}

	if !collectDeletes || len(collected) == 0 {
		return nil
	}

	confirm, cErr := confirmDelete(a.Prompter, a.Logger)
	if cErr != nil {
		return cErr
	}
	if !confirm {
		return nil
	}
	for _, r := range collected {
		if err := spec.Delete(rootCtx, r); err != nil {
			return err
		}
	}
	return nil
}

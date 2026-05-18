/*
Copyright 2024 Cloud Orphaned Resources Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"

	"github.com/pincher95/cor/pkg/cost"
)

// baselineRecord is one orphan in a snapshot file. NDJSON-friendly: one
// record per line.
type baselineRecord struct {
	Resource string  `json:"resource"`
	Key      string  `json:"key"`
	Cost     float64 `json:"cost"`
	Row      []any   `json:"row"`
}

// writeBaseline serializes (resource, collected) into NDJSON at path.
func writeBaseline[Result any](
	path, resource string,
	collected []Result,
	dedup func(r Result) string,
	monthlyCost func(r Result) cost.USD,
	toRow func(r Result) []any,
) error {
	if dedup == nil {
		return fmt.Errorf("--save-baseline requires the command to define DedupKey")
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	enc := json.NewEncoder(f)
	for _, r := range collected {
		rec := baselineRecord{
			Resource: resource,
			Key:      dedup(r),
			Row:      toRow(r),
		}
		if monthlyCost != nil {
			rec.Cost = float64(monthlyCost(r))
		}
		if err := enc.Encode(rec); err != nil {
			return err
		}
	}
	return nil
}

// readBaseline parses an NDJSON snapshot. Missing files yield an empty map and
// no error, mirroring readStateFile — first-run callers shouldn't see a
// scary "file not found" entry in the log.
func readBaseline(path string) (map[string]baselineRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]baselineRecord{}, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	out := make(map[string]baselineRecord)
	dec := json.NewDecoder(f)
	for {
		var rec baselineRecord
		if err := dec.Decode(&rec); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		out[rec.Resource+"\x00"+rec.Key] = rec
	}
	return out, nil
}

// costDriftThreshold is the floor below which two cost values are considered
// equal in baseline diffs. Without it, recomputed sizes (e.g. CloudWatch
// StoredBytes drifting by KBs) trigger spurious "changed" entries.
const costDriftThreshold = 0.01

// diffBaseline renders added / removed / changed sections vs. the prior file
// for the given resource label.
func diffBaseline[Result any](
	out io.Writer,
	priorPath, resource string,
	collected []Result,
	dedup func(r Result) string,
	monthlyCost func(r Result) cost.USD,
) error {
	prior, err := readBaseline(priorPath)
	if err != nil {
		return err
	}
	curr := make(map[string]baselineRecord, len(collected))
	for _, r := range collected {
		rec := baselineRecord{Resource: resource, Key: dedup(r)}
		if monthlyCost != nil {
			rec.Cost = float64(monthlyCost(r))
		}
		curr[rec.Resource+"\x00"+rec.Key] = rec
	}

	var added, removed, changed []baselineRecord
	for k, c := range curr {
		p, ok := prior[k]
		if !ok {
			added = append(added, c)
			continue
		}
		if math.Abs(p.Cost-c.Cost) > costDriftThreshold {
			changed = append(changed, c)
		}
	}
	for k, p := range prior {
		if p.Resource != resource {
			continue
		}
		if _, ok := curr[k]; !ok {
			removed = append(removed, p)
		}
	}

	sortByCostDesc := func(s []baselineRecord) {
		sort.SliceStable(s, func(i, j int) bool { return s[i].Cost > s[j].Cost })
	}
	sortByCostDesc(added)
	sortByCostDesc(removed)
	sortByCostDesc(changed)

	_, _ = fmt.Fprintf(out, "\n# baseline diff: %s\n", resource)
	_, _ = fmt.Fprintf(out, "+ added:   %d\n", len(added))
	_, _ = fmt.Fprintf(out, "- removed: %d\n", len(removed))
	_, _ = fmt.Fprintf(out, "~ changed: %d\n", len(changed))
	for _, r := range added {
		_, _ = fmt.Fprintf(out, "  + %s  %s\n", r.Key, cost.USD(r.Cost))
	}
	for _, r := range removed {
		_, _ = fmt.Fprintf(out, "  - %s  %s\n", r.Key, cost.USD(r.Cost))
	}
	for _, r := range changed {
		_, _ = fmt.Fprintf(out, "  ~ %s  %s\n", r.Key, cost.USD(r.Cost))
	}
	return nil
}

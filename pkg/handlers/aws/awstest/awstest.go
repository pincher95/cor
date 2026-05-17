/*
Copyright 2024 Cloud Orphaned Resources Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package awstest provides small helpers for constructing fake AWS SDK clients
// driven by middleware. Tests stub the operations they care about; unknown
// operations fail loudly rather than calling AWS.
package awstest

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go/middleware"
)

// TestingT is the subset of testing.TB this package uses.
type TestingT interface {
	Helper()
	Fatalf(format string, args ...any)
	Errorf(format string, args ...any)
}

// Stubs maps operation names (e.g. "DescribeVolumes") to either a fixed result,
// or a func(context.Context, any) (any, error) for dynamic responses.
type Stubs map[string]any

// Calls records the operations invoked on a fake client.
type Calls struct {
	mu  sync.Mutex
	ops []string
}

// Names returns the operation names observed, in invocation order.
func (c *Calls) Names() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.ops))
	copy(out, c.ops)
	return out
}

// Count returns how many times op was invoked.
func (c *Calls) Count(op string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, name := range c.ops {
		if name == op {
			n++
		}
	}
	return n
}

func (c *Calls) record(op string) {
	c.mu.Lock()
	c.ops = append(c.ops, op)
	c.mu.Unlock()
}

// Config returns an aws.Config wired with a middleware stub that serves
// responses from `stubs` and records each call into `calls`. Pass to any
// service NewFromConfig constructor (e.g. ec2.NewFromConfig(cfg)).
//
// Pointer-typed stub results are deep-copied per invocation. AWS SDK
// middleware writes back to the returned Result (e.g. ResultMetadata),
// which would race across concurrent operations if the same instance was
// shared.
func Config(t TestingT, calls *Calls, stubs Stubs) aws.Config {
	t.Helper()
	var mu sync.Mutex
	return aws.Config{
		Region: "us-east-1",
		APIOptions: []func(*middleware.Stack) error{
			func(s *middleware.Stack) error {
				return s.Initialize.Add(middleware.InitializeMiddlewareFunc("AWSTestStub",
					func(ctx context.Context, in middleware.InitializeInput, _ middleware.InitializeHandler) (
						middleware.InitializeOutput, middleware.Metadata, error,
					) {
						mu.Lock()
						op := middleware.GetOperationName(ctx)
						if calls != nil {
							calls.record(op)
						}
						v, ok := stubs[op]
						mu.Unlock()
						if !ok {
							t.Errorf("AWS operation %q not stubbed", op)
							return middleware.InitializeOutput{}, middleware.Metadata{}, fmt.Errorf("not stubbed: %s", op)
						}
						switch f := v.(type) {
						case func(context.Context, any) (any, error):
							out, err := f(ctx, in.Parameters)
							if err != nil {
								return middleware.InitializeOutput{}, middleware.Metadata{}, err
							}
							return middleware.InitializeOutput{Result: clonePointer(out)}, middleware.Metadata{}, nil
						default:
							return middleware.InitializeOutput{Result: clonePointer(v)}, middleware.Metadata{}, nil
						}
					}), middleware.Before)
			},
		},
	}
}

func clonePointer(v any) any {
	if v == nil {
		return nil
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return v
	}
	dst := reflect.New(rv.Elem().Type())
	dst.Elem().Set(rv.Elem())
	return dst.Interface()
}

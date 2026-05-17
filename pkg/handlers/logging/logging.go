/*
Copyright 2024 Cloud Orphaned Resources Contributors.

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

package logging

import (
	"io"
	"log/slog"
	"os"
)

// Logger wraps slog for backward-compatible LogInfo/LogError calls plus
// structured key/value context.
type Logger struct {
	slog *slog.Logger
}

// NewLogger returns a Logger that writes text-format records to stdout. Kept
// as a zero-arg constructor for backward compatibility with existing call
// sites that don't have a format/out preference.
func NewLogger() *Logger {
	return NewLoggerWithFormat("text", os.Stdout)
}

// NewLoggerWithFormat builds a Logger that writes records to `out` in either
// "json" or "text" format. Unknown formats fall back to text.
func NewLoggerWithFormat(format string, out io.Writer) *Logger {
	var h slog.Handler
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if format == "json" {
		h = slog.NewJSONHandler(out, opts)
	} else {
		h = slog.NewTextHandler(out, opts)
	}
	return &Logger{slog: slog.New(h)}
}

// LogError logs an error with a message and structured context. Callers decide
// on termination — this function never calls os.Exit.
func (l *Logger) LogError(message string, err error, context map[string]any) {
	attrs := mapToAttrs(context)
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	l.slog.Error(message, attrs...)
}

// LogInfo logs an informational message with structured context.
func (l *Logger) LogInfo(message string, context map[string]any) {
	l.slog.Info(message, mapToAttrs(context)...)
}

func mapToAttrs(ctx map[string]any) []any {
	if len(ctx) == 0 {
		return nil
	}
	out := make([]any, 0, 2*len(ctx))
	for k, v := range ctx {
		out = append(out, k, v)
	}
	return out
}

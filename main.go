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
package main

import (
	"context"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"syscall"

	"github.com/felixge/fgprof"
	"github.com/pincher95/cor/cmd"
)

func main() {

	// Only enable profiling endpoints when explicitly requested.
	// This avoids surprising behavior (opening ports) for a CLI tool.
	if os.Getenv("COR_PPROF") == "1" {
		addr := os.Getenv("COR_PPROF_ADDR")
		if addr == "" {
			addr = ":6060"
		}
		http.DefaultServeMux.Handle("/debug/fgprof", fgprof.Handler())
		go func() {
			log.Println(http.ListenAndServe(addr, nil))
		}()
	}

	// Set up signal handling context
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	// Execute the root command with the context
	if err := cmd.Execute(ctx); err != nil {
		os.Exit(1)
	}
}

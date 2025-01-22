/*
Copyright © 2024 NAME HERE <EMAIL ADDRESS>
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

	http.DefaultServeMux.Handle("/debug/fgprof", fgprof.Handler())
	go func() {
		log.Println(http.ListenAndServe(":6060", nil))
	}()

	// logger := logging.NewLogger()
	// rootCmd := cmd.GetRootCommand()

	// Parse flags before setting up the context
	// rootCmd.SetArgs(os.Args[1:])
	// if err := rootCmd.ParseFlags(os.Args[1:]); err != nil {
	// 	logger.LogError("Error parsing flags", err, nil, true)
	// }

	// Retrieve the timeout value
	// timeout, err := rootCmd.PersistentFlags().GetDuration("timeout")
	// if err != nil {
	// 	logger.LogError("Error retrieving timeout flag", err, nil, true)
	// }

	// Set up signal handling context
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	// Apply timeout if specified
	// if timeout > 0 {
	// 	var cancel context.CancelFunc
	// 	ctx, cancel = context.WithTimeout(ctx, timeout)
	// 	defer cancel()
	// }

	// Execute the root command with the context
	if err := cmd.Execute(ctx); err != nil {
		os.Exit(1)
	}
}

package main

import (
	"fmt"
	"os"

	"github.com/ko-build/ko/internal/cli"
	"github.com/ko-build/ko/internal/logger"
)

func main() {
	root := cli.NewRootCmd()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		logger.Error(err.Error())
		os.Exit(1)
	}
}

package main

import (
	"fmt"
	"os"

	"github.com/puremetricsai/lumi/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

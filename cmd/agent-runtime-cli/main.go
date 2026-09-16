package main

import (
	"fmt"
	"github.com/helpin-ai/agent-runtime/internal/cli"
	"os"
)

func main() {
	if err := cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

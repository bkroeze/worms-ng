package main

import (
	"context"
	"os"

	"worms.ng/internal/debug"
)

func main() { os.Exit(debug.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr)) }

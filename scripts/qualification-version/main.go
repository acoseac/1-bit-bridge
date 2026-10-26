// Command qualification-version checks committed qualification evidence versions.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
)

func main() {
	base := flag.String("base", "", "merge base with main (required)")
	head := flag.String("head", "", "PR head (required)")
	flag.Parse()
	if *base == "" || *head == "" {
		fmt.Fprintln(os.Stderr, "base and head are required")
		os.Exit(1)
	}
	if e := qualification.CheckVersion(".", *base, *head); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	fmt.Printf("qualification version OK: %s..%s\n", *base, *head)
}

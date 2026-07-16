package main

import (
	"fmt"
	"os"

	"github.com/palgroup/palai/spikes/internal/report"
)

func main() {
	paths := os.Args[1:]
	if len(paths) == 0 {
		fmt.Fprintln(os.Stderr, "usage: report-check <report.json>...")
		os.Exit(2)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read report %s: %v\n", path, err)
			os.Exit(1)
		}
		stored, err := report.Decode(data)
		if err != nil {
			fmt.Fprintf(os.Stderr, "validate report %s: %v\n", path, err)
			os.Exit(1)
		}
		if err := stored.ValidateStored(); err != nil {
			fmt.Fprintf(os.Stderr, "validate report %s: %v\n", path, err)
			os.Exit(1)
		}
		if !stored.Passed {
			fmt.Fprintf(os.Stderr, "validate report %s: report did not pass\n", path)
			os.Exit(1)
		}
	}
	fmt.Printf("report_check=PASS files=%d\n", len(paths))
}

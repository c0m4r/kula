package main

import (
	"fmt"
	"io"
	"text/tabwriter"

	"kula/internal/collector"
)

func runDisks(out io.Writer) error {
	disks, err := collector.ListDisks()
	if err != nil {
		return err
	}
	if len(disks) == 0 {
		_, err := fmt.Fprintln(out, "No disks found.")
		return err
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "DEVICE\tID"); err != nil {
		return err
	}
	for _, disk := range disks {
		id := disk.ID
		if id == "" {
			id = "unavailable (unstable kernel name)"
		}
		if _, err := fmt.Fprintf(w, "/dev/%s\t%s\n", disk.Name, id); err != nil {
			return err
		}
	}
	return w.Flush()
}

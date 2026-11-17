package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"
)

const defaultRadarTop = 3

func runRadar(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("radar", flag.ContinueOnError)
	fs.SetOutput(stderr)
	top := fs.Int("top", defaultRadarTop, "number of hotspots to show")
	jsonOut := fs.Bool("json", false, "emit structured radar JSON")
	rootFlag := fs.String("root", ".", "project root directory")
	help, err := parseFlags(fs, args)
	if err != nil {
		return commandError(stderr, "%v", err)
	}
	if help {
		return ExitOK
	}
	if *top <= 0 {
		*top = defaultRadarTop
	}

	root, err := filepath.Abs(*rootFlag)
	if err != nil {
		return commandError(stderr, "resolve project root: %v", err)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		if err == nil {
			err = fmt.Errorf("not a directory")
		}
		return commandError(stderr, "project root %s: %v", root, err)
	}

	deps, err := newDependencies()
	if err != nil {
		return commandError(stderr, "prepare engine: %v", err)
	}
	if deps.radar == nil {
		return commandError(stderr, "radar engine is not available in this build")
	}

	ctx, stop := newCommandContext(deps.engine)
	defer stop()
	result, err := deps.radar.Scan(ctx, root, *top)
	if err != nil {
		return commandError(stderr, "run radar: %v", err)
	}

	if *jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(result); err != nil {
			return commandError(stderr, "encode radar JSON: %v", err)
		}
		return ExitOK
	}

	printRadarHuman(stdout, result)
	return ExitOK
}

func printRadarHuman(w io.Writer, result radarResult) {
	if len(result.Hotspots) == 0 {
		fmt.Fprintln(w, "No hotspots found.")
	} else {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "FILE\tRISK\tREFS\tCOMPLEXITY\tESCAPES\tREWORK\tDEAD\tDUP")
		for _, hotspot := range result.Hotspots {
			fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%t\t%t\n",
				hotspot.File, hotspot.RiskScore, hotspot.References, hotspot.Complexity,
				hotspot.Escapes, hotspot.Rework, hotspot.DeadCode, hotspot.Duplicate)
		}
		_ = tw.Flush()
	}
	for _, skipped := range result.Skipped {
		if skipped.Tool != "" {
			fmt.Fprintf(w, "skipped %s (%s): %s\n", skipped.Dimension, skipped.Tool, skipped.Reason)
		} else {
			fmt.Fprintf(w, "skipped %s: %s\n", skipped.Dimension, skipped.Reason)
		}
	}
}

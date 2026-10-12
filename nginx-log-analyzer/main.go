// Command nginx-log-analyzer streams Nginx access logs and prints a traffic
// report as a terminal table, JSON or a self-contained HTML page.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/GonService/go-projects/nginx-log-analyzer/internal/analyze"
	"github.com/GonService/go-projects/nginx-log-analyzer/internal/input"
	"github.com/GonService/go-projects/nginx-log-analyzer/internal/report"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "nginx-log-analyzer:", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	def := analyze.DefaultOptions()
	fs := flag.NewFlagSet("nginx-log-analyzer", flag.ContinueOnError)
	fs.SetOutput(stderr)
	format := fs.String("format", "table", "output format: table, json or html")
	out := fs.String("o", "", "write the report to this file instead of stdout")
	topN := fs.Int("top", def.Top, "rows in each top list")
	raw := fs.Bool("raw-paths", false, "do not collapse numeric and UUID path segments into :id")
	spikeMin := fs.Int("spike-min", def.SpikeMin, "minimum 4xx or 5xx responses in a minute to report a spike")
	spikeX := fs.Float64("spike-factor", def.SpikeX, "a spike is this many times the median errors per minute")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: nginx-log-analyzer [flags] [access.log ...]   (no files or \"-\" reads stdin; .gz is detected)")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	render, ok := map[string]func(io.Writer, analyze.Report) error{
		"table": report.Table, "json": report.JSON, "html": report.HTML,
	}[*format]
	if !ok {
		return fmt.Errorf("unknown format %q (use table, json or html)", *format)
	}

	files := fs.Args()
	if len(files) == 0 {
		files = []string{"-"}
	}
	a := analyze.New(analyze.Options{Top: *topN, Normalize: !*raw, SpikeMin: *spikeMin, SpikeX: *spikeX})
	for _, path := range files {
		rc, err := input.Open(path, stdin)
		if err != nil {
			return err
		}
		_, err = input.Feed(a, rc)
		_ = rc.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	sources := make([]string, len(files))
	for i, f := range files {
		sources[i] = f
		if f == "-" {
			sources[i] = "stdin"
		}
	}
	rep := a.Report(sources)

	if *out == "" {
		w := bufio.NewWriter(stdout)
		if err := render(w, rep); err != nil {
			return err
		}
		return w.Flush()
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	if err := render(w, rep); err != nil {
		_ = f.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "wrote %s (%d requests)\n", *out, rep.Requests)
	return nil
}

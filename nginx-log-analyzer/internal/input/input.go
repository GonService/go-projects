// Package input opens log sources (files, gzip files or stdin) and streams
// their lines into an analyzer.
package input

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"os"

	"github.com/GonService/go-projects/nginx-log-analyzer/internal/analyze"
	"github.com/GonService/go-projects/nginx-log-analyzer/internal/logparse"
)

// maxLine is the longest line accepted; longer lines count as malformed.
const maxLine = 1 << 20

// Open returns a reader for path ("-" is stdin). Gzip is detected from the
// content, not the file name, so rotated files like access.log.2.gz and
// piped `cat x.gz` both work.
func Open(path string, stdin io.Reader) (io.ReadCloser, error) {
	var src io.ReadCloser
	if path == "-" {
		src = io.NopCloser(stdin)
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		src = f
	}
	return Decompress(src)
}

// Decompress wraps r with a gzip reader when it starts with the gzip magic bytes.
func Decompress(r io.ReadCloser) (io.ReadCloser, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	magic, err := br.Peek(2)
	if err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			_ = r.Close()
			return nil, fmt.Errorf("gzip: %w", err)
		}
		return readCloser{zr, func() error { _ = zr.Close(); return r.Close() }}, nil
	}
	return readCloser{br, r.Close}, nil
}

type readCloser struct {
	io.Reader
	close func() error
}

func (rc readCloser) Close() error { return rc.close() }

// Feed parses every line of r into a. It returns the number of lines read.
func Feed(a *analyze.Analyzer, r io.Reader) (int, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	lines := 0
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		lines++
		e, err := logparse.Parse(line)
		if err != nil {
			a.Malformed()
			continue
		}
		a.Add(e)
	}
	return lines, sc.Err()
}

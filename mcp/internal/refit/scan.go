// SPDX-License-Identifier: Apache-2.0

package refit

import (
	"bufio"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

// Scanner answers the fetcher's one question that needs span start times (fetch.WindowChecker): how many spans in a
// folder of trace objects start inside a window. It reads each object line by line (one ExportTraceServiceRequest
// per line, gzip or plain) and matches startTimeUnixNano as text, so it needs none of the reader's structure and
// reads an object once.
type Scanner struct{}

var startRE = regexp.MustCompile(`"startTimeUnixNano"\s*:\s*"?([0-9]+)"?`)

// LateSpans counts the spans under dir whose start is in [start, end).
func (Scanner) LateSpans(ctx context.Context, dir string, start, end time.Time) (int, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	lo, hi := start.UnixNano(), end.UnixNano()
	n := 0
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		c, err := countStarts(filepath.Join(dir, e.Name()), lo, hi)
		if err != nil {
			return 0, err
		}
		n += c
	}
	return n, nil
}

func countStarts(path string, lo, hi int64) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<20)
	var r io.Reader = br
	if head, err := br.Peek(2); err == nil && len(head) == 2 && head[0] == 0x1f && head[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return 0, err
		}
		defer gz.Close()
		r = gz
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<30)
	n := 0
	for sc.Scan() {
		for _, m := range startRE.FindAllSubmatch(sc.Bytes(), -1) {
			t, err := strconv.ParseInt(string(m[1]), 10, 64)
			if err != nil {
				continue
			}
			if t >= lo && t < hi {
				n++
			}
		}
	}
	return n, sc.Err()
}

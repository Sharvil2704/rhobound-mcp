// SPDX-License-Identifier: Apache-2.0

package fetch

import (
	"bytes"
	"compress/gzip"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/objstore"
)

// The suffixes the reader parses (DATA.md 3.2). Every other object is refused by name.
var readable = []string{".jsonl.gz", ".json.gz", ".jsonl", ".json"}

// suffix returns the object's readable suffix, or "" when the reader would skip it.
func suffix(key string) string {
	for _, s := range readable {
		if strings.HasSuffix(key, s) && len(key) > len(s) && key[len(key)-len(s)-1] != '/' {
			return s
		}
	}
	return ""
}

func isGzip(b []byte) bool { return len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b }

// maxSniffText bounds what a sniffed head may inflate to: a few kilobytes of gzip can hold megabytes.
const maxSniffText = 64 << 10

var startRE = regexp.MustCompile(`"startTimeUnixNano"\s*:\s*"?([0-9]{1,19})[",}\s]`)

// firstStart returns the start of the first span in an object's head, inflating a gzip head as far as it goes.
func firstStart(head []byte) (time.Time, bool) {
	text := head
	if isGzip(head) {
		zr, err := gzip.NewReader(bytes.NewReader(head))
		if err != nil {
			return time.Time{}, false
		}
		// a head ends mid-stream: keep what inflated before the unexpected end
		text, _ = io.ReadAll(io.LimitReader(zr, maxSniffText))
	}
	m := startRE.FindSubmatch(text)
	if m == nil {
		return time.Time{}, false
	}
	ns, err := strconv.ParseInt(string(m[1]), 10, 64)
	if err != nil || ns <= 0 {
		return time.Time{}, false
	}
	return time.Unix(0, ns).UTC(), true
}

// tzThreshold is how far the partition minutes may run ahead of or behind the span starts before the partitions
// are read as written in another time zone. An export is late by its retry, batch and SDK delays (minutes); a
// partition in local time is off by whole hours (or quarter hours).
const tzThreshold = 45 * time.Minute

// partitionOffset is the median, over the objects whose head shows a span start, of partition minute minus span
// start. ok is false when no object shows one.
func partitionOffset(objs []*listed) (off time.Duration, n int, ok bool) {
	var offs []time.Duration
	for _, o := range objs {
		if o.hasStart && o.hasPart {
			offs = append(offs, o.part.Sub(o.start))
		}
	}
	if len(offs) == 0 {
		return 0, 0, false
	}
	sort.Slice(offs, func(i, j int) bool { return offs[i] < offs[j] })
	return offs[len(offs)/2], len(offs), true
}

// offsetHours rounds an offset to quarter hours, the granularity of time zones.
func offsetHours(d time.Duration) float64 {
	return float64(d.Round(15*time.Minute)) / float64(time.Hour)
}

func parsePartition(key string) (time.Time, bool) { return objstore.ParsePartition(key) }

// Package logbuf provides an in-memory ring buffer for harness log output.
//
// A Ring is an io.Writer that splits incoming bytes on newline and retains the
// most recent N lines along with the moment each was written. Subscribers may
// register a channel to receive each new entry as it arrives so the UI can
// stream logs over SSE without polling.
package logbuf

import (
	"bytes"
	"strings"
	"sync"
	"time"
)

// Entry is a single retained log line plus the time it was written. The Line
// field excludes the trailing newline and any \r the writer included.
type Entry struct {
	Time time.Time
	Line string
}

// defaultMaxEntries is the cap used when New is given a non-positive size.
const defaultMaxEntries = 500

// maxPartialBytes bounds an unterminated log line. Child processes can write
// arbitrary output without newlines; retaining only the newest chunk keeps the
// status page useful without allowing unbounded memory growth.
const maxPartialBytes = 64 * 1024

// Ring is a bounded line-oriented ring buffer. It is safe for concurrent use.
type Ring struct {
	mu          sync.Mutex
	max         int
	entries     []Entry
	partial     []byte
	partialTime time.Time
	subs        map[chan Entry]struct{}
	now         func() time.Time
}

// New returns a Ring that retains at most max entries. If max <= 0,
// defaultMaxEntries is used.
func New(max int) *Ring {
	if max <= 0 {
		max = defaultMaxEntries
	}
	return &Ring{
		max:  max,
		subs: make(map[chan Entry]struct{}),
		now:  time.Now,
	}
}

// Write splits p on newline boundaries, appending each complete line as an
// Entry. Bytes after the last newline are buffered until the next Write or
// Snapshot call, capped at maxPartialBytes. It always returns len(p), nil so
// callers behave like writing to a real sink (and tee writers do not abort on a
// ring drop).
func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(p) > 0 && len(r.partial) == 0 {
		r.partialTime = r.now()
	}
	r.partial = append(r.partial, p...)

	for {
		i := bytes.IndexByte(r.partial, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(r.partial[:i]), "\r")
		r.partial = r.partial[i+1:]
		e := Entry{Time: r.now(), Line: line}
		r.appendEntry(e)
		r.fanoutLocked(e)
		if len(r.partial) > 0 {
			r.partialTime = r.now()
		} else {
			r.partialTime = time.Time{}
		}
	}
	r.truncatePartialLocked()
	return len(p), nil
}

// Snapshot returns a copy of the retained entries. A trailing unterminated
// fragment is included as a final entry so the latest output is visible even
// if a process died mid-line.
func (r *Ring) Snapshot() []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Entry, 0, len(r.entries)+1)
	out = append(out, r.entries...)
	if len(r.partial) > 0 {
		out = append(out, Entry{
			Time: r.partialTime,
			Line: strings.TrimRight(string(r.partial), "\r"),
		})
	}
	return out
}

// Resize changes the retention cap. If max is <= 0 the default is used.
// Shrinking drops the oldest entries so Snapshot reflects the new cap
// immediately; growing is a no-op until more lines arrive. Subscribers are
// unaffected.
func (r *Ring) Resize(max int) {
	if max <= 0 {
		max = defaultMaxEntries
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if max == r.max {
		return
	}
	if len(r.entries) > max {
		drop := len(r.entries) - max
		r.entries = append(r.entries[:0], r.entries[drop:]...)
	}
	r.max = max
}

// Subscribe registers ch to receive each new entry. The returned cancel
// function removes the subscription. Sends are non-blocking; if ch is full,
// the entry is dropped for that subscriber.
func (r *Ring) Subscribe(ch chan Entry) (cancel func()) {
	r.mu.Lock()
	r.subs[ch] = struct{}{}
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		delete(r.subs, ch)
		r.mu.Unlock()
	}
}

// appendEntry adds e to the ring, evicting the oldest entry if full. Caller
// must hold r.mu.
func (r *Ring) appendEntry(e Entry) {
	if len(r.entries) < r.max {
		r.entries = append(r.entries, e)
		return
	}
	copy(r.entries, r.entries[1:])
	r.entries[r.max-1] = e
}

// truncatePartialLocked enforces maxPartialBytes for an unterminated line.
// Caller must hold r.mu.
func (r *Ring) truncatePartialLocked() {
	if len(r.partial) <= maxPartialBytes {
		return
	}
	drop := len(r.partial) - maxPartialBytes
	copy(r.partial, r.partial[drop:])
	r.partial = r.partial[:maxPartialBytes]
}

func (r *Ring) fanoutLocked(e Entry) {
	for ch := range r.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

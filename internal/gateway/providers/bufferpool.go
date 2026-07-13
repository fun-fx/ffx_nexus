package providers

import (
	"bytes"
	"sync"
)

// bufferPool recycles 64 KiB scratch buffers used by the SSE scanner
// across all OpenAI-compatible streaming responses. The lifetime is
// bounded to a single SSE chunk decode: Get-or-allocate, hand to
// bufio.Scanner.Buffer, let scanner mutate in place, Put back when
// the goroutine writes its DONE event. This keeps concurrent streams
// from accumulating 64 KiB each into the heap while latency-sensitive
// GC passes are running.
//
// We deliberately keep ONE pool (not per-host): the buffer carries no
// provider state, just bytes. The cap is loose — 64 KiB is fat by Go
// standard but cheap; an over-allocation here would let the pool
// absorb sudden bursts (model returning lots of tool calls in
// parallel).
var bufferPool = sync.Pool{
	New: func() any {
		// up-to-1 MiB max; 64 KiB initial capacity. Mirrors the
		// historic `scanner.Buffer(make([]byte, 0, 64*1024),
		// 1024*1024)` call sites used to inline.
		buf := make([]byte, 0, 64*1024)
		buf = buf[:0]
		return &buf
	},
}

// acquireBuffer pulls a 64 KiB scratch buffer from the pool. Caller
// MUST release with releaseBuffer when streaming is finished — even on
// the error path. Buffers are owned by the streaming goroutine, so the
// Put site is usually the goroutine exit point.
func acquireBuffer() []byte {
	return *bufferPool.Get().(*[]byte)
}

// releaseBuffer returns the buffer to the pool. We zero-length it
// instead of zeroing its contents — the goroutine that picks it up
// will overwrite with its own SSE bytes, so there's no leak risk.
// The capacity is preserved so the next Get doesn't allocate again.
func releaseBuffer(b []byte) {
	if cap(b) < 64*1024 {
		// Defensive: don't return undersized buffers; let the GC have
		// them. This branch hits only if a caller mutated capacity.
		return
	}
	b = b[:0]
	bufferPool.Put(&b)
}

// peekBufferToBytes is a tiny convenience used by SSE parsers when
// they want to compare against a literal "data: " or "[DONE]" prefix.
// It avoids an allocation on the hot path when the input buffer is
// already a []byte fetched from the pool.
func peekBufferToBytes(b []byte) []byte {
	if bytes.HasPrefix(b, []byte("data: ")) {
		return b[len("data: "):]
	}
	if bytes.HasPrefix(b, []byte("data:")) {
		return b[len("data:"):]
	}
	return b
}

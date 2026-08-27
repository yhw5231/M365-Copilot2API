package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Compile-time guarantee: streamingWriter must implement http.Flusher so the
// flush chain (captureWriter -> traceResponseWriter -> traceWriter -> here)
// reaches the real socket and SSE frames are not buffered until handler return.
var _ http.Flusher = (*streamingWriter)(nil)

func TestStreamingWriterImplementsFlusher(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &streamingWriter{ResponseWriter: rec}

	// A Flush without any prior Write must not panic, must propagate to the
	// underlying writer, and must mark the header as written (status implied).
	sw.Flush()
	if !sw.headerWritten {
		t.Fatal("Flush() should mark headerWritten")
	}
	if !rec.Flushed {
		t.Fatal("Flush() did not reach the underlying writer")
	}
}

func TestStreamingWriterFlushPropagatesToUnderlyingFlusher(t *testing.T) {
	rec := httptest.NewRecorder()
	inner := &traceWriter{ResponseWriter: rec}
	sw := &streamingWriter{ResponseWriter: inner}

	sw.Flush()
	if !rec.Flushed {
		t.Fatal("Flush() did not propagate through traceWriter to the recorder")
	}
}

func TestStreamingWriterHeaderTracking(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &streamingWriter{ResponseWriter: rec}

	sw.WriteHeader(http.StatusOK)
	if !sw.headerWritten {
		t.Fatal("WriteHeader should mark headerWritten")
	}

	sw2 := &streamingWriter{ResponseWriter: rec}
	sw2.Write([]byte("x"))
	if !sw2.headerWritten {
		t.Fatal("Write should mark headerWritten")
	}
}

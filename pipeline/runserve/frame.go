package runserve

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
)

// frameWriter serialises Frames onto the response as NDJSON, flushing each
// one so the caller sees output while the pipeline is still running rather
// than in one burst at the end. When record is set, every frame is also
// applied to it, so the run's state survives the response stream breaking —
// see registry.go.
type frameWriter struct {
	mu      sync.Mutex
	w       io.Writer
	flusher http.Flusher
	record  *runRecord
}

func (f *frameWriter) emit(kind, text string) {
	if text == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.record != nil {
		switch kind {
		case FrameLog:
			f.record.appendLog(text)
		case FrameReport:
			f.record.finish(StatusReport, text, "")
		case FrameError:
			f.record.finish(StatusError, "", text)
		}
	}
	// The response may already be gone (caller disconnected); the record
	// above still captured the frame, so a write failure past this point is
	// expected and not worth reporting.
	if err := json.NewEncoder(f.w).Encode(Frame{Kind: kind, Text: text}); err != nil {
		return
	}
	if f.flusher != nil {
		f.flusher.Flush()
	}
}

// sink adapts the writer to an io.Writer the pipeline engine can hold as its
// Stdout/Stderr. Each Write becomes one frame: the engine writes line-wise,
// so frames stay line-shaped without this having to buffer and split.
func (f *frameWriter) sink(kind string) io.Writer {
	return frameSink{fw: f, kind: kind}
}

type frameSink struct {
	fw   *frameWriter
	kind string
}

func (s frameSink) Write(p []byte) (int, error) {
	s.fw.emit(s.kind, string(p))
	return len(p), nil
}

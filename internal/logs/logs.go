// Package logs writes redacted job logs to disk and serves them by byte offset.
package logs

import (
	"bytes"
	"io"
	"os"
	"sort"
	"sync"
)

// Writer redacts secrets line by line, so a secret split across two writes is still caught.
// Secrets containing newlines are not supported.
type Writer struct {
	mu      sync.Mutex
	f       *os.File
	buf     []byte
	secrets [][]byte
}

func Create(file string, secrets []string) (*Writer, error) {
	f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, err
	}
	w := &Writer{f: f}
	for _, s := range secrets {
		if len(s) >= 4 {
			w.secrets = append(w.secrets, []byte(s))
		}
	}
	// Longest first, so a secret that contains another is replaced whole.
	sort.Slice(w.secrets, func(i, j int) bool { return len(w.secrets[i]) > len(w.secrets[j]) })
	return w, nil
}

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if i := bytes.LastIndexByte(w.buf, '\n'); i >= 0 {
		if err := w.emit(w.buf[:i+1]); err != nil {
			return 0, err
		}
		w.buf = append(w.buf[:0], w.buf[i+1:]...)
	}
	return len(p), nil
}

// Printf-style helper for server-side messages in the job log.
func (w *Writer) Line(s string) {
	w.Write([]byte("[controlplane] " + s + "\n"))
}

func (w *Writer) emit(b []byte) error {
	for _, s := range w.secrets {
		b = bytes.ReplaceAll(b, s, []byte("***"))
	}
	_, err := w.f.Write(b)
	return err
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.emit(append(w.buf, '\n'))
		w.buf = nil
	}
	return w.f.Close()
}

// Read returns up to max bytes starting at offset, and the offset to continue from.
func Read(file string, offset int64, max int) ([]byte, int64, error) {
	f, err := os.Open(file)
	if os.IsNotExist(err) {
		return nil, offset, nil
	}
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()
	buf := make([]byte, max)
	n, err := f.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return nil, offset, err
	}
	return buf[:n], offset + int64(n), nil
}

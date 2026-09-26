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
	file    *os.File
	buf     []byte
	secrets [][]byte
}

func Create(file string, secrets []string) (*Writer, error) {
	logFile, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, err
	}
	writer := &Writer{file: logFile}
	for _, secret := range secrets {
		if len(secret) >= 4 {
			writer.secrets = append(writer.secrets, []byte(secret))
		}
	}
	// Longest first, so a secret that contains another is replaced whole.
	sort.Slice(writer.secrets, func(left, right int) bool { return len(writer.secrets[left]) > len(writer.secrets[right]) })
	return writer, nil
}

func (writer *Writer) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	writer.buf = append(writer.buf, data...)
	if lastNewline := bytes.LastIndexByte(writer.buf, '\n'); lastNewline >= 0 {
		if err := writer.emit(writer.buf[:lastNewline+1]); err != nil {
			return 0, err
		}
		writer.buf = append(writer.buf[:0], writer.buf[lastNewline+1:]...)
	}
	return len(data), nil
}

// Printf-style helper for server-side messages in the job log.
func (writer *Writer) Line(message string) {
	writer.Write([]byte("[controlplane] " + message + "\n"))
}

func (writer *Writer) emit(chunk []byte) error {
	for _, secret := range writer.secrets {
		chunk = bytes.ReplaceAll(chunk, secret, []byte("***"))
	}
	_, err := writer.file.Write(chunk)
	return err
}

func (writer *Writer) Close() error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if len(writer.buf) > 0 {
		writer.emit(append(writer.buf, '\n'))
		writer.buf = nil
	}
	return writer.file.Close()
}

// Read returns up to max bytes starting at offset, and the offset to continue from.
func Read(file string, offset int64, max int) ([]byte, int64, error) {
	logFile, err := os.Open(file)
	if os.IsNotExist(err) {
		return nil, offset, nil
	}
	if err != nil {
		return nil, offset, err
	}
	defer logFile.Close()
	buf := make([]byte, max)
	bytesRead, err := logFile.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return nil, offset, err
	}
	return buf[:bytesRead], offset + int64(bytesRead), nil
}

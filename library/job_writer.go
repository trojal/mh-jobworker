package library

import (
	"os"
	"sync"
)

// jobWriter implements io.Writer and handles file writing and reader notification
type jobWriter struct {
	file *os.File
	cond *sync.Cond
}

// Write implements io.Writer
func (w *jobWriter) Write(p []byte) (n int, err error) {
	n, err = w.file.Write(p)
	if err == nil {
		w.cond.Broadcast()
	}
	return n, err
}

func newJobWriter(file *os.File, cond *sync.Cond) *jobWriter {
	return &jobWriter{
		file: file,
		cond: cond,
	}
}

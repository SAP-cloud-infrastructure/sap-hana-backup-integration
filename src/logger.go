package main

import (
	"fmt"
	"io"
	"os"
)

// Logger writes diagnostic lines to an io.Writer.
// All output goes to stderr in production; tests can redirect it.
type Logger struct {
	w io.Writer
}

// NewLogger creates a Logger writing to w. If w is nil, os.Stderr is used.
func NewLogger(w io.Writer) *Logger {
	if w == nil {
		w = os.Stderr
	}
	return &Logger{w: w}
}

func (l *Logger) Infof(format string, args ...any) {
	fmt.Fprintf(l.w, "INFO  "+format+"\n", args...)
}

func (l *Logger) Warnf(format string, args ...any) {
	fmt.Fprintf(l.w, "WARN  "+format+"\n", args...)
}

func (l *Logger) Errorf(format string, args ...any) {
	fmt.Fprintf(l.w, "ERROR "+format+"\n", args...)
}

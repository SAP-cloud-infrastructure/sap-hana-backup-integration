package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// LogLevel represents the severity level for diagnostic logging.
type LogLevel int

const (
	LogLevelDebug LogLevel = iota
	LogLevelInfo
	LogLevelWarn
	LogLevelError
)

// ParseLogLevel parses a log level string (case-insensitive).
// Accepts: debug, info, warn, warning, error. Defaults to info for unknown values.
func ParseLogLevel(s string) LogLevel {
	switch strings.ToLower(s) {
	case "debug":
		return LogLevelDebug
	case "info":
		return LogLevelInfo
	case "warn", "warning":
		return LogLevelWarn
	case "error":
		return LogLevelError
	default:
		return LogLevelInfo
	}
}

func (l LogLevel) String() string {
	switch l {
	case LogLevelDebug:
		return "debug"
	case LogLevelInfo:
		return "info"
	case LogLevelWarn:
		return "warn"
	case LogLevelError:
		return "error"
	default:
		return "info"
	}
}

// LogRotateFreq controls how often the log file is rotated.
type LogRotateFreq int

const (
	RotateNever LogRotateFreq = iota
	RotateMinute
	RotateHour
	RotateDay
)

// ParseLogRotateFreq parses a rotation frequency string (case-insensitive).
// Accepts: minute, hour, day, never. Defaults to never for unknown values.
func ParseLogRotateFreq(s string) LogRotateFreq {
	switch strings.ToLower(s) {
	case "minute":
		return RotateMinute
	case "hour":
		return RotateHour
	case "day":
		return RotateDay
	default:
		return RotateNever
	}
}

// rotateBucket returns a time-string that changes at each rotation boundary.
func rotateBucket(freq LogRotateFreq, t time.Time) string {
	switch freq {
	case RotateMinute:
		return t.UTC().Format("2006-01-02T15-04")
	case RotateHour:
		return t.UTC().Format("2006-01-02T15")
	case RotateDay:
		return t.UTC().Format("2006-01-02")
	default:
		return ""
	}
}

// logTimestampFormat is the timestamp layout for log lines.
const logTimestampFormat = "2006-01-02T15:04:05.000Z"

// Logger writes timestamped, level-filtered diagnostic lines.
// It is safe for concurrent use by multiple goroutines.
type Logger struct {
	mu         sync.Mutex
	level      LogLevel
	w          io.Writer
	filePath   string
	rotateFreq LogRotateFreq
	curBucket  string
	curFile    *os.File
}

// NewLogger creates a Logger writing to w at INFO level with no rotation.
// If w is nil, os.Stderr is used. This is the lightweight constructor for pre-config use.
func NewLogger(w io.Writer) *Logger {
	if w == nil {
		w = os.Stderr
	}
	return &Logger{
		level: LogLevelInfo,
		w:     w,
	}
}

// openLogFile opens a log file for appending, refusing to follow symlinks.
// Uses Lstat to detect symlinks before open, and O_NOFOLLOW to close the
// TOCTOU race window between the check and the open syscall.
func openLogFile(filePath string) (*os.File, error) {
	if fi, err := os.Lstat(filePath); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("log file %s is a symlink: refusing to open", filePath)
		}
	}
	return os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0640)
}

// NewFileLogger creates a Logger with full configuration.
// If filePath is "", logs are written to os.Stderr.
func NewFileLogger(filePath string, level LogLevel, rotateFreq LogRotateFreq) (*Logger, error) {
	if filePath == "" {
		return &Logger{
			level:      level,
			w:          os.Stderr,
			rotateFreq: rotateFreq,
		}, nil
	}
	// MkdirAll creates the directory with mode 0750 only if it does not already exist.
	// If the directory exists, its permissions are not changed — operators must ensure
	// the log directory is pre-created with mode 0750 (or tighter), owned by <sid>adm.
	if err := os.MkdirAll(filepath.Dir(filePath), 0750); err != nil {
		return nil, fmt.Errorf("create log directory %s: %w", filepath.Dir(filePath), err)
	}
	f, err := openLogFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("open log file %s: %w", filePath, err)
	}
	now := time.Now()
	return &Logger{
		level:      level,
		w:          f,
		filePath:   filePath,
		rotateFreq: rotateFreq,
		curBucket:  rotateBucket(rotateFreq, now),
		curFile:    f,
	}, nil
}

// Close flushes and closes any open log file. Safe to call on a stderr-backed Logger.
func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.curFile != nil {
		if err := l.curFile.Sync(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: log file sync failed: %v\n", err)
		}
		if err := l.curFile.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: log file close failed: %v\n", err)
		}
		l.curFile = nil
	}
}

// maybeRotate rotates the log file when the current time crosses a rotation boundary.
// Must be called with l.mu held.
func (l *Logger) maybeRotate(now time.Time) {
	if l.filePath == "" || l.rotateFreq == RotateNever {
		return
	}
	bucket := rotateBucket(l.rotateFreq, now)
	if bucket == l.curBucket {
		return
	}
	if l.curFile != nil {
		l.curFile.Sync()
		l.curFile.Close()
		l.curFile = nil
	}
	// Rename the active file to <filePath>.<oldBucket> before opening a fresh one.
	_ = os.Rename(l.filePath, l.filePath+"."+l.curBucket)

	f, err := openLogFile(l.filePath)
	if err != nil {
		// Fall back to stderr so logging continues even if rotation fails.
		l.w = os.Stderr
		l.curBucket = bucket
		return
	}
	l.curFile = f
	l.w = f
	l.curBucket = bucket
}

func (l *Logger) log(level LogLevel, format string, args ...any) {
	if level < l.level {
		return
	}
	now := time.Now().UTC()
	msg := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("%s %-7s : %s\n", now.Format(logTimestampFormat), level.String(), msg)

	l.mu.Lock()
	defer l.mu.Unlock()
	l.maybeRotate(now)
	fmt.Fprint(l.w, line)
}

func (l *Logger) Debugf(format string, args ...any) { l.log(LogLevelDebug, format, args...) }
func (l *Logger) Infof(format string, args ...any)  { l.log(LogLevelInfo, format, args...) }
func (l *Logger) Warnf(format string, args ...any)  { l.log(LogLevelWarn, format, args...) }
func (l *Logger) Errorf(format string, args ...any) { l.log(LogLevelError, format, args...) }

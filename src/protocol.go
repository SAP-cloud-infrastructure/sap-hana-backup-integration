package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// BackintVersion is the Backint API version this tool implements.
const BackintVersion = "1.50"

// SoftwareName is the name of this Backint implementation.
const SoftwareName = "SCI-hdbbackint-CEPH"

// SoftwareVersion is the version of this Backint implementation.
const SoftwareVersion = "0.1.0"

// timestampFormat is the ISO 8601 layout used in Backint protocol output.
const timestampFormat = "2006-01-02T15:04:05.000Z"

// ErrNotFound is returned by S3Backend methods when the requested object does not exist.
type ErrNotFound struct {
	S3Key string
}

func (e *ErrNotFound) Error() string {
	return fmt.Sprintf("not found (s3 key: %s)", e.S3Key)
}

// IsNotFound reports whether err is or wraps an ErrNotFound.
func IsNotFound(err error) bool {
	var e *ErrNotFound
	return errors.As(err, &e)
}

// ErrDeleteDenied is returned when the backend refuses a deletion request.
type ErrDeleteDenied struct {
	S3Key string
}

func (e *ErrDeleteDenied) Error() string {
	return fmt.Sprintf("delete denied (s3 key: %s)", e.S3Key)
}

// IsDeleteDenied reports whether err is or wraps an ErrDeleteDenied.
func IsDeleteDenied(err error) bool {
	var e *ErrDeleteDenied
	return errors.As(err, &e)
}

// InputLine represents a parsed line from a Backint input stream.
type InputLine struct {
	Keyword            string
	Params             []string
	OriginalLine       string
	ExternalBackupID   string
	FileName           string
	DestinationName    string
	MaxSize            int64
	IsSoftwareID       bool
	IsToolOption       bool
	ToolOptionString   string
	SoftwareIDToolName string
	SoftwareIDVersion  string
}

// InitializeOutput opens outFile for writing and returns it.
// If outFile is "" or "-", os.Stdout is returned (and the closer is a no-op).
func InitializeOutput(outFile string) (io.WriteCloser, error) {
	if outFile == "" || outFile == "-" {
		return os.Stdout, nil
	}
	f, err := os.OpenFile(outFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open output file %s: %w", outFile, err)
	}
	return f, nil
}

// WriteOutput formats and writes one protocol line to w.
func WriteOutput(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, format+"\n", args...)
}

// WriteSoftwareID writes the #SOFTWAREID identification line to w.
func WriteSoftwareID(w io.Writer) {
	WriteOutput(w, "#SOFTWAREID \"backint %s\" \"%s %s\"", BackintVersion, SoftwareName, SoftwareVersion)
}

// GenerateEBID returns the current UTC time as epoch milliseconds.
// Example: "1734105678123"
func GenerateEBID() string {
	return strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
}

// ParseInput reads and parses all lines from a Backint input stream.
func ParseInput(reader io.Reader) ([]InputLine, error) {
	var lines []InputLine
	scanner := bufio.NewScanner(reader)

	for scanner.Scan() {
		originalLine := scanner.Text()
		trimmedLine := strings.TrimSpace(originalLine)
		if trimmedLine == "" {
			continue
		}

		parsed := InputLine{OriginalLine: originalLine}
		parts := strings.Fields(trimmedLine)
		if len(parts) == 0 {
			continue
		}
		parsed.Keyword = parts[0]

		paramString := strings.TrimSpace(trimmedLine[len(parsed.Keyword):])
		var currentParam strings.Builder
		inQuote := false
		isEscaped := false

		for _, r := range paramString {
			if isEscaped {
				currentParam.WriteRune(r)
				isEscaped = false
				continue
			}
			if r == '\\' {
				isEscaped = true
				continue
			}
			if r == '"' {
				inQuote = !inQuote
				continue
			}
			if r == ' ' && !inQuote {
				if currentParam.Len() > 0 {
					parsed.Params = append(parsed.Params, currentParam.String())
					currentParam.Reset()
				}
			} else {
				currentParam.WriteRune(r)
			}
		}
		if currentParam.Len() > 0 {
			parsed.Params = append(parsed.Params, currentParam.String())
		}

		// Filter out empty tokens produced by consecutive spaces.
		var finalParams []string
		for _, p := range parsed.Params {
			if t := strings.TrimSpace(p); t != "" {
				finalParams = append(finalParams, t)
			}
		}
		parsed.Params = finalParams

		switch parsed.Keyword {
		case "#SOFTWAREID":
			parsed.IsSoftwareID = true
			if len(parsed.Params) >= 1 {
				parsed.SoftwareIDToolName = parsed.Params[0]
			}
			if len(parsed.Params) >= 2 {
				parsed.SoftwareIDVersion = parsed.Params[1]
			}
		case "#TOOLOPTION":
			parsed.IsToolOption = true
			if len(parsed.Params) > 0 {
				parsed.ToolOptionString = parsed.Params[0]
			}
		case "#PIPE", "#FILE":
			if len(parsed.Params) >= 1 {
				parsed.FileName = parsed.Params[0]
			}
			if len(parsed.Params) >= 2 {
				if n, err := strconv.ParseInt(parsed.Params[1], 10, 64); err == nil {
					parsed.MaxSize = n
				} else {
					fmt.Fprintf(os.Stderr, "WARN  ParseInput: invalid MaxSize %q for %s: %v\n",
						parsed.Params[1], parsed.FileName, err)
				}
			}
		case "#NULL":
			if len(parsed.Params) >= 1 {
				parsed.FileName = parsed.Params[0]
			}
			if len(parsed.Params) >= 2 {
				parsed.DestinationName = parsed.Params[1]
			}
		case "#EBID":
			if len(parsed.Params) >= 1 {
				parsed.ExternalBackupID = parsed.Params[0]
			}
			if len(parsed.Params) >= 2 {
				parsed.FileName = parsed.Params[1]
			}
			if len(parsed.Params) >= 3 {
				parsed.DestinationName = parsed.Params[2]
			}
		}
		lines = append(lines, parsed)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading input: %w", err)
	}
	return lines, nil
}

// FormatTimestamp formats t as an ISO 8601 string with millisecond precision.
func FormatTimestamp(t time.Time) string {
	return t.UTC().Format(timestampFormat)
}

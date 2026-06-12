package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// App owns all application state and implements the four Backint operation handlers.
// There are no package-level globals — everything flows through App.
type App struct {
	cfg         *S3Config
	s3          S3Backend
	log         *Logger
	output      io.Writer
	outMu       sync.Mutex // guards concurrent writes to output
	userID      string
	dbBackupID  string
	numObjects  string
	levelOrOp   string
	tag         string // "[SID][DB_NAME][level_or_op] " prefix for all log lines
}

// NewApp constructs an App.
func NewApp(cfg *S3Config, s3 S3Backend, log *Logger, output io.Writer,
	userID, dbBackupID, numObjects, levelOrOp string, tag string) *App {
	return &App{
		cfg:        cfg,
		s3:         s3,
		log:        log,
		output:     output,
		userID:     userID,
		dbBackupID: dbBackupID,
		numObjects: numObjects,
		levelOrOp:  levelOrOp,
		tag:        tag,
	}
}

// fileTag builds a per-file log tag "[SID][DB_NAME][backup_level] " by extracting
// DB_NAME from the segment after "/backint/" in the HANA path.
func (a *App) fileTag(hanaPath string) string {
	sid := a.userID
	if parts := strings.SplitN(a.userID, "@", 2); len(parts) == 2 {
		sid = parts[1]
	}
	dbName := "-"
	const sep = "/backint/"
	if idx := strings.Index(hanaPath, sep); idx >= 0 {
		rest := hanaPath[idx+len(sep):]
		if slashIdx := strings.Index(rest, "/"); slashIdx >= 0 {
			dbName = rest[:slashIdx]
		} else if rest != "" {
			dbName = rest
		}
	}
	return fmt.Sprintf("[%s][%s][%s] ", sid, dbName, a.levelOrOp)
}

// writeOutput writes one Backint protocol line to the output channel.
// It is safe for concurrent use by multiple goroutines.
func (a *App) writeOutput(format string, args ...any) {
	a.outMu.Lock()
	defer a.outMu.Unlock()
	WriteOutput(a.output, format, args...)
}

// handleBackup processes all #PIPE and #FILE entries from inputs.
// Returns (false, nil) if any individual file operation fails (partial failure),
// (true, nil) on full success, or (false, err) on a fatal error.
func (a *App) handleBackup(ctx context.Context, inputs []InputLine) (bool, error) {
	a.log.Infof("%sbackup start: sessionID=%s numObjects=%s",
		a.tag, a.dbBackupID, a.numObjects)

	var (
		mu    sync.Mutex
		allOK = true
		wg    sync.WaitGroup
		sem   = make(chan struct{}, a.cfg.UploadChannelSize)
	)

	for _, line := range inputs {
		if line.Keyword != "#PIPE" && line.Keyword != "#FILE" {
			if !line.IsSoftwareID && !line.IsToolOption {
				a.log.Debugf("%sbackup: skipping non-object line: %s", a.tag, line.OriginalLine)
			}
			continue
		}

		sem <- struct{}{} // acquire slot; blocks when pool is full
		wg.Add(1)
		go func(line InputLine) {
			defer wg.Done()
			defer func() { <-sem }()

			tag := a.fileTag(line.FileName)
			a.log.Debugf("%sbackup: processing %s (maxSize=%d)", tag, line.FileName, line.MaxSize)

			var src io.ReadCloser
			var openErr error
			if line.Keyword == "#FILE" {
				src, openErr = os.Open(line.FileName)
			} else { // #PIPE
				src, openErr = os.OpenFile(line.FileName, os.O_RDONLY, os.ModeNamedPipe)
			}
			if openErr != nil {
				a.writeOutput("#ERROR %s", line.FileName)
				a.log.Errorf("%sbackup: open source %s: %v", tag, line.FileName, openErr)
				mu.Lock()
				allOK = false
				mu.Unlock()
				return
			}
			defer src.Close()

			ebid := GenerateEBID()
			a.log.Debugf("%sbackup: generated EBID=%s for %s", tag, ebid, line.FileName)

			bytesUploaded, _, err := a.s3.BackupObject(ctx, line.FileName, src, ebid, tag)
			if err != nil {
				a.writeOutput("#ERROR %s", line.FileName)
				a.log.Errorf("%sbackup: upload %s (EBID %s): %v", tag, line.FileName, ebid, err)
				mu.Lock()
				allOK = false
				mu.Unlock()
				return
			}
			a.writeOutput("#SAVED \"%s\" \"%s\" %d", ebid, line.FileName, bytesUploaded)
		}(line)
	}

	wg.Wait()
	return allOK, nil
}

// handleRestore processes all #NULL and #EBID entries from inputs.
func (a *App) handleRestore(ctx context.Context, inputs []InputLine) error {
	a.log.Infof("%srestore start", a.tag)

	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
		sem      = make(chan struct{}, a.cfg.UploadChannelSize)
	)

	for _, line := range inputs {
		if line.Keyword != "#NULL" && line.Keyword != "#EBID" {
			if !line.IsSoftwareID && !line.IsToolOption {
				a.log.Debugf("%srestore: skipping non-object line: %s", a.tag, line.OriginalLine)
			}
			continue
		}

		hanaPath := line.FileName
		ebid := line.ExternalBackupID

		if hanaPath == "" {
			msg := fmt.Sprintf("missing filename for restore: %s", line.OriginalLine)
			a.writeOutput("#ERROR %s", msg)
			mu.Lock()
			if firstErr == nil {
				firstErr = fmt.Errorf("%s", msg)
			}
			mu.Unlock()
			continue
		}

		sem <- struct{}{}
		wg.Add(1)
		go func(line InputLine, hanaPath, ebid string) {
			defer wg.Done()
			defer func() { <-sem }()

			tag := a.fileTag(hanaPath)
			a.log.Debugf("%srestore: path=%s ebid=%q", tag, hanaPath, ebid)

			destPath := hanaPath
			if line.DestinationName != "" {
				destPath = line.DestinationName
			}

			var dst io.WriteCloser
			var openErr error
			fi, statErr := os.Stat(destPath)
			if statErr == nil && fi.Mode()&os.ModeNamedPipe != 0 {
				a.log.Debugf("%srestore: opening named pipe for writing: %s", tag, destPath)
				dst, openErr = os.OpenFile(destPath, os.O_WRONLY, os.ModeNamedPipe)
			} else {
				a.log.Debugf("%srestore: creating/opening file for writing: %s", tag, destPath)
				dst, openErr = os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
			}
			if openErr != nil {
				a.writeOutput("#ERROR %s", hanaPath)
				a.log.Errorf("%srestore: open destination %s: %v", tag, destPath, openErr)
				mu.Lock()
				if firstErr == nil {
					firstErr = openErr
				}
				mu.Unlock()
				return
			}
			defer dst.Close()

			_, resolvedEbid, s3Err := a.s3.RestoreObject(ctx, hanaPath, ebid, dst, tag)
			if s3Err != nil {
				if IsNotFound(s3Err) {
					a.writeOutput("#NOTFOUND %s", hanaPath)
				} else {
					a.writeOutput("#ERROR %s", hanaPath)
				}
				a.log.Errorf("%srestore: %s: %v", tag, hanaPath, s3Err)
				mu.Lock()
				if firstErr == nil {
					firstErr = s3Err
				}
				mu.Unlock()
				return
			}
			a.writeOutput("#RESTORED \"%s\" \"%s\"", resolvedEbid, hanaPath)
		}(line, hanaPath, ebid)
	}

	wg.Wait()
	return firstErr
}

// handleInquire processes inquire requests: global list, per-path list, or specific EBID.
func (a *App) handleInquire(ctx context.Context, inputs []InputLine) error {
	a.log.Infof("%sinquire start", a.tag)
	var firstErr error

	for _, line := range inputs {
		hanaPath := line.FileName
		ebid := line.ExternalBackupID
		tag := a.fileTag(hanaPath)
		a.log.Debugf("%sinquire: keyword=%s path=%q ebid=%q", tag, line.Keyword, hanaPath, ebid)

		switch {
		case line.Keyword == "#NULL" && hanaPath == "":
			// Global inquire — list all backups for this tenant.
			objects, err := a.s3.ListAllForTenant(ctx, a.userID)
			if err != nil {
				if IsNotFound(err) {
					a.writeOutput("#NOTFOUND")
				} else {
					a.writeOutput("#ERROR could not list objects for global inquire: %v", err)
				}
				a.log.Errorf("%sinquire: global list: %v", tag, err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			for _, obj := range objects {
				a.writeOutput("#BACKUP \"%s\" \"%s\" \"%s\"",
					obj.ExternalBackupID, obj.FileName, FormatTimestamp(obj.CreationDate))
			}

		case line.Keyword == "#NULL" && hanaPath != "":
			// List all versions for a specific path.
			objects, err := a.s3.ListVersionsForPath(ctx, hanaPath)
			if err != nil {
				if IsNotFound(err) {
					a.writeOutput("#NOTFOUND \"%s\"", hanaPath)
				} else {
					a.writeOutput("#ERROR %s", hanaPath)
				}
				a.log.Errorf("%sinquire: list versions for %s: %v", tag, hanaPath, err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			for _, obj := range objects {
				a.writeOutput("#BACKUP \"%s\" \"%s\" \"%s\"",
					obj.ExternalBackupID, obj.FileName, FormatTimestamp(obj.CreationDate))
			}

		case line.Keyword == "#EBID" && hanaPath != "" && ebid != "":
			// Specific version metadata.
			modTime, err := a.s3.InquireObject(ctx, hanaPath, ebid, tag)
			if err != nil {
				if IsNotFound(err) {
					a.writeOutput("#NOTFOUND \"%s\" \"%s\"", ebid, hanaPath)
				} else {
					a.writeOutput("#ERROR \"%s\" \"%s\"", ebid, hanaPath)
				}
				a.log.Errorf("%sinquire: object %s (EBID %s): %v", tag, hanaPath, ebid, err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			a.writeOutput("#BACKUP \"%s\" \"%s\" \"%s\"", ebid, hanaPath, FormatTimestamp(modTime))

		default:
			if line.IsSoftwareID || line.IsToolOption {
				continue
			}
			msg := fmt.Sprintf("invalid inquire request: %s", line.OriginalLine)
			a.writeOutput("#ERROR %s", msg)
			if firstErr == nil {
				firstErr = fmt.Errorf("%s", msg)
			}
		}
	}
	return firstErr
}

// handleDelete processes all #EBID entries from inputs.
func (a *App) handleDelete(ctx context.Context, inputs []InputLine) error {
	a.log.Infof("%sdelete start", a.tag)
	var firstErr error

	for _, line := range inputs {
		if line.Keyword != "#EBID" {
			if !line.IsSoftwareID && !line.IsToolOption {
				a.log.Debugf("%sdelete: skipping non-EBID line: %s", a.tag, line.OriginalLine)
			}
			continue
		}

		hanaPath := line.FileName
		ebid := line.ExternalBackupID

		if hanaPath == "" || ebid == "" {
			msg := fmt.Sprintf("missing EBID or filename for delete: %s", line.OriginalLine)
			a.writeOutput("#ERROR %s", msg)
			if firstErr == nil {
				firstErr = fmt.Errorf("%s", msg)
			}
			continue
		}
		tag := a.fileTag(hanaPath)
		a.log.Debugf("%sdelete: path=%s ebid=%s", tag, hanaPath, ebid)

		err := a.s3.DeleteObject(ctx, hanaPath, ebid, tag)
		if err != nil {
			if IsNotFound(err) {
				a.writeOutput("#NOTFOUND \"%s\" \"%s\"", ebid, hanaPath)
			} else if IsDeleteDenied(err) {
				a.writeOutput("#NOTDELETED \"%s\" \"%s\"", ebid, hanaPath)
			} else {
				a.writeOutput("#ERROR \"%s\" \"%s\"", ebid, hanaPath)
			}
			a.log.Errorf("%sdelete: %s (EBID %s): %v", tag, hanaPath, ebid, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		a.writeOutput("#DELETED \"%s\" \"%s\"", ebid, hanaPath)
	}
	return firstErr
}

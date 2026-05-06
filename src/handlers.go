package main

import (
	"context"
	"fmt"
	"io"
	"os"
)

// App owns all application state and implements the four Backint operation handlers.
// There are no package-level globals — everything flows through App.
type App struct {
	cfg         *S3Config
	s3          S3Backend
	log         *Logger
	output      io.Writer
	userID      string
	dbBackupID  string
	numObjects  string
	backupLevel string
}

// NewApp constructs an App.
func NewApp(cfg *S3Config, s3 S3Backend, log *Logger, output io.Writer,
	userID, dbBackupID, numObjects, backupLevel string) *App {
	return &App{
		cfg:         cfg,
		s3:          s3,
		log:         log,
		output:      output,
		userID:      userID,
		dbBackupID:  dbBackupID,
		numObjects:  numObjects,
		backupLevel: backupLevel,
	}
}

// writeOutput writes one Backint protocol line to the output channel.
func (a *App) writeOutput(format string, args ...any) {
	WriteOutput(a.output, format, args...)
}

// handleBackup processes all #PIPE and #FILE entries from inputs.
// Returns (false, nil) if any individual file operation fails (partial failure),
// (true, nil) on full success, or (false, err) on a fatal error.
func (a *App) handleBackup(ctx context.Context, inputs []InputLine) (bool, error) {
	a.log.Infof("backup start: sessionID=%s numObjects=%s level=%s",
		a.dbBackupID, a.numObjects, a.backupLevel)

	allOK := true
	for _, line := range inputs {
		if line.Keyword != "#PIPE" && line.Keyword != "#FILE" {
			if !line.IsSoftwareID && !line.IsToolOption {
				a.log.Infof("backup: skipping non-object line: %s", line.OriginalLine)
			}
			continue
		}
		a.log.Infof("backup: processing %s (maxSize=%d)", line.FileName, line.MaxSize)

		var src io.ReadCloser
		var openErr error
		if line.Keyword == "#FILE" {
			src, openErr = os.Open(line.FileName)
		} else { // #PIPE
			src, openErr = os.OpenFile(line.FileName, os.O_RDONLY, os.ModeNamedPipe)
		}
		if openErr != nil {
			a.writeOutput("#ERROR %s", line.FileName)
			a.log.Errorf("backup: open source %s: %v", line.FileName, openErr)
			allOK = false
			continue
		}

		func() {
			defer src.Close()
			ebid := GenerateEBID()
			a.log.Infof("backup: generated EBID=%s for %s", ebid, line.FileName)

			bytesUploaded, _, err := a.s3.BackupObject(ctx, line.FileName, src, ebid)
			if err != nil {
				a.writeOutput("#ERROR %s", line.FileName)
				a.log.Errorf("backup: upload %s (EBID %s): %v", line.FileName, ebid, err)
				allOK = false
				return
			}
			a.writeOutput("#SAVED \"%s\" \"%s\" %d", ebid, line.FileName, bytesUploaded)
		}()
	}
	return allOK, nil
}

// handleRestore processes all #NULL and #EBID entries from inputs.
func (a *App) handleRestore(ctx context.Context, inputs []InputLine) error {
	a.log.Infof("restore start")
	var firstErr error

	for _, line := range inputs {
		if line.Keyword != "#NULL" && line.Keyword != "#EBID" {
			if !line.IsSoftwareID && !line.IsToolOption {
				a.log.Infof("restore: skipping non-object line: %s", line.OriginalLine)
			}
			continue
		}

		hanaPath := line.FileName
		ebid := line.ExternalBackupID

		if hanaPath == "" {
			msg := fmt.Sprintf("missing filename for restore: %s", line.OriginalLine)
			a.writeOutput("#ERROR %s", msg)
			if firstErr == nil {
				firstErr = fmt.Errorf("%s", msg)
			}
			continue
		}
		a.log.Infof("restore: path=%s ebid=%q", hanaPath, ebid)

		destPath := hanaPath
		if line.DestinationName != "" {
			destPath = line.DestinationName
		}

		var dst io.WriteCloser
		var openErr error
		fi, statErr := os.Stat(destPath)
		if statErr == nil && fi.Mode()&os.ModeNamedPipe != 0 {
			a.log.Infof("restore: opening named pipe for writing: %s", destPath)
			dst, openErr = os.OpenFile(destPath, os.O_WRONLY, os.ModeNamedPipe)
		} else {
			a.log.Infof("restore: creating/opening file for writing: %s", destPath)
			dst, openErr = os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		}
		if openErr != nil {
			a.writeOutput("#ERROR %s", hanaPath)
			a.log.Errorf("restore: open destination %s: %v", destPath, openErr)
			if firstErr == nil {
				firstErr = openErr
			}
			continue
		}

		err := func() error {
			defer dst.Close()
			_, resolvedEbid, s3Err := a.s3.RestoreObject(ctx, hanaPath, ebid, dst)
			if s3Err != nil {
				if IsNotFound(s3Err) {
					a.writeOutput("#NOTFOUND %s", hanaPath)
				} else {
					a.writeOutput("#ERROR %s", hanaPath)
				}
				a.log.Errorf("restore: %s: %v", hanaPath, s3Err)
				return s3Err
			}
			a.writeOutput("#RESTORED \"%s\" \"%s\"", resolvedEbid, hanaPath)
			return nil
		}()
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// handleInquire processes inquire requests: global list, per-path list, or specific EBID.
func (a *App) handleInquire(ctx context.Context, inputs []InputLine) error {
	a.log.Infof("inquire start")
	var firstErr error

	for _, line := range inputs {
		hanaPath := line.FileName
		ebid := line.ExternalBackupID
		a.log.Infof("inquire: keyword=%s path=%q ebid=%q", line.Keyword, hanaPath, ebid)

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
				a.log.Errorf("inquire: global list: %v", err)
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
				a.log.Errorf("inquire: list versions for %s: %v", hanaPath, err)
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
			modTime, err := a.s3.InquireObject(ctx, hanaPath, ebid)
			if err != nil {
				if IsNotFound(err) {
					a.writeOutput("#NOTFOUND \"%s\" \"%s\"", ebid, hanaPath)
				} else {
					a.writeOutput("#ERROR \"%s\" \"%s\"", ebid, hanaPath)
				}
				a.log.Errorf("inquire: object %s (EBID %s): %v", hanaPath, ebid, err)
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
	a.log.Infof("delete start")
	var firstErr error

	for _, line := range inputs {
		if line.Keyword != "#EBID" {
			if !line.IsSoftwareID && !line.IsToolOption {
				a.log.Infof("delete: skipping non-EBID line: %s", line.OriginalLine)
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
		a.log.Infof("delete: path=%s ebid=%s", hanaPath, ebid)

		err := a.s3.DeleteObject(ctx, hanaPath, ebid)
		if err != nil {
			if IsNotFound(err) {
				a.writeOutput("#NOTFOUND \"%s\" \"%s\"", ebid, hanaPath)
			} else if IsDeleteDenied(err) {
				a.writeOutput("#NOTDELETED \"%s\" \"%s\"", ebid, hanaPath)
			} else {
				a.writeOutput("#ERROR \"%s\" \"%s\"", ebid, hanaPath)
			}
			a.log.Errorf("delete: %s (EBID %s): %v", hanaPath, ebid, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		a.writeOutput("#DELETED \"%s\" \"%s\"", ebid, hanaPath)
	}
	return firstErr
}


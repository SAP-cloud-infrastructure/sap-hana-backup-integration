//

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// parseSID extracts the SID from a user flag value.
// HANA passes either plain "SID" or "DBNAME@SID" per the Backint spec.
// Returns the SID in both cases.
func parseSID(userID string) string {
	if parts := strings.SplitN(userID, "@", 2); len(parts) == 2 {
		return parts[1]
	}
	return userID
}

func main() {
	// CLI flags — local variables, no package globals.
	var (
		userIDArg      string
		functionArg    string
		paramFileArg   string
		inputFileArg   string
		outputFileArg  string
		dbBackupIDArg  string
		numObjectsArg  string
		backupLevelArg string
		versionShort   bool
		versionDetail  bool
	)

	flag.StringVar(&userIDArg, "u", "", "User ID (DBNAME@SID).")
	flag.StringVar(&functionArg, "f", "", "Function: backup, restore, inquire, delete. Mandatory.")
	flag.StringVar(&paramFileArg, "p", "", "Path to S3 parameter file. Mandatory.")
	flag.StringVar(&inputFileArg, "i", "-", "Input file path ('-' for stdin).")
	flag.StringVar(&outputFileArg, "o", "-", "Output file path ('-' for stdout).")
	flag.StringVar(&dbBackupIDArg, "s", "", "Database backup ID / HANA session ID (informational).")
	flag.StringVar(&numObjectsArg, "c", "", "Number of objects in backup (informational).")
	flag.StringVar(&backupLevelArg, "l", "", "Backup level (informational).")
	flag.BoolVar(&versionShort, "v", false, "Print short version and exit.")
	flag.BoolVar(&versionDetail, "V", false, "Print detailed version and exit.")
	flag.Parse()

	if versionShort {
		fmt.Printf("backint %s %s %s\n", BackintVersion, SoftwareName, SoftwareVersion)
		os.Exit(0)
	}
	if versionDetail {
		fmt.Printf("backint %s %s %s\n", BackintVersion, SoftwareName, SoftwareVersion)
		fmt.Println("SAP HANA Backint for S3-compatible storage (CEPH/SCI).")
		os.Exit(0)
	}

	// Set up output channel first so protocol errors can be written.
	out, err := InitializeOutput(outputFileArg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR failed to open output %s: %v\n", outputFileArg, err)
		os.Exit(1)
	}
	if f, ok := out.(*os.File); ok && f != os.Stdout {
		defer func() { f.Sync(); f.Close() }()
	}

	if functionArg == "" {
		WriteOutput(out, "#ERROR Function -f not specified.")
		fmt.Fprintln(os.Stderr, "ERROR -f <function> is mandatory.")
		os.Exit(1)
	}
	if paramFileArg == "" {
		WriteOutput(out, "#ERROR Parameter file -p not specified.")
		fmt.Fprintln(os.Stderr, "ERROR -p <param_file> is mandatory.")
		os.Exit(1)
	}

	// Load config early (before reading input) so the log file is created as
	// soon as possible, allowing all subsequent errors to be written there.
	cfg, err := LoadS3Config(paramFileArg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR failed to load S3 config from %s: %v\n", paramFileArg, err)
		WriteOutput(out, "#ERROR failed to load S3 config: %v", err)
		os.Exit(1)
	}

	log, err := NewFileLogger(
		cfg.LogFile,
		ParseLogLevel(cfg.LogLevel),
		ParseLogRotateFreq(cfg.LogRotateFreq),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR could not open log file %s: %v\n", cfg.LogFile, err)
		WriteOutput(out, "#ERROR could not open log file %s: %v", cfg.LogFile, err)
		os.Exit(1)
	}
	defer log.Close()

	// Build session tag for logging: [SID][-][backup_level]
	// Done immediately after logger init so every subsequent log line carries the tag.
	sessionSID := parseSID(userIDArg)
	levelOrOp := strings.ToUpper(functionArg)
	if backupLevelArg != "" {
		levelOrOp = backupLevelArg
	}
	sessionTag := fmt.Sprintf("[%s][-][%s] ", sessionSID, levelOrOp)

	if userIDArg == "" {
		log.Warnf("%s-u not specified; HANA usually provides this", sessionTag)
	}
	if strings.EqualFold(functionArg, "inquire") && userIDArg == "" {
		WriteOutput(out, "#ERROR -u <user_id> is required for inquire")
		log.Errorf("%s-u is required for inquire to scope the tenant namespace", sessionTag)
		os.Exit(1)
	}
	if dbBackupIDArg == "" && strings.EqualFold(functionArg, "backup") {
		log.Warnf("%s-s not specified; HANA session ID not available", sessionTag)
	}

	// Open input source.
	var inputReader io.Reader = os.Stdin
	if inputFileArg != "" && inputFileArg != "-" {
		f, err := os.Open(inputFileArg)
		if err != nil {
			WriteOutput(out, "#ERROR failed to open input file %s: %v", inputFileArg, err)
			log.Errorf("%sopen input file %s: %v", sessionTag, inputFileArg, err)
			os.Exit(1)
		}
		defer f.Close()
		inputReader = f
	}

	parsedInputs, err := ParseInput(inputReader)
	if err != nil {
		WriteOutput(out, "#ERROR failed to parse input: %v", err)
		log.Errorf("%sparse input: %v", sessionTag, err)
		os.Exit(1)
	}

	// Extract DB_NAME from the first file path in the input.
	// Each hdbbackint invocation is scoped to a single database, so the first
	// file path's /backint/<DB_NAME>/ segment identifies the database for all lines.
	// Done here — before TOOLOPTION processing — so that error log lines from
	// TOOLOPTION validation carry the full [SID][DB_NAME][level] tag rather than
	// the placeholder [SID][-][level].
	sessionDBName := "-"
	const backintSep = "/backint/"
	for _, pi := range parsedInputs {
		if pi.FileName == "" {
			continue
		}
		if idx := strings.Index(pi.FileName, backintSep); idx >= 0 {
			rest := pi.FileName[idx+len(backintSep):]
			if slashIdx := strings.Index(rest, "/"); slashIdx >= 0 {
				sessionDBName = rest[:slashIdx]
			} else if rest != "" {
				sessionDBName = rest
			}
		}
		if sessionDBName != "-" {
			break
		}
	}
	sessionTag = fmt.Sprintf("[%s][%s][%s] ", sessionSID, sessionDBName, levelOrOp)

	// Scan for #TOOLOPTION overrides. Last occurrence wins.
	//
	// Two formats are supported:
	//
	// Format 1 — PARAMETER_FILE=<path>
	//   Discards all values from the -p file and reloads from the given file.
	//   The already-open log file is intentionally kept — log_file, log_level,
	//   and log_rotate_frequency from the original -p file remain in effect for
	//   the entire process lifetime. This is by design: the log file must be open
	//   before input is parsed (so startup errors are captured), and reopening it
	//   mid-run would split the session log across two files.
	//
	// Format 2 — key=value;key=value;...
	//   Applies the listed key=value pairs on top of the -p values. Log fields are
	//   warned and skipped. Unknown keys or invalid values are a hard error.
	for _, pi := range parsedInputs {
		if !pi.IsToolOption {
			continue
		}
		ts := pi.ToolOptionString
		if ts == "" {
			log.Warnf("%s#TOOLOPTION is empty; ignoring", sessionTag)
			continue
		}

		const pfKey = "PARAMETER_FILE="
		if strings.HasPrefix(ts, pfKey) {
			// Format 1: file override.
			path := strings.TrimSpace(strings.TrimPrefix(ts, pfKey))
			if path == "" {
				WriteOutput(out, "#ERROR #TOOLOPTION PARAMETER_FILE value is empty")
				log.Errorf("%s#TOOLOPTION PARAMETER_FILE value is empty", sessionTag)
				os.Exit(1)
			}
			if _, statErr := os.Stat(path); statErr != nil {
				WriteOutput(out, "#ERROR #TOOLOPTION PARAMETER_FILE not accessible: %v", statErr)
				log.Errorf("%s#TOOLOPTION PARAMETER_FILE not accessible %s: %v", sessionTag, path, statErr)
				os.Exit(1)
			}
			log.Infof("%s#TOOLOPTION PARAMETER_FILE override: %s", sessionTag, path)
			overrideCfg, cfgErr := LoadS3Config(path)
			if cfgErr != nil {
				WriteOutput(out, "#ERROR #TOOLOPTION failed to load PARAMETER_FILE %s: %v", path, cfgErr)
				log.Errorf("%s#TOOLOPTION load PARAMETER_FILE %s: %v", sessionTag, path, cfgErr)
				os.Exit(1)
			}
			// Preserve log fields from the original -p config since the logger is already open.
			overrideCfg.LogFile = cfg.LogFile
			overrideCfg.LogLevel = cfg.LogLevel
			overrideCfg.LogRotateFreq = cfg.LogRotateFreq
			cfg = overrideCfg
		} else {
			// Format 2: inline key=value overrides.
			log.Infof("%s#TOOLOPTION inline override: %s", sessionTag, ts)
			warnf := func(format string, args ...any) {
				log.Warnf("%s"+format, append([]any{sessionTag}, args...)...)
			}
			if applyErr := ApplyInlineOverrides(cfg, ts, warnf); applyErr != nil {
				WriteOutput(out, "#ERROR %v", applyErr)
				log.Errorf("%s%v", sessionTag, applyErr)
				os.Exit(1)
			}
		}
	}

	// Extract db_version from the #SOFTWAREID line sent by HANA.
	dbVersion := ""
	for _, pi := range parsedInputs {
		if pi.IsSoftwareID && pi.SoftwareIDVersion != "" {
			dbVersion = pi.SoftwareIDVersion
		}
	}

	// Log input metadata.
	for _, pi := range parsedInputs {
		if pi.IsSoftwareID {
			log.Infof("%sreceived #SOFTWAREID from HANA: %q %q", sessionTag, pi.SoftwareIDToolName, pi.SoftwareIDVersion)
		}
		if pi.IsToolOption {
			log.Infof("%sreceived #TOOLOPTION from HANA: %s", sessionTag, pi.ToolOptionString)
		}
	}
	if dbVersion != "" {
		log.Debugf("%sHANA db_version for tagging: %s", sessionTag, dbVersion)
	}

	s3Client, err := NewS3Client(cfg, log, dbVersion, sessionTag, sessionSID)
	if err != nil {
		WriteOutput(out, "#ERROR failed to initialize S3 client: %v", err)
		log.Errorf("init S3 client: %v", err)
		os.Exit(1)
	}

	WriteSoftwareID(out)

	app := NewApp(cfg, s3Client, log, out, userIDArg, dbBackupIDArg, numObjectsArg, levelOrOp, sessionTag)
	ctx := context.Background()
	exitCode := 0

	switch strings.ToLower(functionArg) {
	case "backup":
		ok, err := app.handleBackup(ctx, parsedInputs)
		if err != nil {
			log.Errorf("%sbackup fatal: %v", sessionTag, err)
			exitCode = 1
		} else if !ok {
			exitCode = 1
		}
	case "restore":
		if err := app.handleRestore(ctx, parsedInputs); err != nil {
			log.Errorf("%srestore error: %v", sessionTag, err)
			exitCode = 1
		}
	case "inquire":
		if err := app.handleInquire(ctx, parsedInputs); err != nil {
			log.Errorf("%sinquire error: %v", sessionTag, err)
			exitCode = 1
		}
	case "delete":
		if err := app.handleDelete(ctx, parsedInputs); err != nil {
			log.Errorf("%sdelete error: %v", sessionTag, err)
			exitCode = 1
		}
	default:
		WriteOutput(out, "#ERROR unknown function: %s", functionArg)
		log.Errorf("%sunknown function: %s", sessionTag, functionArg)
		exitCode = 1
	}

	log.Infof("%sexiting with code %d", sessionTag, exitCode)
	os.Exit(exitCode)
}

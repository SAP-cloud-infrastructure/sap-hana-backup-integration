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
	sessionSID := userIDArg
	if parts := strings.SplitN(userIDArg, "@", 2); len(parts) == 2 {
		sessionSID = parts[1]
	}
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

	// Scan for #TOOLOPTION PARAMETER_FILE= override. Last occurrence wins.
	// If overridden, only the S3 connection parameters are reloaded (bucket, region,
	// endpoint, credentials, upload tuning, SSE-KMS, tagging, retries).
	// The already-open log file is intentionally kept — log_file, log_level, and
	// log_rotate_frequency from the original -p file remain in effect for the entire
	// process lifetime. This is by design: the log file must be open before input is
	// parsed (so startup errors are captured), and reopening it mid-run would split
	// the session log across two files, complicating troubleshooting.
	for _, pi := range parsedInputs {
		if !pi.IsToolOption {
			continue
		}
		const pfKey = "PARAMETER_FILE="
		if strings.HasPrefix(pi.ToolOptionString, pfKey) {
			val := strings.TrimSpace(strings.TrimPrefix(pi.ToolOptionString, pfKey))
			if val != "" {
				log.Infof("%s#TOOLOPTION PARAMETER_FILE override: %s", sessionTag, val)
				paramFileArg = val
				overrideCfg, cfgErr := LoadS3Config(paramFileArg)
				if cfgErr != nil {
					WriteOutput(out, "#ERROR failed to load overridden S3 config: %v", cfgErr)
					log.Errorf("%sload overridden S3 config from %s: %v", sessionTag, paramFileArg, cfgErr)
					os.Exit(1)
				}
				cfg = overrideCfg
			} else {
				log.Warnf("%s#TOOLOPTION PARAMETER_FILE is empty; keeping -p value", sessionTag)
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

	// Extract DB_NAME from the first file path in the input.
	// Each hdbbackint invocation is scoped to a single database, so the first
	// file path's /backint/<DB_NAME>/ segment identifies the database for all lines.
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

	app := NewApp(cfg, s3Client, log, out, userIDArg, dbBackupIDArg, numObjectsArg, backupLevelArg, sessionTag)
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

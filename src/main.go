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

	log := NewLogger(os.Stderr)

	if userIDArg == "" {
		log.Warnf("-u not specified; HANA usually provides this")
	}
	if strings.EqualFold(functionArg, "inquire") && userIDArg == "" {
		WriteOutput(out, "#ERROR -u <user_id> is required for inquire")
		log.Errorf("-u is required for inquire to scope the tenant namespace")
		os.Exit(1)
	}
	if dbBackupIDArg == "" && strings.EqualFold(functionArg, "backup") {
		log.Warnf("-s not specified; HANA session ID not available")
	}

	cfg, err := LoadS3Config(paramFileArg)
	if err != nil {
		WriteOutput(out, "#ERROR failed to load S3 config: %v", err)
		log.Errorf("load S3 config from %s: %v", paramFileArg, err)
		os.Exit(1)
	}

	s3Client, err := NewS3Client(cfg, log)
	if err != nil {
		WriteOutput(out, "#ERROR failed to initialize S3 client: %v", err)
		log.Errorf("init S3 client: %v", err)
		os.Exit(1)
	}

	// Open input source.
	var inputReader io.Reader = os.Stdin
	if inputFileArg != "" && inputFileArg != "-" {
		f, err := os.Open(inputFileArg)
		if err != nil {
			WriteOutput(out, "#ERROR failed to open input file %s: %v", inputFileArg, err)
			log.Errorf("open input file %s: %v", inputFileArg, err)
			os.Exit(1)
		}
		defer f.Close()
		inputReader = f
	}

	parsedInputs, err := ParseInput(inputReader)
	if err != nil {
		WriteOutput(out, "#ERROR failed to parse input: %v", err)
		log.Errorf("parse input: %v", err)
		os.Exit(1)
	}

	// Log any SOFTWAREID / TOOLOPTION lines sent by HANA.
	for _, pi := range parsedInputs {
		if pi.IsSoftwareID {
			log.Infof("received #SOFTWAREID from HANA: %q %q", pi.SoftwareIDToolName, pi.SoftwareIDVersion)
		}
		if pi.IsToolOption {
			log.Infof("received #TOOLOPTION from HANA: %s", pi.ToolOptionString)
		}
	}

	WriteSoftwareID(out)

	app := NewApp(cfg, s3Client, log, out, userIDArg, dbBackupIDArg, numObjectsArg, backupLevelArg)
	ctx := context.Background()
	exitCode := 0

	switch strings.ToLower(functionArg) {
	case "backup":
		ok, err := app.handleBackup(ctx, parsedInputs)
		if err != nil {
			log.Errorf("backup fatal: %v", err)
			exitCode = 1
		} else if !ok {
			exitCode = 1
		}
	case "restore":
		if err := app.handleRestore(ctx, parsedInputs); err != nil {
			log.Errorf("restore error: %v", err)
			exitCode = 1
		}
	case "inquire":
		if err := app.handleInquire(ctx, parsedInputs); err != nil {
			log.Errorf("inquire error: %v", err)
			exitCode = 1
		}
	case "delete":
		if err := app.handleDelete(ctx, parsedInputs); err != nil {
			log.Errorf("delete error: %v", err)
			exitCode = 1
		}
	default:
		WriteOutput(out, "#ERROR unknown function: %s", functionArg)
		log.Errorf("unknown function: %s", functionArg)
		exitCode = 1
	}

	log.Infof("exiting with code %d", exitCode)
	os.Exit(exitCode)
}

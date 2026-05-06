package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// S3Config holds the configuration parameters for connecting to S3.
type S3Config struct {
	Endpoint         string
	AccessKey        string
	SecretKey        string
	BucketName       string
	Region           string
	FolderName       string // New: Optional top-level folder in the bucket
	S3ForcePathStyle bool
}

// LoadS3Config parses the parameter file.
func LoadS3Config(filePath string) (*S3Config, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open parameter file %s: %w", filePath, err)
	}
	defer file.Close()

	config := &S3Config{}
	scanner := bufio.NewScanner(file)
	lineNumber := 0

	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			fmt.Fprintf(os.Stderr, "Warning: malformed line %d in parameter file %s: %s\n", lineNumber, filePath, line)
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		switch key {
		case "SCI_endpoint":
			config.Endpoint = value
		case "SCI_accessKey":
			config.AccessKey = value
		case "SCI_secretKey":
			config.SecretKey = value
		case "SCI_bucketName":
			config.BucketName = value
		case "SCI_region":
			config.Region = value
		case "SCI_folderName":
			config.FolderName = strings.Trim(value, "/")
		case "SCI_s3ForcePathStyle":
			config.S3ForcePathStyle = (strings.ToLower(value) == "true")
		default:
			fmt.Fprintf(os.Stderr, "Warning: unknown key '%s' in parameter file %s\n", key, filePath)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading parameter file %s: %w", filePath, err)
	}

	// Validate required fields
	if config.Endpoint == "" {
		return nil, fmt.Errorf("SCI_endpoint is not set in parameter file %s", filePath)
	}
	if config.AccessKey == "" {
		return nil, fmt.Errorf("SCI_accessKey is not set in parameter file %s", filePath)
	}
	if config.SecretKey == "" {
		return nil, fmt.Errorf("SCI_secretKey is not set in parameter file %s", filePath)
	}
	if config.BucketName == "" {
		return nil, fmt.Errorf("SCI_bucketName is not set in parameter file %s", filePath)
	}
	if config.Region == "" {
		fmt.Fprintf(os.Stderr, "Warning: SCI_region is not set in parameter file %s. This might be required by your S3 provider.\n", filePath)
	}
	if config.FolderName != "" {
		fmt.Fprintf(os.Stderr, "Info: Using S3 folder prefix: %s\n", config.FolderName)
	}

	return config, nil
}

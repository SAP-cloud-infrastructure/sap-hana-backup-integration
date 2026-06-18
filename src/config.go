package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// S3Config holds the configuration parameters for connecting to S3.
type S3Config struct {
	// Mandatory fields — startup fails if any of these are absent.
	Endpoint   string
	AccessKey  string
	SecretKey  string
	BucketName string
	Region     string

	// Optional fields — defaults are applied by LoadS3Config when not present.
	EndpointTemplate  string // template for deriving endpoint from region; default: see defaultEndpointTemplate
	FolderName        string // top-level folder prefix inside the bucket
	S3ForcePathStyle  bool   // default: false
	LogLevel          string // default: "info" (only when LogFile is set; "" when logging to stderr)
	LogFile           string // default: "" (stderr)
	LogRotateFreq     string // default: "never"
	ShortenFolderPath bool   // default: false
	Retries           int    // default: 3, min: 0
	Tagging           bool   // default: false
	ObjectTags        string // comma-separated key=value custom tags; max 5
	UploadPartSize    int64  // bytes; default: 134217728 (128 MiB); range: [5 MiB, 256 MiB]
	UploadConcurrency int    // default: 32; range: [1, 200]
	UploadChannelSize int    // default: 10; range: [1, 32]
	SSEEnabled        bool   // default: false; enables per-object SSE-KMS encryption on backup
	SSEKMSKeyID       string // Barbican secret UUID; required when SSEEnabled=true
}

// defaultEndpointTemplate is the default template used to derive the S3 endpoint
// from a region when SCI_endpoint is not set. The placeholder {region} is replaced
// with the resolved region value. Operators can override this via SCI_endpoint_template
// in the parameter file to support other S3-compatible storage backends.
//
// NOTE: This value is a generic placeholder. Replace it with the actual endpoint
// template for your S3-compatible storage before building for production use.
// Example: "https://s3.{region}.your-storage-provider.com"
const defaultEndpointTemplate = "https://s3.{region}.example.com"

// validateEndpointScheme checks that the endpoint uses https.
// http is allowed only for localhost and 127.0.0.1 (local dev/testing).
func validateEndpointScheme(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("endpoint %q is not a valid URL: %w", endpoint, err)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" {
		host := u.Hostname()
		if host == "localhost" || host == "127.0.0.1" {
			return nil
		}
		return fmt.Errorf("endpoint %q uses http — only https is allowed (http permitted for localhost/127.0.0.1 only)", endpoint)
	}
	return fmt.Errorf("endpoint %q has unsupported scheme %q — use https", endpoint, u.Scheme)
}

// LoadS3Config parses the parameter file into an S3Config.
// SCI_endpoint and SCI_region must be set together or omitted together.
// When both are absent, they are auto-detected from the instance metadata
// service (169.254.169.254). Setting only one is a hard startup error.
// All other mandatory fields (SCI_accessKey, SCI_secretKey, SCI_bucketName) must
// be present. All optional fields default to safe values when absent.
func LoadS3Config(filePath string) (*S3Config, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open parameter file %s: %w", filePath, err)
	}
	defer file.Close()

	// Sentinel values (< 0) distinguish "not provided" from "explicitly zero".
	cfg := &S3Config{
		Retries:           -1,
		UploadPartSize:    -1,
		UploadConcurrency: -1,
		UploadChannelSize: -1,
	}

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
			fmt.Fprintf(os.Stderr, "Warning: malformed line %d in parameter file %s: %s\n",
				lineNumber, filePath, line)
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		switch key {
		// --- mandatory ---
		case "SCI_endpoint":
			cfg.Endpoint = value
		case "SCI_accessKey":
			cfg.AccessKey = value
		case "SCI_secretKey":
			cfg.SecretKey = value
		case "SCI_bucketName":
			cfg.BucketName = value
		case "SCI_region":
			cfg.Region = value
		case "SCI_endpoint_template":
			cfg.EndpointTemplate = value

		// --- optional existing ---
		case "SCI_folderName":
			cfg.FolderName = strings.Trim(value, "/")
		case "SCI_s3ForcePathStyle":
			cfg.S3ForcePathStyle = strings.ToLower(value) == "true"

		// --- optional new ---
		case "log_level":
			cfg.LogLevel = strings.ToLower(value)
		case "log_file":
			cfg.LogFile = value
		case "log_rotate_frequency":
			cfg.LogRotateFreq = strings.ToLower(value)
		case "shorten_folder_path":
			cfg.ShortenFolderPath = strings.ToLower(value) == "true"
		case "retries":
			n, parseErr := strconv.Atoi(value)
			if parseErr != nil || n < 0 {
				fmt.Fprintf(os.Stderr, "Warning: invalid retries value %q in %s, using default 3\n",
					value, filePath)
			} else {
				cfg.Retries = n
			}
		case "tagging":
			cfg.Tagging = strings.ToLower(value) == "true"
		case "object_tags":
			cfg.ObjectTags = value
		case "upload_part_size":
			n, parseErr := strconv.ParseInt(value, 10, 64)
			if parseErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: invalid upload_part_size value %q in %s, using default\n",
					value, filePath)
			} else {
				cfg.UploadPartSize = n
			}
		case "upload_concurrency":
			n, parseErr := strconv.Atoi(value)
			if parseErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: invalid upload_concurrency value %q in %s, using default\n",
					value, filePath)
			} else {
				cfg.UploadConcurrency = n
			}
		case "upload_channel_size":
			n, parseErr := strconv.Atoi(value)
			if parseErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: invalid upload_channel_size value %q in %s, using default\n",
					value, filePath)
			} else {
				cfg.UploadChannelSize = n
			}
		case "sse_enabled":
			cfg.SSEEnabled = strings.ToLower(value) == "true"
		case "sse_kms_key_id":
			cfg.SSEKMSKeyID = strings.TrimSpace(value)
		default:
			fmt.Fprintf(os.Stderr, "Warning: unknown key '%s' in parameter file %s\n", key, filePath)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading parameter file %s: %w", filePath, err)
	}

	// --- Validate mandatory fields ---
	if cfg.AccessKey == "" {
		return nil, fmt.Errorf("SCI_accessKey is not set in parameter file %s", filePath)
	}
	if cfg.SecretKey == "" {
		return nil, fmt.Errorf("SCI_secretKey is not set in parameter file %s", filePath)
	}
	if cfg.BucketName == "" {
		return nil, fmt.Errorf("SCI_bucketName is not set in parameter file %s", filePath)
	}

	// --- Validate and resolve SCI_region / SCI_endpoint ---
	// Rule: both must be set together, or both must be absent.
	// - Both set    → used as-is (explicit config; cross-region or air-gapped)
	// - Neither set → both auto-detected from OpenStack instance metadata (standard SCI VM)
	// - Only one set → hard error; operator must set both or neither
	if cfg.EndpointTemplate == "" {
		cfg.EndpointTemplate = defaultEndpointTemplate
	}
	if strings.Contains(cfg.EndpointTemplate, "example.com") {
		return nil, fmt.Errorf("SCI_endpoint_template in %s is still set to the default placeholder; set it to your actual S3-compatible storage endpoint template", filePath)
	}
	if cfg.Region != "" && cfg.Endpoint != "" {
		// Both explicitly set — use as-is, no metadata call.
	} else if cfg.Region == "" && cfg.Endpoint == "" {
		region, err := detectRegionFromMetadata()
		if err != nil {
			return nil, fmt.Errorf("SCI_region and SCI_endpoint are not set in %s, and auto-detection from instance metadata failed: %w", filePath, err)
		}
		cfg.Region = region
		cfg.Endpoint = strings.ReplaceAll(cfg.EndpointTemplate, "{region}", region)
		fmt.Fprintf(os.Stderr, "Info: SCI_region and SCI_endpoint auto-detected from instance metadata: region=%s endpoint=%s\n", cfg.Region, cfg.Endpoint)
	} else if cfg.Region != "" && cfg.Endpoint == "" {
		return nil, fmt.Errorf("SCI_region is set but SCI_endpoint is missing in %s; set both together or omit both for auto-detection", filePath)
	} else {
		return nil, fmt.Errorf("SCI_endpoint is set but SCI_region is missing in %s; set both together or omit both for auto-detection", filePath)
	}
	if err := validateEndpointScheme(cfg.Endpoint); err != nil {
		return nil, fmt.Errorf("invalid SCI_endpoint in %s: %w", filePath, err)
	}

	// --- Validate log_file and log_level must be set together ---
	if cfg.LogFile != "" && cfg.LogLevel == "" {
		return nil, fmt.Errorf("log_file is set but log_level is missing in %s; set both together", filePath)
	}
	if cfg.LogLevel != "" && cfg.LogFile == "" {
		return nil, fmt.Errorf("log_level is set but log_file is missing in %s; set both together", filePath)
	}

	// --- Apply defaults for optional fields not provided ---
	// LogLevel is only defaulted when LogFile is set — when both are empty the
	// logger writes to stderr and LogLevel remaining "" correctly reflects that
	// no file logging is configured (avoids misleading LogLevel="info"/LogFile="" state).
	if cfg.LogLevel == "" && cfg.LogFile != "" {
		cfg.LogLevel = "info"
	}
	if cfg.LogRotateFreq == "" {
		cfg.LogRotateFreq = "never"
	}
	if cfg.Retries < 0 {
		cfg.Retries = 3
	}
	if cfg.UploadPartSize < 0 {
		cfg.UploadPartSize = 128 * 1024 * 1024 // 128 MiB
	}
	if cfg.UploadConcurrency < 0 {
		cfg.UploadConcurrency = 32
	}
	if cfg.UploadChannelSize < 0 {
		cfg.UploadChannelSize = 10
	}

	// --- Validate upload_part_size in [5 MiB, 256 MiB] ---
	const minPartSize int64 = 5 * 1024 * 1024   // 5 MiB
	const maxPartSize int64 = 256 * 1024 * 1024 // 256 MiB
	if cfg.UploadPartSize < minPartSize {
		return nil, fmt.Errorf("upload_part_size %d is below minimum 5242880 (5 MiB)", cfg.UploadPartSize)
	}
	if cfg.UploadPartSize > maxPartSize {
		return nil, fmt.Errorf("upload_part_size %d exceeds maximum 268435456 (256 MiB)", cfg.UploadPartSize)
	}

	// --- Validate upload_concurrency in [1, 200] ---
	if cfg.UploadConcurrency < 1 {
		return nil, fmt.Errorf("upload_concurrency %d is below minimum 1", cfg.UploadConcurrency)
	}
	if cfg.UploadConcurrency > 200 {
		return nil, fmt.Errorf("upload_concurrency %d exceeds maximum 200", cfg.UploadConcurrency)
	}

	// --- Validate upload_channel_size in [1, 32] ---
	if cfg.UploadChannelSize < 1 {
		return nil, fmt.Errorf("upload_channel_size %d is below minimum 1", cfg.UploadChannelSize)
	}
	if cfg.UploadChannelSize > 32 {
		return nil, fmt.Errorf("upload_channel_size %d exceeds maximum 32", cfg.UploadChannelSize)
	}

	// --- Warn if object_tags is set but tagging is disabled ---
	if !cfg.Tagging && cfg.ObjectTags != "" {
		fmt.Fprintf(os.Stderr, "Warning: object_tags is set but tagging=false in %s; tags will be ignored\n",
			filePath)
	}

	// --- Validate object_tags format and AWS rules ---
	if cfg.ObjectTags != "" {
		if err := validateObjectTags(cfg.ObjectTags); err != nil {
			return nil, fmt.Errorf("%s in %s", err, filePath)
		}
	}

	// --- Validate SSE-KMS fields ---
	if cfg.SSEEnabled && cfg.SSEKMSKeyID == "" {
		return nil, fmt.Errorf("sse_kms_key_id must be set when sse_enabled=true in %s", filePath)
	}
	if !cfg.SSEEnabled && cfg.SSEKMSKeyID != "" {
		fmt.Fprintf(os.Stderr, "Warning: sse_kms_key_id is set but sse_enabled=false in %s; encryption will not be applied\n",
			filePath)
	}

	return cfg, nil
}

// validateObjectTags checks each key=value pair in a comma-separated object_tags
// string against AWS tag length and character rules.
func validateObjectTags(objectTags string) error {
	tagRe := regexp.MustCompile(`^[a-zA-Z0-9 _.:/=+\-@]+$`)

	count := 0
	for _, pair := range strings.Split(objectTags, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		count++
		if count > 5 {
			return fmt.Errorf("object_tags: exceeds maximum of 5 custom tags")
		}
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			return fmt.Errorf("object_tags: malformed entry %q — expected key=value", pair)
		}
		k := strings.TrimSpace(kv[0])
		v := strings.TrimSpace(kv[1])
		if len(k) == 0 {
			return fmt.Errorf("object_tags: tag key must not be empty")
		}
		if len(k) > 128 {
			return fmt.Errorf("object_tags: key %q exceeds 128 character limit", k)
		}
		if len(v) > 256 {
			return fmt.Errorf("object_tags: value for key %q exceeds 256 character limit", k)
		}
		if strings.HasPrefix(k, "aws:") {
			return fmt.Errorf("object_tags: key %q uses reserved prefix \"aws:\"", k)
		}
		if !tagRe.MatchString(k) {
			return fmt.Errorf("object_tags: key %q contains invalid characters", k)
		}
		if v != "" && !tagRe.MatchString(v) {
			return fmt.Errorf("object_tags: value for key %q contains invalid characters", k)
		}
	}
	return nil
}

// logFieldKeys is the set of config keys that control logging behaviour.
// These are intentionally excluded from inline TOOLOPTION overrides because
// the logger is already open before input is parsed — changing these mid-run
// would split the session log across multiple files.
var logFieldKeys = map[string]bool{
	"log_file":             true,
	"log_level":            true,
	"log_rotate_frequency": true,
}

// knownConfigKeys is the complete set of keys accepted by the parameter file.
var knownConfigKeys = map[string]bool{
	"SCI_endpoint":          true,
	"SCI_accessKey":         true,
	"SCI_secretKey":         true,
	"SCI_bucketName":        true,
	"SCI_region":            true,
	"SCI_endpoint_template": true,
	"SCI_folderName":        true,
	"SCI_s3ForcePathStyle":  true,
	"log_level":             true,
	"log_file":              true,
	"log_rotate_frequency":  true,
	"shorten_folder_path":   true,
	"retries":               true,
	"tagging":               true,
	"object_tags":           true,
	"upload_part_size":      true,
	"upload_concurrency":    true,
	"upload_channel_size":   true,
	"sse_enabled":           true,
	"sse_kms_key_id":        true,
}

// ApplyInlineOverrides parses a semicolon-delimited key=value string from a
// #TOOLOPTION line and applies matching fields onto cfg in-place.
//
// Rules:
//   - Log fields (log_file, log_level, log_rotate_frequency) are warned and skipped.
//   - Unknown keys are a hard error.
//   - Value validation failures are a hard error.
//   - All other fields overwrite the corresponding cfg field directly.
func ApplyInlineOverrides(cfg *S3Config, kvString string, warnf func(string, ...any)) error {
	pairs := strings.Split(kvString, ";")
	for _, pair := range pairs {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("TOOLOPTION: malformed key=value pair %q", pair)
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		if !knownConfigKeys[key] {
			return fmt.Errorf("TOOLOPTION: unknown key %q", key)
		}
		if logFieldKeys[key] {
			warnf("TOOLOPTION: %q is a log field and cannot be overridden at runtime; ignoring", key)
			continue
		}

		switch key {
		case "SCI_endpoint":
			cfg.Endpoint = value
		case "SCI_accessKey":
			cfg.AccessKey = value
		case "SCI_secretKey":
			cfg.SecretKey = value
		case "SCI_bucketName":
			cfg.BucketName = value
		case "SCI_region":
			cfg.Region = value
		case "SCI_endpoint_template":
			cfg.EndpointTemplate = value
		case "SCI_folderName":
			cfg.FolderName = strings.Trim(value, "/")
		case "SCI_s3ForcePathStyle":
			cfg.S3ForcePathStyle = strings.ToLower(value) == "true"
		case "shorten_folder_path":
			cfg.ShortenFolderPath = strings.ToLower(value) == "true"
		case "retries":
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return fmt.Errorf("TOOLOPTION: invalid value %q for retries: must be a non-negative integer", value)
			}
			cfg.Retries = n
		case "tagging":
			cfg.Tagging = strings.ToLower(value) == "true"
		case "object_tags":
			if err := validateObjectTags(value); err != nil {
				return fmt.Errorf("TOOLOPTION: %w", err)
			}
			cfg.ObjectTags = value
		case "upload_part_size":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return fmt.Errorf("TOOLOPTION: invalid value %q for upload_part_size: must be an integer", value)
			}
			const minPartSize int64 = 5 * 1024 * 1024
			const maxPartSize int64 = 256 * 1024 * 1024
			if n < minPartSize || n > maxPartSize {
				return fmt.Errorf("TOOLOPTION: upload_part_size %d out of range [5242880, 268435456]", n)
			}
			cfg.UploadPartSize = n
		case "upload_concurrency":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 200 {
				return fmt.Errorf("TOOLOPTION: invalid value %q for upload_concurrency: must be integer in [1, 200]", value)
			}
			cfg.UploadConcurrency = n
		case "upload_channel_size":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 32 {
				return fmt.Errorf("TOOLOPTION: invalid value %q for upload_channel_size: must be integer in [1, 32]", value)
			}
			cfg.UploadChannelSize = n
		case "sse_enabled":
			cfg.SSEEnabled = strings.ToLower(value) == "true"
		case "sse_kms_key_id":
			cfg.SSEKMSKeyID = strings.TrimSpace(value)
		}
	}

	// Cross-field validation after all pairs are applied.
	if cfg.SSEEnabled && cfg.SSEKMSKeyID == "" {
		return fmt.Errorf("TOOLOPTION: sse_kms_key_id must be set when sse_enabled=true")
	}
	if !cfg.Tagging && cfg.ObjectTags != "" {
		warnf("TOOLOPTION: object_tags is set but tagging=false; tags will be ignored")
	}

	return nil
}

// regionRe validates that a derived region contains only safe characters
// before it is embedded in the S3 endpoint URL.
var regionRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// availZoneSuffixRe validates that an OpenStack availability_zone ends with a single
// lowercase letter zone suffix directly appended to the region digit
// (e.g. "eu-de-1b" → region "eu-de-1", zone "b"). No separator between region and zone.
var availZoneSuffixRe = regexp.MustCompile(`[a-z]$`)
// derives the SCI region by stripping the trailing zone letter from availability_zone.
// Example: availability_zone "eu-de-1b" → region "eu-de-1".
// A 2-second timeout is used so that non-SCI environments (no metadata service) fail fast.
func detectRegionFromMetadata() (string, error) {
	const metadataURL = "http://169.254.169.254/openstack/latest/meta_data.json"

	client := &http.Client{
		Timeout: 2 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return fmt.Errorf("redirect not allowed for metadata fetch")
		},
	}

	req, err := http.NewRequest(http.MethodGet, metadataURL, nil)
	if err != nil {
		return "", fmt.Errorf("build metadata request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", metadataURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s returned HTTP %d", metadataURL, resp.StatusCode)
	}

	var meta struct {
		AvailabilityZone string `json:"availability_zone"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return "", fmt.Errorf("decode metadata JSON: %w", err)
	}
	if meta.AvailabilityZone == "" {
		return "", fmt.Errorf("availability_zone is empty in instance metadata response")
	}

	// Validate availability_zone ends with a single lowercase letter zone suffix.
	// Format: <region><zone-letter> e.g. "eu-de-1b" → region "eu-de-1".
	az := meta.AvailabilityZone
	if !availZoneSuffixRe.MatchString(az) {
		return "", fmt.Errorf("availability_zone %q does not match expected format <region><zone-letter>", az)
	}
	region := az[:len(az)-1]
	if !regionRe.MatchString(region) {
		return "", fmt.Errorf("derived region %q contains unexpected characters", region)
	}
	return region, nil
}

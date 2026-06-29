package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- helpers -----------------------------------------------------------------

// writeCfg writes content to a temp file and returns its path.
func writeCfg(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "hdbbackint-*.cfg")
	if err != nil {
		t.Fatalf("writeCfg: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("writeCfg write: %v", err)
	}
	f.Close()
	return f.Name()
}

// minCfg returns a minimal valid config string with region+endpoint set.
const minCfg = `
SCI_accessKey=AKIAIOSFODNN7EXAMPLE
SCI_secretKey=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
SCI_bucketName=my-bucket
SCI_region=us-east-1
SCI_endpoint=https://s3.us-east-1.example.com
`

// --- LoadS3Config ------------------------------------------------------------

func TestLoadS3Config_ValidMinimal(t *testing.T) {
	cfg, err := LoadS3Config(writeCfg(t, minCfg))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AccessKey != "AKIAIOSFODNN7EXAMPLE" {
		t.Errorf("AccessKey = %q", cfg.AccessKey)
	}
	if cfg.BucketName != "my-bucket" {
		t.Errorf("BucketName = %q", cfg.BucketName)
	}
	if cfg.Region != "us-east-1" {
		t.Errorf("Region = %q", cfg.Region)
	}
	if cfg.Endpoint != "https://s3.us-east-1.example.com" {
		t.Errorf("Endpoint = %q", cfg.Endpoint)
	}
}

func TestLoadS3Config_Defaults(t *testing.T) {
	cfg, err := LoadS3Config(writeCfg(t, minCfg))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Retries != 3 {
		t.Errorf("Retries = %d, want 3", cfg.Retries)
	}
	if cfg.UploadPartSize != 128*1024*1024 {
		t.Errorf("UploadPartSize = %d", cfg.UploadPartSize)
	}
	if cfg.UploadConcurrency != 32 {
		t.Errorf("UploadConcurrency = %d", cfg.UploadConcurrency)
	}
	if cfg.UploadChannelSize != 10 {
		t.Errorf("UploadChannelSize = %d", cfg.UploadChannelSize)
	}
	if cfg.LogRotateFreq != "never" {
		t.Errorf("LogRotateFreq = %q", cfg.LogRotateFreq)
	}
}

func TestLoadS3Config_LogLevelDefaultOnlyWhenLogFileSet(t *testing.T) {
	// Neither log_file nor log_level set → LogLevel stays ""
	cfg, err := LoadS3Config(writeCfg(t, minCfg))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LogLevel != "" {
		t.Errorf("LogLevel = %q, want empty when LogFile is not set", cfg.LogLevel)
	}

	// Both log_file and log_level set → LogLevel = "debug"
	content := minCfg + "\nlog_file=/tmp/test.log\nlog_level=debug\n"
	cfg2, err := LoadS3Config(writeCfg(t, content))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg2.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg2.LogLevel)
	}
}

func TestLoadS3Config_MissingMandatoryFields(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name: "missing accessKey",
			content: `SCI_secretKey=secret
SCI_bucketName=bucket
SCI_region=us-east-1
SCI_endpoint=https://s3.us-east-1.example.com`,
			wantErr: "SCI_accessKey",
		},
		{
			name: "missing secretKey",
			content: `SCI_accessKey=key
SCI_bucketName=bucket
SCI_region=us-east-1
SCI_endpoint=https://s3.us-east-1.example.com`,
			wantErr: "SCI_secretKey",
		},
		{
			name: "missing bucketName",
			content: `SCI_accessKey=key
SCI_secretKey=secret
SCI_region=us-east-1
SCI_endpoint=https://s3.us-east-1.example.com`,
			wantErr: "SCI_bucketName",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadS3Config(writeCfg(t, tt.content))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got err=%v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadS3Config_RegionEndpointRules(t *testing.T) {
	base := `SCI_accessKey=key
SCI_secretKey=secret
SCI_bucketName=bucket`

	tests := []struct {
		name    string
		extra   string
		wantErr string
	}{
		{
			name:    "only region set",
			extra:   "SCI_region=us-east-1",
			wantErr: "SCI_region is set but SCI_endpoint is missing",
		},
		{
			name:    "only endpoint set",
			extra:   "SCI_endpoint=https://s3.us-east-1.example.com",
			wantErr: "SCI_endpoint is set but SCI_region is missing",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadS3Config(writeCfg(t, base+"\n"+tt.extra))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got err=%v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadS3Config_LogFileLevelMustBeSetTogether(t *testing.T) {
	base := `SCI_accessKey=key
SCI_secretKey=secret
SCI_bucketName=bucket
SCI_region=us-east-1
SCI_endpoint=https://s3.us-east-1.example.com`

	tests := []struct {
		name    string
		extra   string
		wantErr string
	}{
		{
			name:    "log_file without log_level",
			extra:   "log_file=/tmp/test.log",
			wantErr: "log_file is set but log_level is missing",
		},
		{
			name:    "log_level without log_file",
			extra:   "log_level=debug",
			wantErr: "log_level is set but log_file is missing",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadS3Config(writeCfg(t, base+"\n"+tt.extra))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got err=%v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadS3Config_UploadPartSizeRangeErrors(t *testing.T) {
	base := minCfg
	tests := []struct {
		name    string
		value   string
		wantErr string
	}{
		{"below min", "upload_part_size=4194304", "below minimum"},
		{"above max", "upload_part_size=269484032", "exceeds maximum"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadS3Config(writeCfg(t, base+"\n"+tt.value))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got err=%v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadS3Config_UploadConcurrencyRangeErrors(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr string
	}{
		{"below min", "upload_concurrency=0", "below minimum 1"},
		{"above max", "upload_concurrency=201", "exceeds maximum 200"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadS3Config(writeCfg(t, minCfg+"\n"+tt.value))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got err=%v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadS3Config_UploadChannelSizeRangeErrors(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr string
	}{
		{"below min", "upload_channel_size=0", "below minimum 1"},
		{"above max", "upload_channel_size=33", "exceeds maximum 32"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadS3Config(writeCfg(t, minCfg+"\n"+tt.value))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got err=%v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadS3Config_SSEKMSRules(t *testing.T) {
	tests := []struct {
		name    string
		extra   string
		wantErr string
	}{
		{
			name:    "sse_enabled without key",
			extra:   "sse_enabled=true",
			wantErr: "sse_kms_key_id must be set",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadS3Config(writeCfg(t, minCfg+"\n"+tt.extra))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got err=%v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadS3Config_PlaceholderEndpointTemplateRejected(t *testing.T) {
	content := `SCI_accessKey=key
SCI_secretKey=secret
SCI_bucketName=bucket
SCI_endpoint_template=https://s3.{region}.example.com`
	_, err := LoadS3Config(writeCfg(t, content))
	if err == nil || !strings.Contains(err.Error(), "default placeholder") {
		t.Errorf("got err=%v, want placeholder error", err)
	}
}

func TestLoadS3Config_HttpEndpointRejected(t *testing.T) {
	content := minCfg
	content = strings.ReplaceAll(content, "https://s3.us-east-1.example.com", "http://s3.us-east-1.example.com")
	_, err := LoadS3Config(writeCfg(t, content))
	if err == nil || !strings.Contains(err.Error(), "uses http") {
		t.Errorf("got err=%v, want http scheme error", err)
	}
}

func TestLoadS3Config_LocalhostHttpAllowed(t *testing.T) {
	content := `SCI_accessKey=key
SCI_secretKey=secret
SCI_bucketName=bucket
SCI_region=us-east-1
SCI_endpoint=http://localhost:9000`
	_, err := LoadS3Config(writeCfg(t, content))
	if err != nil {
		t.Errorf("localhost http should be allowed, got: %v", err)
	}
}

func TestLoadS3Config_FileNotFound(t *testing.T) {
	_, err := LoadS3Config(filepath.Join(t.TempDir(), "nonexistent.cfg"))
	if err == nil {
		t.Error("expected error for non-existent file")
	}
}

// --- validateEndpointScheme --------------------------------------------------

func TestValidateEndpointScheme(t *testing.T) {
	tests := []struct {
		endpoint string
		wantErr  bool
	}{
		{"https://s3.us-east-1.example.com", false},
		{"https://s3.example.com:443", false},
		{"https:", true},
		{"http://localhost:9000", false},
		{"http://127.0.0.1:9000", false},
		{"http://s3.us-east-1.example.com", true},
		{"ftp://s3.example.com", true},
		{"://bad-url", true},
	}
	for _, tt := range tests {
		t.Run(tt.endpoint, func(t *testing.T) {
			err := validateEndpointScheme(tt.endpoint)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateEndpointScheme(%q) err=%v, wantErr=%v", tt.endpoint, err, tt.wantErr)
			}
		})
	}
}

// --- validateObjectTags ------------------------------------------------------

func TestValidateObjectTags(t *testing.T) {
	tests := []struct {
		name    string
		tags    string
		wantErr bool
		errPart string
	}{
		{"valid single", "env=prod", false, ""},
		{"valid multiple", "env=prod,team=dba", false, ""},
		{"valid 5 tags", "k1=v1,k2=v2,k3=v3,k4=v4,k5=v5", false, ""},
		{"exceeds 5", "k1=v1,k2=v2,k3=v3,k4=v4,k5=v5,k6=v6", true, "exceeds maximum"},
		{"key too long", strings.Repeat("a", 129) + "=val", true, "exceeds 128"},
		{"value too long", "key=" + strings.Repeat("a", 257), true, "exceeds 256"},
		{"invalid key chars", "bad!key=val", true, "invalid characters"},
		{"aws reserved prefix", "aws:tag=val", true, "reserved prefix"},
		{"empty key", "=val", true, "must not be empty"},
		{"malformed no equals", "badvalue", true, "malformed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateObjectTags(tt.tags)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateObjectTags(%q) err=%v, wantErr=%v", tt.tags, err, tt.wantErr)
			}
			if tt.wantErr && tt.errPart != "" && (err == nil || !strings.Contains(err.Error(), tt.errPart)) {
				t.Errorf("validateObjectTags(%q) err=%v, want error containing %q", tt.tags, err, tt.errPart)
			}
		})
	}
}

// --- ApplyInlineOverrides ----------------------------------------------------

func TestApplyInlineOverrides_ValidKeys(t *testing.T) {
	cfg := &S3Config{
		Endpoint:          "https://s3.example.com",
		Region:            "us-east-1",
		AccessKey:         "key",
		SecretKey:         "secret",
		BucketName:        "bucket",
		Retries:           3,
		UploadPartSize:    128 * 1024 * 1024,
		UploadConcurrency: 32,
		UploadChannelSize: 10,
	}
	warns := []string{}
	warnf := func(format string, args ...any) {
		warns = append(warns, fmt.Sprintf(format, args...))
	}

	err := ApplyInlineOverrides(cfg, "SCI_folderName=my-folder;tagging=true;object_tags=env=prod", warnf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.FolderName != "my-folder" {
		t.Errorf("FolderName = %q, want my-folder", cfg.FolderName)
	}
	if !cfg.Tagging {
		t.Error("Tagging should be true")
	}
	if cfg.ObjectTags != "env=prod" {
		t.Errorf("ObjectTags = %q, want env=prod", cfg.ObjectTags)
	}
}

func TestApplyInlineOverrides_UnknownKeyErrors(t *testing.T) {
	cfg := &S3Config{}
	err := ApplyInlineOverrides(cfg, "SCI_bucketNam=wrong", func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("got err=%v, want unknown key error", err)
	}
}

func TestApplyInlineOverrides_LogFieldsWarnedAndSkipped(t *testing.T) {
	cfg := &S3Config{LogFile: "/original.log", LogLevel: "info"}
	warns := []string{}
	err := ApplyInlineOverrides(cfg, "log_file=/new.log", func(format string, args ...any) {
		warns = append(warns, fmt.Sprintf(format, args...))
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LogFile != "/original.log" {
		t.Errorf("LogFile changed to %q, should remain /original.log", cfg.LogFile)
	}
	if len(warns) == 0 {
		t.Error("expected warning for log field override attempt")
	}
}

func TestApplyInlineOverrides_InvalidUploadPartSize(t *testing.T) {
	cfg := &S3Config{UploadPartSize: 128 * 1024 * 1024}
	err := ApplyInlineOverrides(cfg, "upload_part_size=1024", func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Errorf("got err=%v, want out of range error", err)
	}
}

func TestApplyInlineOverrides_SSECrossFieldRule(t *testing.T) {
	cfg := &S3Config{}
	err := ApplyInlineOverrides(cfg, "sse_enabled=true", func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "sse_kms_key_id must be set") {
		t.Errorf("got err=%v, want sse_kms_key_id error", err)
	}
}

func TestApplyInlineOverrides_MalformedPair(t *testing.T) {
	cfg := &S3Config{}
	err := ApplyInlineOverrides(cfg, "badvalue", func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Errorf("got err=%v, want malformed error", err)
	}
}

// --- detectRegionFromMetadata ------------------------------------------------

func TestDetectRegionFromMetadata_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"availability_zone":"eu-de-1b"}`)
	}))
	defer srv.Close()

	region, err := detectRegionFromMetadataURL(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if region != "eu-de-1" {
		t.Errorf("region = %q, want eu-de-1", region)
	}
}

func TestDetectRegionFromMetadata_VariousZones(t *testing.T) {
	tests := []struct {
		az     string
		region string
	}{
		{"eu-de-1b", "eu-de-1"},
		{"us-east-1a", "us-east-1"},
		{"ap-cn-1c", "ap-cn-1"},
	}
	for _, tt := range tests {
		t.Run(tt.az, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"availability_zone":%q}`, tt.az)
			}))
			defer srv.Close()
			region, err := detectRegionFromMetadataURL(srv.URL)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if region != tt.region {
				t.Errorf("region = %q, want %q", region, tt.region)
			}
		})
	}
}

func TestDetectRegionFromMetadata_InvalidZoneFormat(t *testing.T) {
	tests := []struct {
		az      string
		wantErr string
	}{
		{"eu-de-1", "does not match expected format"},
		{"abc", "does not match expected format"},
		{"EU-DE-1b", "does not match expected format"},
		{"", "is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.az, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"availability_zone":%q}`, tt.az)
			}))
			defer srv.Close()
			_, err := detectRegionFromMetadataURL(srv.URL)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("az=%q: got err=%v, want error containing %q", tt.az, err, tt.wantErr)
			}
		})
	}
}

func TestDetectRegionFromMetadata_RedirectRejected(t *testing.T) {
	// target server returns a valid response
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"availability_zone":"eu-de-1b"}`)
	}))
	defer target.Close()

	// redirect server redirects to target
	redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirectSrv.Close()

	_, err := detectRegionFromMetadataURL(redirectSrv.URL)
	if err == nil || !strings.Contains(err.Error(), "redirect not allowed") {
		t.Errorf("got err=%v, want redirect not allowed error", err)
	}
}

func TestDetectRegionFromMetadata_Non200Response(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := detectRegionFromMetadataURL(srv.URL)
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("got err=%v, want HTTP 404 error", err)
	}
}

func TestDetectRegionFromMetadata_ServerUnavailable(t *testing.T) {
	_, err := detectRegionFromMetadataURL("http://127.0.0.1:1") // nothing listening
	if err == nil {
		t.Error("expected error for unavailable server")
	}
}

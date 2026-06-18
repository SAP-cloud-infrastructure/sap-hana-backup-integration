package main

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// newTestClient creates a minimal S3Client for unit testing path and tag logic.
// It does not initialise a real AWS SDK client — only the fields used by the
// functions under test are populated.
func newTestClient(cfg *S3Config, sid, dbVersion string) *S3Client {
	return &S3Client{
		cfg:       cfg,
		sid:       sid,
		dbVersion: dbVersion,
		log:       NewLogger(nil),
		tag:       "",
	}
}

// --- resolveHanaPath ---------------------------------------------------------

func TestResolveHanaPath_ShortenDisabled(t *testing.T) {
	c := newTestClient(&S3Config{ShortenFolderPath: false}, "GCO", "")
	path := "/usr/sap/GCO/SYS/global/hdb/backint/DB_GCO/BACKUP_databackup_0_1"
	got := c.resolveHanaPath(path)
	if got != path {
		t.Errorf("resolveHanaPath() = %q, want %q (unchanged)", got, path)
	}
}

func TestResolveHanaPath_ShortenEnabled(t *testing.T) {
	c := newTestClient(&S3Config{ShortenFolderPath: true}, "GCO", "")
	tests := []struct {
		input string
		want  string
	}{
		{
			"/usr/sap/GCO/SYS/global/hdb/backint/DB_GCO/BACKUP_databackup_0_1",
			"GCO/DB_GCO/BACKUP_databackup_0_1",
		},
		{
			"/usr/sap/GCO/SYS/global/hdb/backint/SYSTEMDB/log_backup_0_0_0_0",
			"GCO/SYSTEMDB/log_backup_0_0_0_0",
		},
	}
	for _, tt := range tests {
		got := c.resolveHanaPath(tt.input)
		if got != tt.want {
			t.Errorf("resolveHanaPath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestResolveHanaPath_ShortenNoBackintSep(t *testing.T) {
	c := newTestClient(&S3Config{ShortenFolderPath: true}, "GCO", "")
	path := "/some/other/path/no-separator-here"
	got := c.resolveHanaPath(path)
	if got != path {
		t.Errorf("resolveHanaPath() = %q, want %q (unchanged when no /backint/ found)", got, path)
	}
}

func TestResolveHanaPath_ShortenEmptySID(t *testing.T) {
	c := newTestClient(&S3Config{ShortenFolderPath: true}, "", "")
	path := "/usr/sap/GCO/SYS/global/hdb/backint/DB_GCO/BACKUP_databackup_0_1"
	got := c.resolveHanaPath(path)
	// No SID prefix when sid is empty
	if got != "DB_GCO/BACKUP_databackup_0_1" {
		t.Errorf("resolveHanaPath() = %q, want DB_GCO/BACKUP_databackup_0_1", got)
	}
}

// --- buildTagSet -------------------------------------------------------------

func TestBuildTagSet_NoDbVersionNoCustomTags(t *testing.T) {
	c := newTestClient(&S3Config{Tagging: true}, "GCO", "")
	tags := c.buildTagSet("")
	if len(tags) != 1 {
		t.Fatalf("len(tags) = %d, want 1", len(tags))
	}
	assertTag(t, tags, "hdbbackint_version", SoftwareVersion)
}

func TestBuildTagSet_WithDbVersion(t *testing.T) {
	c := newTestClient(&S3Config{Tagging: true}, "GCO", "HANA HDB server 2.00.083.00")
	tags := c.buildTagSet("")
	if len(tags) != 2 {
		t.Fatalf("len(tags) = %d, want 2", len(tags))
	}
	assertTag(t, tags, "hdbbackint_version", SoftwareVersion)
	assertTag(t, tags, "db_version", "HANA HDB server 2.00.083.00")
}

func TestBuildTagSet_CustomTags(t *testing.T) {
	c := newTestClient(&S3Config{
		Tagging:    true,
		ObjectTags: "env=prod,team=dba",
	}, "GCO", "HANA 2.0")
	tags := c.buildTagSet("")
	// 2 auto + 2 custom = 4
	if len(tags) != 4 {
		t.Fatalf("len(tags) = %d, want 4", len(tags))
	}
	assertTag(t, tags, "env", "prod")
	assertTag(t, tags, "team", "dba")
}

func TestBuildTagSet_MaxFiveCustomTags(t *testing.T) {
	c := newTestClient(&S3Config{
		Tagging:    true,
		ObjectTags: "k1=v1,k2=v2,k3=v3,k4=v4,k5=v5,k6=v6",
	}, "GCO", "")
	tags := c.buildTagSet("")
	// 1 auto + 5 custom (k6 ignored) = 6
	if len(tags) != 6 {
		t.Fatalf("len(tags) = %d, want 6 (5 custom + 1 auto)", len(tags))
	}
	// k6 must not be present
	for _, tag := range tags {
		if *tag.Key == "k6" {
			t.Error("k6 should be ignored (exceeds 5 custom tag limit)")
		}
	}
}

func TestBuildTagSet_MalformedCustomTagSkipped(t *testing.T) {
	c := newTestClient(&S3Config{
		Tagging:    true,
		ObjectTags: "valid=yes,badentry,also=fine",
	}, "GCO", "")
	tags := c.buildTagSet("")
	// 1 auto + 2 valid custom (badentry skipped) = 3
	if len(tags) != 3 {
		t.Fatalf("len(tags) = %d, want 3 (malformed entry skipped)", len(tags))
	}
}

// assertTag is a helper that verifies a tag key=value exists in the slice.
func assertTag(t *testing.T, tags []types.Tag, key, value string) {
	t.Helper()
	for _, tag := range tags {
		if *tag.Key == key {
			if *tag.Value != value {
				t.Errorf("tag %q = %q, want %q", key, *tag.Value, value)
			}
			return
		}
	}
	t.Errorf("tag %q not found in tagset", key)
}

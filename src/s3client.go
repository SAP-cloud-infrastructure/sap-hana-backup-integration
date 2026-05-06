package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
)

// S3Backend is the interface for all S3 operations.
// Using an interface decouples handlers from the AWS SDK and enables testing.
type S3Backend interface {
	BackupObject(ctx context.Context, hanaPath string, body io.Reader, ebid string) (bytesUploaded int64, s3Key string, err error)
	RestoreObject(ctx context.Context, hanaPath string, ebid string, dst io.Writer) (bytesWritten int64, resolvedEbid string, err error)
	DeleteObject(ctx context.Context, hanaPath string, ebid string) error
	InquireObject(ctx context.Context, hanaPath string, ebid string) (time.Time, error)
	ListVersionsForPath(ctx context.Context, hanaPath string) ([]ObjectInfo, error)
	ListAllForTenant(ctx context.Context, userID string) ([]ObjectInfo, error)
}

// ObjectInfo carries metadata for a single backup object.
type ObjectInfo struct {
	ExternalBackupID string
	FileName         string
	CreationDate     time.Time
	S3Key            string
}

// S3Client is the production implementation of S3Backend, using AWS SDK v2.
type S3Client struct {
	svc *s3.Client
	cfg *S3Config
	log *Logger
}

// NewS3Client builds an S3Client from cfg.
func NewS3Client(cfg *S3Config, log *Logger) (*S3Client, error) {
	credsProvider := credentials.NewStaticCredentialsProvider(
		cfg.AccessKey,
		cfg.SecretKey,
		"", // session token — empty for CEPH/static credentials
	)

	awsCfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion(cfg.Region),
		config.WithCredentialsProvider(credsProvider),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Apply endpoint and path-style via per-client functional options.
	svc := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.Endpoint)
		if cfg.S3ForcePathStyle {
			o.UsePathStyle = true
		}
	})

	log.Infof("S3 client initialized: endpoint=%s region=%s pathStyle=%t folder=%q",
		cfg.Endpoint, cfg.Region, cfg.S3ForcePathStyle, cfg.FolderName)

	return &S3Client{svc: svc, cfg: cfg, log: log}, nil
}

// buildS3ObjectKey constructs the S3 key: [FolderName/]<trimmedHanaPath>/<EBID>.bak
func (c *S3Client) buildS3ObjectKey(hanaPath, ebid string) string {
	trimmed := strings.TrimPrefix(hanaPath, "/")
	object := ebid + ".bak"
	if c.cfg.FolderName != "" {
		return path.Join(c.cfg.FolderName, trimmed, object)
	}
	return path.Join(trimmed, object)
}

// buildS3Prefix constructs the listing prefix: [FolderName/]<trimmedHanaPath>/
func (c *S3Client) buildS3Prefix(hanaPath string) string {
	trimmed := strings.TrimPrefix(hanaPath, "/")
	if c.cfg.FolderName != "" {
		return path.Join(c.cfg.FolderName, trimmed) + "/"
	}
	return trimmed + "/"
}

// countingReader wraps an io.Reader and counts bytes read.
type countingReader struct {
	r io.Reader
	n int64
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.n += int64(n)
	return n, err
}

// BackupObject streams body to S3 using multipart upload.
// No in-memory buffering — the manager chunks the stream directly.
func (c *S3Client) BackupObject(ctx context.Context, hanaPath string, body io.Reader, ebid string) (int64, string, error) {
	if ebid == "" {
		return 0, "", fmt.Errorf("ebid cannot be empty for backup")
	}
	s3Key := c.buildS3ObjectKey(hanaPath, ebid)

	cr := &countingReader{r: body}

	uploader := manager.NewUploader(c.svc, func(u *manager.Uploader) {
		u.PartSize = 64 * 1024 * 1024 // 64 MiB — safe for CEPH RGW minimum part size
		u.Concurrency = 1             // single-threaded to avoid ordering issues on CEPH
		u.LeavePartsOnError = false
	})

	c.log.Infof("S3 upload start: bucket=%s key=%s path=%s ebid=%s",
		c.cfg.BucketName, s3Key, hanaPath, ebid)

	_, err := uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(c.cfg.BucketName),
		Key:    aws.String(s3Key),
		Body:   cr,
	})
	if err != nil {
		return 0, s3Key, fmt.Errorf("upload %s (key %s): %w", hanaPath, s3Key, err)
	}

	c.log.Infof("S3 upload done: key=%s bytes=%d", s3Key, cr.n)
	return cr.n, s3Key, nil
}

// RestoreObject downloads an object from S3 and writes it to dst.
// If ebid is empty, the latest version for hanaPath is resolved automatically.
func (c *S3Client) RestoreObject(ctx context.Context, hanaPath string, ebid string, dst io.Writer) (int64, string, error) {
	resolvedEbid := ebid
	if resolvedEbid == "" {
		c.log.Infof("Restore #NULL: resolving latest EBID for %s", hanaPath)
		objects, err := c.ListVersionsForPath(ctx, hanaPath)
		if err != nil {
			return 0, "", fmt.Errorf("list versions for #NULL restore of %s: %w", hanaPath, err)
		}
		resolvedEbid = objects[0].ExternalBackupID // sorted latest-first
		c.log.Infof("Restore #NULL: resolved EBID=%s for %s", resolvedEbid, hanaPath)
	}

	s3Key := c.buildS3ObjectKey(hanaPath, resolvedEbid)
	c.log.Infof("S3 restore start: bucket=%s key=%s", c.cfg.BucketName, s3Key)

	out, err := c.svc.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.cfg.BucketName),
		Key:    aws.String(s3Key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return 0, resolvedEbid, &ErrNotFound{S3Key: s3Key}
		}
		return 0, resolvedEbid, fmt.Errorf("get object %s: %w", s3Key, err)
	}
	defer out.Body.Close()

	written, err := io.Copy(dst, out.Body)
	if err != nil {
		return written, resolvedEbid, fmt.Errorf("write object %s to output: %w", s3Key, err)
	}
	c.log.Infof("S3 restore done: key=%s bytes=%d", s3Key, written)
	return written, resolvedEbid, nil
}

// DeleteObject removes a specific backup version from S3.
// S3 DeleteObject is idempotent; no pre-flight HeadObject is needed.
func (c *S3Client) DeleteObject(ctx context.Context, hanaPath string, ebid string) error {
	if ebid == "" {
		return fmt.Errorf("ebid cannot be empty for delete")
	}
	s3Key := c.buildS3ObjectKey(hanaPath, ebid)
	c.log.Infof("S3 delete: bucket=%s key=%s", c.cfg.BucketName, s3Key)

	_, err := c.svc.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.cfg.BucketName),
		Key:    aws.String(s3Key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return &ErrNotFound{S3Key: s3Key}
		}
		return fmt.Errorf("delete object %s: %w", s3Key, err)
	}
	c.log.Infof("S3 delete done: key=%s", s3Key)
	return nil
}

// InquireObject returns the last-modified time for a specific backup version.
func (c *S3Client) InquireObject(ctx context.Context, hanaPath string, ebid string) (time.Time, error) {
	if ebid == "" {
		return time.Time{}, fmt.Errorf("ebid cannot be empty for inquire")
	}
	s3Key := c.buildS3ObjectKey(hanaPath, ebid)
	c.log.Infof("S3 inquire: bucket=%s key=%s", c.cfg.BucketName, s3Key)

	head, err := c.svc.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.cfg.BucketName),
		Key:    aws.String(s3Key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return time.Time{}, &ErrNotFound{S3Key: s3Key}
		}
		return time.Time{}, fmt.Errorf("head object %s: %w", s3Key, err)
	}
	if head.LastModified == nil {
		return time.Time{}, fmt.Errorf("LastModified is nil for key %s", s3Key)
	}
	c.log.Infof("S3 inquire done: key=%s lastModified=%v", s3Key, *head.LastModified)
	return *head.LastModified, nil
}

// ListVersionsForPath returns all backup versions for hanaPath, sorted newest-first.
func (c *S3Client) ListVersionsForPath(ctx context.Context, hanaPath string) ([]ObjectInfo, error) {
	prefix := c.buildS3Prefix(hanaPath)
	c.log.Infof("S3 list versions: bucket=%s prefix=%q", c.cfg.BucketName, prefix)

	var objects []ObjectInfo
	paginator := s3.NewListObjectsV2Paginator(c.svc, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.cfg.BucketName),
		Prefix: aws.String(prefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list page for prefix %q: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil || obj.LastModified == nil || !strings.HasSuffix(*obj.Key, ".bak") {
				continue
			}
			ebidWithExt := path.Base(*obj.Key)
			ebid := strings.TrimSuffix(ebidWithExt, ".bak")
			if ebid == "" {
				continue
			}
			objects = append(objects, ObjectInfo{
				ExternalBackupID: ebid,
				FileName:         hanaPath,
				CreationDate:     *obj.LastModified,
				S3Key:            *obj.Key,
			})
		}
	}

	if len(objects) == 0 {
		return nil, &ErrNotFound{S3Key: prefix}
	}
	sort.Slice(objects, func(i, j int) bool {
		return objects[i].CreationDate.After(objects[j].CreationDate)
	})
	c.log.Infof("S3 list versions done: prefix=%q count=%d", prefix, len(objects))
	return objects, nil
}

// ListAllForTenant returns all backup objects under a tenant-scoped prefix, sorted newest-first.
func (c *S3Client) ListAllForTenant(ctx context.Context, userID string) ([]ObjectInfo, error) {
	basePrefix := ""
	if c.cfg.FolderName != "" {
		basePrefix = c.cfg.FolderName + "/"
	}

	// Derive a specific prefix from the SID in "DBNAME@SID" format.
	if parts := strings.Split(userID, "@"); len(parts) == 2 && parts[1] != "" {
		basePrefix = path.Join(basePrefix, fmt.Sprintf("usr/sap/%s/", parts[1]))
	} else if userID != "" && !strings.Contains(userID, "@") {
		basePrefix = path.Join(basePrefix, fmt.Sprintf("usr/sap/%s/", userID))
	} else {
		if c.cfg.FolderName == "" {
			basePrefix = "usr/sap/"
		}
		c.log.Warnf("could not derive SID from userID=%q, using broad prefix=%q", userID, basePrefix)
	}
	basePrefix = strings.TrimRight(basePrefix, "/") + "/"

	c.log.Infof("S3 list all for tenant: bucket=%s prefix=%q", c.cfg.BucketName, basePrefix)

	var objects []ObjectInfo
	paginator := s3.NewListObjectsV2Paginator(c.svc, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.cfg.BucketName),
		Prefix: aws.String(basePrefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list page for tenant prefix %q: %w", basePrefix, err)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil || obj.LastModified == nil || !strings.HasSuffix(*obj.Key, ".bak") {
				continue
			}
			// Reconstruct original HANA path from S3 key.
			pathWithoutFolder := *obj.Key
			if c.cfg.FolderName != "" {
				pathWithoutFolder = strings.TrimPrefix(*obj.Key, c.cfg.FolderName+"/")
			}
			dir := path.Dir(pathWithoutFolder)
			ebidWithExt := path.Base(pathWithoutFolder)
			ebid := strings.TrimSuffix(ebidWithExt, ".bak")
			if ebid == "" {
				continue
			}
			objects = append(objects, ObjectInfo{
				ExternalBackupID: ebid,
				FileName:         "/" + dir,
				CreationDate:     *obj.LastModified,
				S3Key:            *obj.Key,
			})
		}
	}

	if len(objects) == 0 {
		return nil, &ErrNotFound{S3Key: basePrefix}
	}
	sort.Slice(objects, func(i, j int) bool {
		return objects[i].CreationDate.After(objects[j].CreationDate)
	})
	c.log.Infof("S3 list all for tenant done: prefix=%q count=%d", basePrefix, len(objects))
	return objects, nil
}

// isS3NotFound returns true when err signals a missing object (404 / NoSuchKey).
// It checks both SDK v2 typed errors and the smithy fallback for CEPH RGW.
func isS3NotFound(err error) bool {
	// Typed SDK v2 errors (AWS S3 proper).
	var notFound *types.NoSuchKey
	if errors.As(err, &notFound) {
		return true
	}
	var notFoundBucket *types.NoSuchBucket
	if errors.As(err, &notFoundBucket) {
		return true
	}
	// Smithy fallback for CEPH and other S3-compatible stores that return
	// HTTP 404 with a generic error code.
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		return code == "NotFound" || code == "NoSuchKey" || code == "404"
	}
	return false
}

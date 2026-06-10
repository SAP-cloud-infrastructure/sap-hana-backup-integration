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
	BackupObject(ctx context.Context, hanaPath string, body io.Reader, ebid string, tag string) (bytesUploaded int64, s3Key string, err error)
	RestoreObject(ctx context.Context, hanaPath string, ebid string, dst io.Writer, tag string) (bytesWritten int64, resolvedEbid string, err error)
	DeleteObject(ctx context.Context, hanaPath string, ebid string, tag string) error
	InquireObject(ctx context.Context, hanaPath string, ebid string, tag string) (time.Time, error)
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
	svc       *s3.Client
	cfg       *S3Config
	log       *Logger
	dbVersion string // extracted from #SOFTWAREID, used in object tags
	tag       string // "[SID][-][backup_level] " session tag for log lines
	sid       string // HANA SID, used as path prefix when shorten_folder_path=true
}

// NewS3Client builds an S3Client from cfg.
// dbVersion is the HANA software version string extracted from #SOFTWAREID,
// used to populate the db_version object tag when tagging is enabled.
// tag is the session log prefix "[SID][-][backup_level] ".
// sid is the HANA SID, used as a path prefix when shorten_folder_path=true.
func NewS3Client(cfg *S3Config, log *Logger, dbVersion string, tag string, sid string) (*S3Client, error) {
	credsProvider := credentials.NewStaticCredentialsProvider(
		cfg.AccessKey,
		cfg.SecretKey,
		"", // session token — empty for CEPH/static credentials
	)

	// cfg.Retries is the number of retry attempts; SDK uses total attempts = retries+1.
	awsCfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion(cfg.Region),
		config.WithCredentialsProvider(credsProvider),
		config.WithRetryMaxAttempts(cfg.Retries+1),
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

	log.Infof("%sS3 client initialized: endpoint=%s region=%s pathStyle=%t folder=%q retries=%d",
		tag, cfg.Endpoint, cfg.Region, cfg.S3ForcePathStyle, cfg.FolderName, cfg.Retries)
	if cfg.SSEEnabled {
		log.Infof("%sSSE-KMS enabled: kms_key_id=%s", tag, cfg.SSEKMSKeyID)
	}

	return &S3Client{svc: svc, cfg: cfg, log: log, dbVersion: dbVersion, tag: tag, sid: sid}, nil
}

// resolveHanaPath applies the shorten_folder_path transformation when configured.
// It strips the /usr/sap/<anything>/backint/ prefix, leaving <SID>/<DBNAME>/<file>.
// The SID is prepended so that objects remain scoped per system even with shortened paths.
func (c *S3Client) resolveHanaPath(hanaPath string) string {
	if !c.cfg.ShortenFolderPath {
		return hanaPath
	}
	const sep = "/backint/"
	if idx := strings.Index(hanaPath, sep); idx >= 0 {
		shortened := hanaPath[idx+len(sep):]
		if c.sid != "" {
			return c.sid + "/" + shortened
		}
		return shortened
	}
	return hanaPath
}

// buildS3ObjectKey constructs the S3 key: [FolderName/]<resolvedPath>/<EBID>.bak
func (c *S3Client) buildS3ObjectKey(hanaPath, ebid string) string {
	trimmed := strings.TrimPrefix(c.resolveHanaPath(hanaPath), "/")
	object := ebid + ".bak"
	if c.cfg.FolderName != "" {
		return path.Join(c.cfg.FolderName, trimmed, object)
	}
	return path.Join(trimmed, object)
}

// buildS3Prefix constructs the listing prefix: [FolderName/]<resolvedPath>/
func (c *S3Client) buildS3Prefix(hanaPath string) string {
	trimmed := strings.TrimPrefix(c.resolveHanaPath(hanaPath), "/")
	if c.cfg.FolderName != "" {
		return path.Join(c.cfg.FolderName, trimmed) + "/"
	}
	return trimmed + "/"
}

// buildTagSet returns S3 object tags as []types.Tag for use with PutObjectTagging.
// It always includes hdbbackint_version and db_version (when available).
// Up to 5 custom tags from cfg.ObjectTags (comma-separated key=value) are appended.
func (c *S3Client) buildTagSet(tag string) []types.Tag {
	tags := []types.Tag{
		{Key: aws.String("hdbbackint_version"), Value: aws.String(SoftwareVersion)},
	}
	if c.dbVersion != "" {
		tags = append(tags, types.Tag{Key: aws.String("db_version"), Value: aws.String(c.dbVersion)})
	}
	if c.cfg.ObjectTags != "" {
		customCount := 0
		for _, pair := range strings.Split(c.cfg.ObjectTags, ",") {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			if customCount >= 5 {
				c.log.Warnf("%sobject_tags: exceeds 5 custom tag limit; ignoring remaining tags", c.tag)
				break
			}
			kv := strings.SplitN(pair, "=", 2)
			if len(kv) != 2 {
				c.log.Warnf("%sobject_tags: skipping malformed entry %q", c.tag, pair)
				continue
			}
			tags = append(tags, types.Tag{
				Key:   aws.String(strings.TrimSpace(kv[0])),
				Value: aws.String(strings.TrimSpace(kv[1])),
			})
			customCount++
		}
	}
	return tags
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
func (c *S3Client) BackupObject(ctx context.Context, hanaPath string, body io.Reader, ebid string, tag string) (int64, string, error) {
	if ebid == "" {
		return 0, "", fmt.Errorf("ebid cannot be empty for backup")
	}
	s3Key := c.buildS3ObjectKey(hanaPath, ebid)

	cr := &countingReader{r: body}

	uploader := manager.NewUploader(c.svc, func(u *manager.Uploader) {
		u.PartSize = c.cfg.UploadPartSize
		u.Concurrency = c.cfg.UploadConcurrency
		u.LeavePartsOnError = false
	})

	input := &s3.PutObjectInput{
		Bucket: aws.String(c.cfg.BucketName),
		Key:    aws.String(s3Key),
		Body:   cr,
	}
	if c.cfg.SSEEnabled {
		input.ServerSideEncryption = types.ServerSideEncryptionAwsKms
		input.SSEKMSKeyId = aws.String(c.cfg.SSEKMSKeyID)
	}

	c.log.Debugf("%sS3 upload start: bucket=%s key=%s path=%s ebid=%s partSize=%d concurrency=%d tagging=%t sse=%t",
		tag, c.cfg.BucketName, s3Key, hanaPath, ebid, c.cfg.UploadPartSize, c.cfg.UploadConcurrency, c.cfg.Tagging, c.cfg.SSEEnabled)

	_, err := uploader.Upload(ctx, input)
	if err != nil {
		return 0, s3Key, fmt.Errorf("upload %s (key %s): %w", hanaPath, s3Key, err)
	}

	if c.cfg.Tagging {
		tagSet := c.buildTagSet(tag)
		_, tagErr := c.svc.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
			Bucket:  aws.String(c.cfg.BucketName),
			Key:     aws.String(s3Key),
			Tagging: &types.Tagging{TagSet: tagSet},
		})
		if tagErr != nil {
			c.log.Warnf("%sS3 tagging failed: key=%s: %v", tag, s3Key, tagErr)
		} else {
			c.log.Debugf("%sS3 tagging applied: key=%s tags=%d", tag, s3Key, len(tagSet))
		}
	}

	c.log.Debugf("%sS3 upload done: key=%s bytes=%d", tag, s3Key, cr.n)
	return cr.n, s3Key, nil
}

// RestoreObject downloads an object from S3 and writes it to dst.
// If ebid is empty, the latest version for hanaPath is resolved automatically.
func (c *S3Client) RestoreObject(ctx context.Context, hanaPath string, ebid string, dst io.Writer, tag string) (int64, string, error) {
	resolvedEbid := ebid
	if resolvedEbid == "" {
		c.log.Debugf("%sRestore #NULL: resolving latest EBID for %s", tag, hanaPath)
		objects, err := c.ListVersionsForPath(ctx, hanaPath)
		if err != nil {
			return 0, "", fmt.Errorf("list versions for #NULL restore of %s: %w", hanaPath, err)
		}
		resolvedEbid = objects[0].ExternalBackupID // sorted latest-first
		c.log.Debugf("%sRestore #NULL: resolved EBID=%s for %s", tag, resolvedEbid, hanaPath)
	}

	s3Key := c.buildS3ObjectKey(hanaPath, resolvedEbid)
	c.log.Debugf("%sS3 restore start: bucket=%s key=%s", tag, c.cfg.BucketName, s3Key)

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
	c.log.Debugf("%sS3 restore done: key=%s bytes=%d", tag, s3Key, written)
	return written, resolvedEbid, nil
}

// DeleteObject removes a specific backup version from S3.
// A HeadObject pre-flight check is performed first so that a non-existent key
// returns ErrNotFound (→ #NOTFOUND) rather than silently succeeding, as CEPH RGW
// DeleteObject is idempotent and returns 200 even for missing keys.
func (c *S3Client) DeleteObject(ctx context.Context, hanaPath string, ebid string, tag string) error {
	if ebid == "" {
		return fmt.Errorf("ebid cannot be empty for delete")
	}
	s3Key := c.buildS3ObjectKey(hanaPath, ebid)
	c.log.Debugf("%sS3 delete: bucket=%s key=%s", tag, c.cfg.BucketName, s3Key)

	_, err := c.svc.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.cfg.BucketName),
		Key:    aws.String(s3Key),
	})
	if err != nil {
		if isS3NotFound(err) {
			c.log.Debugf("%sS3 delete: key=%s not found (pre-flight)", tag, s3Key)
			return &ErrNotFound{S3Key: s3Key}
		}
		return fmt.Errorf("head object before delete %s: %w", s3Key, err)
	}

	_, err = c.svc.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.cfg.BucketName),
		Key:    aws.String(s3Key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return &ErrNotFound{S3Key: s3Key}
		}
		return fmt.Errorf("delete object %s: %w", s3Key, err)
	}
	c.log.Debugf("%sS3 delete done: key=%s", tag, s3Key)
	return nil
}

// InquireObject returns the last-modified time for a specific backup version.
func (c *S3Client) InquireObject(ctx context.Context, hanaPath string, ebid string, tag string) (time.Time, error) {
	if ebid == "" {
		return time.Time{}, fmt.Errorf("ebid cannot be empty for inquire")
	}
	s3Key := c.buildS3ObjectKey(hanaPath, ebid)
	c.log.Debugf("%sS3 inquire: bucket=%s key=%s", tag, c.cfg.BucketName, s3Key)

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
	c.log.Debugf("%sS3 inquire done: key=%s lastModified=%v", tag, s3Key, *head.LastModified)
	return *head.LastModified, nil
}

// ListVersionsForPath returns all backup versions for hanaPath, sorted newest-first.
func (c *S3Client) ListVersionsForPath(ctx context.Context, hanaPath string) ([]ObjectInfo, error) {
	prefix := c.buildS3Prefix(hanaPath)
	c.log.Debugf("%sS3 list versions: bucket=%s prefix=%q", c.tag, c.cfg.BucketName, prefix)

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
	c.log.Debugf("%sS3 list versions done: prefix=%q count=%d", c.tag, prefix, len(objects))
	return objects, nil
}

// ListAllForTenant returns all backup objects under a tenant-scoped prefix, sorted newest-first.
func (c *S3Client) ListAllForTenant(ctx context.Context, userID string) ([]ObjectInfo, error) {
	basePrefix := ""
	if c.cfg.FolderName != "" {
		basePrefix = c.cfg.FolderName + "/"
	}

	if c.cfg.ShortenFolderPath {
		// Paths are shortened; SID is prepended to the key: [FolderName/]<SID>/<DBNAME>/<file>/<EBID>.bak
		// Scope the listing to the SID prefix so only this system's objects are returned.
		if c.sid != "" {
			basePrefix = path.Join(basePrefix, c.sid) + "/"
		}
		c.log.Debugf("%sListAllForTenant with shorten_folder_path=true: using SID-scoped prefix=%q", c.tag, basePrefix)
	} else {
		// Derive a specific prefix from the SID in "DBNAME@SID" format.
		if parts := strings.Split(userID, "@"); len(parts) == 2 && parts[1] != "" {
			basePrefix = path.Join(basePrefix, fmt.Sprintf("usr/sap/%s/", parts[1]))
		} else if userID != "" && !strings.Contains(userID, "@") {
			basePrefix = path.Join(basePrefix, fmt.Sprintf("usr/sap/%s/", userID))
		} else {
			if c.cfg.FolderName == "" {
				basePrefix = "usr/sap/"
			}
			c.log.Warnf("%scould not derive SID from userID=%q, using broad prefix=%q", c.tag, userID, basePrefix)
		}
		basePrefix = strings.TrimRight(basePrefix, "/") + "/"
	}

	c.log.Debugf("%sS3 list all for tenant: bucket=%s prefix=%q", c.tag, c.cfg.BucketName, basePrefix)

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
			// When shorten_folder_path=true, SID is prepended to the key — strip it
			// so the returned FileName matches the original HANA path segment.
			if c.cfg.ShortenFolderPath && c.sid != "" {
				pathWithoutFolder = strings.TrimPrefix(pathWithoutFolder, c.sid+"/")
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
	c.log.Debugf("%sS3 list all for tenant done: prefix=%q count=%d", c.tag, basePrefix, len(objects))
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

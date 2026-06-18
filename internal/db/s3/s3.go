// Package s3 is the treeman driver for S3-compatible object stores
// (AWS S3, MinIO, Garage, Ceph RGW, Backblaze B2, Cloudflare R2,
// anything that speaks the S3 API).
//
// Scope is intentionally narrow: per-worktree BUCKET lifecycle —
// create on prepare, drop (with all keys) on teardown. No object-level
// snapshot/restore, no branch-scoped swap, no dump/load. The
// "namespace" for a treeman s3 entry is a BUCKET NAME RENDERED FROM
// `key_prefix`; this driver's prefix operations match buckets whose
// names begin with that string.
package s3

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"golang.org/x/sync/errgroup"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/containerip"
	"github.com/stubbedev/treeman/internal/db/reachability"
)

// Driver wraps an aws-sdk-go-v2 S3 client. Buckets are addressed by
// rendered name; there's no per-bucket session state to carry.
type Driver struct {
	Client      *awss3.Client
	Region      string
	EndpointSet bool
}

// Connect builds an S3 client from cfg and probes the endpoint for
// reachability. When `cfg.Container` / `cfg.ComposeService` is set,
// the endpoint URL's host/port are rewritten using the container's
// published port or bridge IP (same machinery as the ES driver).
//
// Reachability probe is best-effort: a TCP connect against the
// endpoint's host:port. For AWS S3 (no explicit endpoint) the probe
// is skipped — the SDK's regional endpoint is discovered lazily.
func Connect(ctx context.Context, cfg config.S3Conn) (*Driver, error) {
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	endpoint := cfg.Endpoint
	if cfg.Container != "" || cfg.ComposeService != "" {
		opts := containerip.Opts{
			Container:      cfg.Container,
			ComposeService: cfg.ComposeService,
			ComposeProject: cfg.ComposeProject,
			Engine:         cfg.ContainerEngine,
			Network:        cfg.Network,
			InternalPort:   containerip.URIPort(endpoint, 9000),
		}
		addr, err := containerip.ResolveAddr(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("resolve container: %w", err)
		}
		if addr != nil {
			endpoint = containerip.RewriteHostPortInURIWithPort(endpoint, addr.Host, addr.Port)
		}
	}

	if endpoint != "" {
		if err := reachability.ProbeURLCtx(ctx, "s3", endpoint); err != nil {
			return nil, err
		}
	}

	// Both creds must be supplied together. Passing one half to the
	// static-cred provider produces a cryptic SigV4 signing error at
	// the first API call; surface the misconfig here instead.
	hasAK, hasSK := cfg.AccessKey != "", cfg.SecretKey != ""
	if hasAK != hasSK {
		which := "secret_key"
		if hasSK {
			which = "access_key"
		}
		return nil, fmt.Errorf(
			"s3: %s is empty but the other half is set — provide both, or neither (to use the SDK default credential chain)",
			which,
		)
	}

	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(region),
	}
	if hasAK && hasSK {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("s3: load aws config: %w", err)
	}

	endpointSet := endpoint != ""
	client := awss3.NewFromConfig(awsCfg, func(o *awss3.Options) {
		if endpointSet {
			o.BaseEndpoint = &endpoint
		}
		o.UsePathStyle = cfg.UsePathStyle
		o.HTTPClient = defaultHTTPClient
	})
	return &Driver{Client: client, Region: region, EndpointSet: endpointSet}, nil
}

// EngineVersion returns "" + nil — S3 has no version endpoint, and
// the observed `Server` header varies wildly between AWS, MinIO,
// Garage, R2, etc. Stubbed so the engine fits the same shape as the
// other drivers' status output.
func (d *Driver) EngineVersion(_ context.Context) (string, error) {
	return "", nil
}

// BucketExists reports whether `name` exists and is accessible to
// the configured credentials.
func (d *Driver) BucketExists(ctx context.Context, name string) (bool, error) {
	_, err := d.Client.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: &name})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("s3: head bucket %q: %w", name, err)
}

// EnsureBucket creates `name` if it doesn't already exist. The
// "already owned by you" / "already exists" responses are treated as
// success — this is the idempotent prepare path.
//
// AWS S3 quirks: CreateBucket in us-east-1 must NOT set a
// LocationConstraint; every other AWS region requires one. Self-hosted
// implementations (MinIO, Garage, Ceph RGW) interpret region freely
// and reject arbitrary LocationConstraint values, so we only emit the
// constraint when the SDK is talking to AWS (no explicit endpoint).
func (d *Driver) EnsureBucket(ctx context.Context, name string) error {
	in := &awss3.CreateBucketInput{Bucket: &name}
	if !d.EndpointSet && d.Region != "" && d.Region != "us-east-1" {
		in.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraint(d.Region),
		}
	}
	_, err := d.Client.CreateBucket(ctx, in)
	if err == nil {
		return nil
	}
	if isAlreadyOwned(err) {
		return nil
	}
	return fmt.Errorf("s3: create bucket %q: %w", name, err)
}

// ListMatching returns every bucket whose name starts with `prefix`.
// Buckets are an account-wide flat namespace, so this is a simple
// `ListBuckets` + filter — there's no `ListBuckets(prefix=...)` API.
func (d *Driver) ListMatching(ctx context.Context, prefix string) ([]string, error) {
	out, err := d.Client.ListBuckets(ctx, &awss3.ListBucketsInput{})
	if err != nil {
		return nil, fmt.Errorf("s3: list buckets: %w", err)
	}
	var matches []string
	for _, b := range out.Buckets {
		if b.Name == nil {
			continue
		}
		if strings.HasPrefix(*b.Name, prefix) {
			matches = append(matches, *b.Name)
		}
	}
	return matches, nil
}

// DropMatching deletes every bucket whose name starts with `prefix`,
// emptying each one first (S3 forbids DeleteBucket while the bucket
// is non-empty). Returns the names that were removed. Errors short-
// circuit, leaving the remaining buckets in place so the caller can
// retry without losing track of which got dropped.
//
// SAFETY: bucket namespace is account-wide and prefix matching is
// literal — a short or generic prefix (e.g. "dev") will drop unrelated
// buckets in the same account. Callers must enforce a sufficiently
// specific prefix; the package-level minimum is `MinDropPrefixLen`,
// enforced here as a defense-in-depth guard.
func (d *Driver) DropMatching(ctx context.Context, prefix string) ([]string, error) {
	if len(prefix) < MinDropPrefixLen {
		return nil, fmt.Errorf(
			"s3: refusing to drop with prefix %q: length %d < MinDropPrefixLen=%d (would risk reaping unrelated buckets in the account)",
			prefix,
			len(prefix),
			MinDropPrefixLen,
		)
	}
	matches, err := d.ListMatching(ctx, prefix)
	if err != nil {
		return nil, err
	}
	var dropped []string
	for _, name := range matches {
		if err := d.emptyBucket(ctx, name); err != nil {
			return dropped, err
		}
		if _, err := d.Client.DeleteBucket(ctx, &awss3.DeleteBucketInput{Bucket: &name}); err != nil {
			if isNotFound(err) {
				dropped = append(dropped, name)
				continue
			}
			return dropped, fmt.Errorf("s3: delete bucket %q: %w", name, err)
		}
		dropped = append(dropped, name)
	}
	return dropped, nil
}

// emptyBucket deletes every object (and every object version + every
// in-progress multipart upload) in `name`.
//
// The version-aware walk via ListObjectVersions covers both versioned
// and unversioned buckets on AWS and MinIO. Garage (and some other
// S3-compatible stores) don't implement ListObjectVersions and answer
// 501 NotImplemented — those are unversioned, so we fall back to the
// universally-supported ListObjectsV2 walk. abortMultiparts is
// best-effort for the same reason: a backend without multipart-upload
// listing has none to abort.
func (d *Driver) emptyBucket(ctx context.Context, name string) error {
	if err := d.abortMultiparts(ctx, name); err != nil && !isNotImplemented(err) {
		return err
	}
	err := d.emptyVersioned(ctx, name)
	if isNotImplemented(err) {
		return d.emptyUnversioned(ctx, name)
	}
	return err
}

// emptyVersioned drains the bucket via ListObjectVersions (deletes
// every version + delete-marker). Used on backends that support
// versioning APIs (AWS, MinIO).
func (d *Driver) emptyVersioned(ctx context.Context, name string) error {
	var keyMarker, versionMarker *string
	return d.drainBucket(ctx, name, func(gctx context.Context) ([]s3types.ObjectIdentifier, bool, error) {
		out, err := d.Client.ListObjectVersions(gctx, &awss3.ListObjectVersionsInput{
			Bucket:          &name,
			KeyMarker:       keyMarker,
			VersionIdMarker: versionMarker,
		})
		if err != nil {
			return nil, false, fmt.Errorf("s3: list versions %q: %w", name, err)
		}
		batch := make([]s3types.ObjectIdentifier, 0, len(out.Versions)+len(out.DeleteMarkers))
		for _, v := range out.Versions {
			batch = append(batch, s3types.ObjectIdentifier{Key: v.Key, VersionId: v.VersionId})
		}
		for _, m := range out.DeleteMarkers {
			batch = append(batch, s3types.ObjectIdentifier{Key: m.Key, VersionId: m.VersionId})
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			return batch, false, nil
		}
		if eqStrPtr(out.NextKeyMarker, keyMarker) && eqStrPtr(out.NextVersionIdMarker, versionMarker) {
			return nil, false, fmt.Errorf("s3: list versions %q: pagination stuck (IsTruncated=true but markers unchanged)", name)
		}
		keyMarker = out.NextKeyMarker
		versionMarker = out.NextVersionIdMarker
		return batch, true, nil
	})
}

// emptyUnversioned drains the bucket via ListObjectsV2 (current objects
// only). The fallback for backends that don't implement
// ListObjectVersions, which are unversioned, so there are no historical
// versions to miss.
func (d *Driver) emptyUnversioned(ctx context.Context, name string) error {
	var token *string
	return d.drainBucket(ctx, name, func(gctx context.Context) ([]s3types.ObjectIdentifier, bool, error) {
		out, err := d.Client.ListObjectsV2(gctx, &awss3.ListObjectsV2Input{
			Bucket:            &name,
			ContinuationToken: token,
		})
		if err != nil {
			return nil, false, fmt.Errorf("s3: list objects %q: %w", name, err)
		}
		batch := make([]s3types.ObjectIdentifier, 0, len(out.Contents))
		for _, o := range out.Contents {
			batch = append(batch, s3types.ObjectIdentifier{Key: o.Key})
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			return batch, false, nil
		}
		if eqStrPtr(out.NextContinuationToken, token) || out.NextContinuationToken == nil {
			return nil, false, fmt.Errorf("s3: list objects %q: pagination stuck (IsTruncated=true but token unchanged)", name)
		}
		token = out.NextContinuationToken
		return batch, true, nil
	})
}

// drainBucket runs a list/delete pipeline: `page` is called repeatedly
// to fetch the next batch of object identifiers (and report whether more
// pages remain), while deletes for already-listed pages run
// concurrently. Page N+1 lists while page N deletes, instead of stalling
// each round-trip behind the other. The errgroup limit bounds both
// in-flight DeleteObjects requests and the number of buffered pages held
// in memory (g.Go blocks once the limit is hit), so a million-object
// bucket can't balloon the heap. A delete failure cancels gctx, which
// aborts the next page() call.
//
// A NotFound from page() (bucket vanished mid-drain) ends the walk
// cleanly; any other list error is returned after in-flight deletes
// drain. A delete error is preferred over the list error, since a delete
// failure cancels gctx and the list error is then the downstream
// "context canceled" symptom.
func (d *Driver) drainBucket(
	ctx context.Context,
	name string,
	page func(ctx context.Context) ([]s3types.ObjectIdentifier, bool, error),
) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(emptyDeleteConcurrency)
	var listErr error
	for {
		ids, more, err := page(gctx)
		if err != nil {
			if !isNotFound(err) {
				listErr = err
			}
			break
		}
		if len(ids) > 0 {
			batch := ids
			g.Go(func() error { return d.deleteBatch(gctx, name, batch) })
		}
		if !more {
			break
		}
	}
	if werr := g.Wait(); werr != nil {
		return werr
	}
	return listErr
}

// deleteBatch issues one DeleteObjects call per 1000-key chunk (the
// S3 limit). Per-key errors within a chunk are aggregated via
// errors.Join so the caller sees every failure in a single response,
// not just the first.
func (d *Driver) deleteBatch(ctx context.Context, bucket string, ids []s3types.ObjectIdentifier) error {
	const maxKeys = 1000
	for len(ids) > 0 {
		n := min(len(ids), maxKeys)
		chunk := ids[:n]
		ids = ids[n:]
		out, err := d.Client.DeleteObjects(ctx, &awss3.DeleteObjectsInput{
			Bucket: &bucket,
			Delete: &s3types.Delete{Objects: chunk, Quiet: quietTrue},
		})
		if err != nil {
			return fmt.Errorf("s3: delete objects in %q: %w", bucket, err)
		}
		var perKey []error
		for _, e := range out.Errors {
			key, code, msg := "", "", ""
			if e.Key != nil {
				key = *e.Key
			}
			if e.Code != nil {
				code = *e.Code
			}
			if e.Message != nil {
				msg = *e.Message
			}
			perKey = append(perKey, fmt.Errorf("s3: delete object %s/%s: %s (%s)", bucket, key, msg, code))
		}
		if len(perKey) > 0 {
			return errors.Join(perKey...)
		}
	}
	return nil
}

func (d *Driver) abortMultiparts(ctx context.Context, name string) error {
	var (
		keyMarker      *string
		uploadIDMarker *string
	)
	for {
		out, err := d.Client.ListMultipartUploads(ctx, &awss3.ListMultipartUploadsInput{
			Bucket:         &name,
			KeyMarker:      keyMarker,
			UploadIdMarker: uploadIDMarker,
		})
		if err != nil {
			if isNotFound(err) {
				return nil
			}
			return fmt.Errorf("s3: list multipart uploads %q: %w", name, err)
		}
		for _, u := range out.Uploads {
			if _, abErr := d.Client.AbortMultipartUpload(ctx, &awss3.AbortMultipartUploadInput{
				Bucket:   &name,
				Key:      u.Key,
				UploadId: u.UploadId,
			}); abErr != nil {
				return fmt.Errorf("s3: abort multipart in %q: %w", name, abErr)
			}
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			return nil
		}
		if eqStrPtr(out.NextKeyMarker, keyMarker) && eqStrPtr(out.NextUploadIdMarker, uploadIDMarker) {
			return fmt.Errorf("s3: list multipart uploads %q: pagination stuck (IsTruncated=true but markers unchanged)", name)
		}
		keyMarker = out.NextKeyMarker
		uploadIDMarker = out.NextUploadIdMarker
	}
}

func isNotFound(err error) bool {
	var nsk *s3types.NoSuchBucket
	if errors.As(err, &nsk) {
		return true
	}
	var nf *s3types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchBucket", "NoSuchKey", "NotFound":
			return true
		}
	}
	return false
}

// isNotImplemented reports whether the backend answered an S3 action
// with 501 NotImplemented — e.g. Garage rejecting ListObjectVersions
// because it has no versioning support. Used to pick the unversioned
// fallback path rather than treating it as a hard failure.
func isNotImplemented(err error) bool {
	if err == nil {
		return false
	}
	var ae smithy.APIError
	if errors.As(err, &ae) && ae.ErrorCode() == "NotImplemented" {
		return true
	}
	var re *awshttp.ResponseError
	if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotImplemented {
		return true
	}
	return false
}

func isAlreadyOwned(err error) bool {
	var owned *s3types.BucketAlreadyOwnedByYou
	if errors.As(err, &owned) {
		return true
	}
	var exists *s3types.BucketAlreadyExists
	if errors.As(err, &exists) {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "BucketAlreadyOwnedByYou", "BucketAlreadyExists":
			return true
		}
	}
	return false
}

func eqStrPtr(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// MinDropPrefixLen is the shortest prefix DropMatching will accept.
// S3 buckets share an account-wide namespace, so a generic prefix
// like "dev" or "test" would reap unrelated buckets. Callers should
// also enforce this at config-load time on the literal portion of
// `key_prefix` (everything before the first template token).
const MinDropPrefixLen = 6

// emptyDeleteConcurrency bounds the in-flight DeleteObjects requests
// (and, via errgroup backpressure, the number of buffered list pages)
// while emptying a bucket. 8 keeps a single large teardown saturating
// the connection without overwhelming a small self-hosted MinIO/Garage
// node or tripping AWS request-rate throttling.
const emptyDeleteConcurrency = 8

var (
	quietTrue = aws.Bool(true)

	// defaultHTTPClient — shared HTTP client tuned for the S3 API:
	// 30s per-request ceiling, default transport otherwise. Mirrors
	// the timeout the ES driver uses.
	defaultHTTPClient = &http.Client{Timeout: 30 * time.Second}

	// Bucket naming: lowercase letters, digits, hyphens. Must start
	// and end with alphanumeric. 3-63 chars. We deliberately reject
	// dots (legal on AWS in path-style but break virtual-host SSL
	// and are disallowed by several non-AWS impls).
	bucketNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
)

// ValidateBucketName checks a rendered bucket name against the
// conservative subset of S3 naming rules: 3-63 chars, lowercase
// alphanumeric + hyphen, starts and ends alphanumeric. Called from
// prepareS3 after `key_prefix` template render so a slug-derived
// uppercase / overlong / dotted bucket name fails with a clear
// treeman error instead of a downstream `InvalidBucketName` from
// CreateBucket.
func ValidateBucketName(name string) error {
	if n := len(name); n < 3 || n > 63 {
		return fmt.Errorf("bucket name %q: length %d, must be 3-63 chars", name, n)
	}
	if !bucketNameRE.MatchString(name) {
		return fmt.Errorf(
			"bucket name %q: must be lowercase alphanumeric with hyphens, starting and ending alphanumeric (no dots, no uppercase, no underscores)",
			name,
		)
	}
	return nil
}

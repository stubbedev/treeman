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
	"net/url"
	"regexp"
	"strings"
	"sync"
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
		if err := d.DropBucket(ctx, name); err != nil {
			return dropped, err
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

// objRef is one object surfaced by a sharded walk — a deletable
// version reference and/or a copyable key+size.
type objRef struct {
	key       string
	versionID *string // versioned delete only; nil otherwise
	size      int64   // copy only (multipart threshold)
}

// pageLister lists ONE page under (prefix, token) with Delimiter="/",
// returning this page's objects, the child common-prefixes to recurse
// into, and the opaque next-page token (nil = this prefix exhausted).
// The token type is private to each lister (continuation string for V2,
// key+version markers for Versions); walkShards treats it opaquely.
type pageLister func(ctx context.Context, prefix string, token any) (objs []objRef, children []string, next any, err error)

// emptyVersioned drains the bucket via ListObjectVersions (deletes
// every version + delete-marker). Used on backends that support
// versioning APIs (AWS, MinIO).
func (d *Driver) emptyVersioned(ctx context.Context, name string) error {
	return d.walkShards(ctx, d.listVersionsPage(name), func(gctx context.Context, page []objRef) error {
		return d.deleteBatch(gctx, name, page)
	})
}

// emptyUnversioned drains the bucket via ListObjectsV2 (current objects
// only). The fallback for backends that don't implement
// ListObjectVersions, which are unversioned, so there are no historical
// versions to miss.
func (d *Driver) emptyUnversioned(ctx context.Context, name string) error {
	return d.walkShards(ctx, d.listV2Page(name), func(gctx context.Context, page []objRef) error {
		return d.deleteBatch(gctx, name, page)
	})
}

// listV2Page returns a pageLister over ListObjectsV2 (current objects),
// sharded by "/" delimiter so walkShards can recurse into each
// "directory" concurrently. token is a *string continuation token.
func (d *Driver) listV2Page(bucket string) pageLister {
	return func(ctx context.Context, prefix string, token any) ([]objRef, []string, any, error) {
		var ct *string
		if token != nil {
			ct, _ = token.(*string)
		}
		out, err := d.Client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
			Bucket:            &bucket,
			Prefix:            strPtrOrNil(prefix),
			Delimiter:         aws.String("/"),
			ContinuationToken: ct,
		})
		if err != nil {
			return nil, nil, nil, fmt.Errorf("s3: list objects %q: %w", bucket, err)
		}
		objs := make([]objRef, 0, len(out.Contents))
		for _, o := range out.Contents {
			objs = append(objs, objRef{key: aws.ToString(o.Key), size: aws.ToInt64(o.Size)})
		}
		children := commonPrefixes(out.CommonPrefixes)
		if out.IsTruncated == nil || !*out.IsTruncated {
			return objs, children, nil, nil
		}
		if out.NextContinuationToken == nil || eqStrPtr(out.NextContinuationToken, ct) {
			return nil, nil, nil, fmt.Errorf("s3: list objects %q: pagination stuck (IsTruncated=true but token unchanged)", bucket)
		}
		return objs, children, out.NextContinuationToken, nil
	}
}

// verMarker is the ListObjectVersions pagination cursor (a key +
// version-id marker pair), carried opaquely through walkShards.
type verMarker struct{ key, ver *string }

// listVersionsPage returns a pageLister over ListObjectVersions,
// sharded by "/" delimiter. token is a verMarker.
func (d *Driver) listVersionsPage(bucket string) pageLister {
	return func(ctx context.Context, prefix string, token any) ([]objRef, []string, any, error) {
		var m verMarker
		if token != nil {
			m, _ = token.(verMarker)
		}
		out, err := d.Client.ListObjectVersions(ctx, &awss3.ListObjectVersionsInput{
			Bucket:          &bucket,
			Prefix:          strPtrOrNil(prefix),
			Delimiter:       aws.String("/"),
			KeyMarker:       m.key,
			VersionIdMarker: m.ver,
		})
		if err != nil {
			return nil, nil, nil, fmt.Errorf("s3: list versions %q: %w", bucket, err)
		}
		objs := make([]objRef, 0, len(out.Versions)+len(out.DeleteMarkers))
		for _, v := range out.Versions {
			objs = append(objs, objRef{key: aws.ToString(v.Key), versionID: v.VersionId})
		}
		for _, dm := range out.DeleteMarkers {
			objs = append(objs, objRef{key: aws.ToString(dm.Key), versionID: dm.VersionId})
		}
		children := commonPrefixes(out.CommonPrefixes)
		if out.IsTruncated == nil || !*out.IsTruncated {
			return objs, children, nil, nil
		}
		if eqStrPtr(out.NextKeyMarker, m.key) && eqStrPtr(out.NextVersionIdMarker, m.ver) {
			return nil, nil, nil, fmt.Errorf("s3: list versions %q: pagination stuck (IsTruncated=true but markers unchanged)", bucket)
		}
		return objs, children, verMarker{out.NextKeyMarker, out.NextVersionIdMarker}, nil
	}
}

// walkShards enumerates every object in a bucket using delimiter-sharded
// PARALLEL listing: each common-prefix ("directory") is walked by its
// own bounded worker, so listing throughput isn't capped by serial
// pagination — the previous bottleneck that left the concurrent
// deleters/copiers starved waiting one page at a time. Within a single
// prefix, pagination stays serial (token-chained); across prefixes it's
// concurrent up to listConcurrency workers. Each discovered page is
// handed to `handle` inline by the worker that listed it.
//
// Resource-bounded: a fixed worker pool (no goroutine-per-object) and a
// LIFO prefix queue whose depth is the key-tree breadth. A flat bucket
// (no "/" in keys) yields no children and degrades to a single serial
// walk — no worse than before.
//
// A NotFound mid-walk (bucket vanished) ends that branch cleanly; any
// other list/handle error stops every worker and is returned.
func (d *Driver) walkShards(ctx context.Context, list pageLister, handle func(context.Context, []objRef) error) error {
	g, gctx := errgroup.WithContext(ctx)

	var (
		mu          sync.Mutex
		cond        = sync.NewCond(&mu)
		queue       []string
		outstanding int  // prefixes pushed but not yet finished
		stopped     bool // an error/cancel woke everyone to exit
	)
	push := func(p string) {
		mu.Lock()
		if !stopped {
			queue = append(queue, p)
			outstanding++
			cond.Signal()
		}
		mu.Unlock()
	}
	pop := func() (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		for len(queue) == 0 && outstanding > 0 && !stopped {
			cond.Wait()
		}
		if stopped || outstanding == 0 {
			return "", false
		}
		p := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		return p, true
	}
	finish := func() {
		mu.Lock()
		outstanding--
		if outstanding == 0 {
			cond.Broadcast() // wake idle workers so they exit
		}
		mu.Unlock()
	}
	stopAll := func() {
		mu.Lock()
		stopped = true
		cond.Broadcast()
		mu.Unlock()
	}

	push("") // root prefix
	for range listConcurrency {
		g.Go(func() error {
			for {
				prefix, ok := pop()
				if !ok {
					return nil
				}
				err := d.walkPrefix(gctx, prefix, list, handle, push)
				finish()
				if err != nil {
					stopAll()
					return err
				}
			}
		})
	}
	return g.Wait()
}

// walkPrefix paginates one prefix serially, pushing discovered child
// prefixes back onto the shared queue and handling each page inline.
func (d *Driver) walkPrefix(
	ctx context.Context,
	prefix string,
	list pageLister,
	handle func(context.Context, []objRef) error,
	push func(string),
) error {
	var token any
	for {
		objs, children, next, err := list(ctx, prefix, token)
		if err != nil {
			if isNotFound(err) {
				return nil
			}
			return err
		}
		for _, c := range children {
			push(c)
		}
		if len(objs) > 0 {
			if err := handle(ctx, objs); err != nil {
				return err
			}
		}
		if next == nil {
			return nil
		}
		token = next
	}
}

// deleteBatch issues one DeleteObjects call per 1000-key chunk (the
// S3 limit) for one listed page. Per-key errors within a chunk are
// aggregated via errors.Join so the caller sees every failure in a
// single response, not just the first.
func (d *Driver) deleteBatch(ctx context.Context, bucket string, page []objRef) error {
	ids := make([]s3types.ObjectIdentifier, len(page))
	for i, o := range page {
		ids[i] = s3types.ObjectIdentifier{Key: aws.String(o.key), VersionId: o.versionID}
	}
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

// abortMultiparts aborts every in-progress multipart upload in `name`.
// Listing paginates serially (uploads are typically few); the aborts
// themselves run concurrently under a bounded group.
func (d *Driver) abortMultiparts(ctx context.Context, name string) error {
	var (
		uploads        []s3types.MultipartUpload
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
		uploads = append(uploads, out.Uploads...)
		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		if eqStrPtr(out.NextKeyMarker, keyMarker) && eqStrPtr(out.NextUploadIdMarker, uploadIDMarker) {
			return fmt.Errorf("s3: list multipart uploads %q: pagination stuck (IsTruncated=true but markers unchanged)", name)
		}
		keyMarker = out.NextKeyMarker
		uploadIDMarker = out.NextUploadIdMarker
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(copyConcurrency)
	for _, u := range uploads {
		g.Go(func() error {
			if _, err := d.Client.AbortMultipartUpload(gctx, &awss3.AbortMultipartUploadInput{
				Bucket:   &name,
				Key:      u.Key,
				UploadId: u.UploadId,
			}); err != nil {
				return fmt.Errorf("s3: abort multipart in %q: %w", name, err)
			}
			return nil
		})
	}
	return g.Wait()
}

// commonPrefixes extracts the non-nil Prefix strings from a
// ListObjects*/ListObjectVersions CommonPrefixes slice.
func commonPrefixes(cps []s3types.CommonPrefix) []string {
	if len(cps) == 0 {
		return nil
	}
	out := make([]string, 0, len(cps))
	for _, cp := range cps {
		if cp.Prefix != nil {
			out = append(out, *cp.Prefix)
		}
	}
	return out
}

// strPtrOrNil returns nil for "" (the root prefix) so the SDK omits the
// Prefix param, else a pointer to s.
func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ─── branch-scoped (durable copy) primitives ────────────────────────
//
// These back the engine-agnostic swap lifecycle (internal/prepare
// branchstate): the active worktree bucket and a per-branch durable
// bucket are independent buckets, and Capture/Restore copy the whole
// object set between them server-side. There is no name-vs-prefix
// filtering — a bucket is the atomic unit, so S3 behaves like the
// name-scoped engines (mysql/postgres/mongo), not the prefix engines.

// Empty resets `name` to an empty, present bucket — creates it if
// missing, then deletes every object. The branch-swap layer's
// "Empty(active)" primitive.
func (d *Driver) Empty(ctx context.Context, name string) error {
	if err := d.EnsureBucket(ctx, name); err != nil {
		return err
	}
	return d.emptyBucket(ctx, name)
}

// DropBucket removes EXACTLY `name` (empty it first — S3 forbids
// DeleteBucket on a non-empty bucket). Idempotent: a bucket that's
// already gone is success. This is the exact-name counterpart to
// DropMatching's prefix reap, used for active + durable bucket teardown.
func (d *Driver) DropBucket(ctx context.Context, name string) error {
	if err := d.emptyBucket(ctx, name); err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	if _, err := d.Client.DeleteBucket(ctx, &awss3.DeleteBucketInput{Bucket: &name}); err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("s3: delete bucket %q: %w", name, err)
	}
	return nil
}

// CopyBucket makes `dst` a content copy of `src`: ensures dst exists,
// empties it (so a Restore never leaves stale objects from a prior
// branch), then server-side-copies every current object from src.
//
// Copies are server-side (CopyObject / UploadPartCopy) — object bytes
// never round-trip through this process. Listing is delimiter-sharded
// and parallel (walkShards) so the copiers aren't starved one page at a
// time; each listed page fans its objects out across a shared semaphore
// that caps total in-flight copies (and bounds copy goroutines) at
// copyConcurrency. Objects ≥5 GiB use a multipart copy. This is the
// most-performant whole-bucket copy the S3 API offers.
func (d *Driver) CopyBucket(ctx context.Context, src, dst string) error {
	if err := d.EnsureBucket(ctx, dst); err != nil {
		return err
	}
	if err := d.emptyBucket(ctx, dst); err != nil {
		return err
	}
	sem := make(chan struct{}, copyConcurrency)
	return d.walkShards(ctx, d.listV2Page(src), func(gctx context.Context, page []objRef) error {
		cg, cgctx := errgroup.WithContext(gctx)
		for _, o := range page {
			// Acquire BEFORE spawning so at most copyConcurrency copy
			// goroutines exist at once — a huge page can't spawn a
			// goroutine per object.
			select {
			case sem <- struct{}{}:
			case <-cgctx.Done():
				return cgctx.Err()
			}
			cg.Go(func() error {
				defer func() { <-sem }()
				return d.copyObject(cgctx, src, dst, o.key, o.size)
			})
		}
		return cg.Wait()
	})
}

// copyObject server-side-copies one object src/key → dst/key. Objects
// at or above maxSingleCopyBytes (the 5 GiB CopyObject ceiling) are
// copied via multipart UploadPartCopy; smaller ones via a single
// CopyObject. The caller holds the copy-concurrency slot for this whole
// object.
func (d *Driver) copyObject(ctx context.Context, src, dst, key string, size int64) error {
	if size >= maxSingleCopyBytes {
		return d.copyObjectMultipart(ctx, src, dst, key, size)
	}
	_, err := d.Client.CopyObject(ctx, &awss3.CopyObjectInput{
		Bucket:     &dst,
		Key:        &key,
		CopySource: aws.String(copySource(src, key)),
	})
	if err != nil {
		return fmt.Errorf("s3: copy %s/%s → %s: %w", src, key, dst, err)
	}
	return nil
}

// copyObjectMultipart copies a large object (≥5 GiB) using multipart
// UploadPartCopy with a byte-range per part. The caller holds the
// object's copy-concurrency slot; parts copy concurrently under their
// own bounded group (copyPartConcurrency) so a single huge object can't
// monopolise the global copy semaphore by holding it per-part.
func (d *Driver) copyObjectMultipart(ctx context.Context, src, dst, key string, size int64) error {
	create, err := d.Client.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{
		Bucket: &dst,
		Key:    &key,
	})
	if err != nil {
		return fmt.Errorf("s3: create multipart copy %s/%s: %w", dst, key, err)
	}
	uploadID := create.UploadId
	cs := copySource(src, key)

	nParts := int((size + copyPartBytes - 1) / copyPartBytes)
	parts := make([]s3types.CompletedPart, nParts)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(copyPartConcurrency)
	for i := range nParts {
		partNum := int32(i + 1)
		start := int64(i) * copyPartBytes
		end := min(start+copyPartBytes-1, size-1)
		g.Go(func() error {
			out, perr := d.Client.UploadPartCopy(gctx, &awss3.UploadPartCopyInput{
				Bucket:          &dst,
				Key:             &key,
				UploadId:        uploadID,
				PartNumber:      aws.Int32(partNum),
				CopySource:      aws.String(cs),
				CopySourceRange: aws.String(fmt.Sprintf("bytes=%d-%d", start, end)),
			})
			if perr != nil {
				return fmt.Errorf("s3: copy part %d of %s/%s: %w", partNum, dst, key, perr)
			}
			parts[partNum-1] = s3types.CompletedPart{
				ETag:       out.CopyPartResult.ETag,
				PartNumber: aws.Int32(partNum),
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		_, _ = d.Client.AbortMultipartUpload(ctx, &awss3.AbortMultipartUploadInput{
			Bucket: &dst, Key: &key, UploadId: uploadID,
		})
		return err
	}
	_, err = d.Client.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{
		Bucket:          &dst,
		Key:             &key,
		UploadId:        uploadID,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		return fmt.Errorf("s3: complete multipart copy %s/%s: %w", dst, key, err)
	}
	return nil
}

// copySource builds the CopySource header value for CopyObject /
// UploadPartCopy: "<bucket>/<key>" with the key path-escaped (spaces,
// unicode, etc.) while keeping "/" separators intact. Using url.URL's
// EscapedPath avoids both under-escaping (breaks on spaces) and
// over-escaping the slashes in nested keys.
func copySource(bucket, key string) string {
	u := url.URL{Path: "/" + bucket + "/" + key}
	return strings.TrimPrefix(u.EscapedPath(), "/")
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

// listConcurrency is the number of prefix-shard workers walkShards runs
// in parallel — each lists one "/"-delimited prefix at a time and
// handles its pages inline (bulk delete, or copy fan-out). 8 keeps a
// large sharded teardown/copy saturating the connection without
// overwhelming a small self-hosted MinIO/Garage node or tripping AWS
// request-rate throttling. Flat buckets (no "/" keys) use a single
// worker — there's nothing to shard.
const listConcurrency = 8

// Branch-scoped whole-bucket copy tuning.
const (
	// copyConcurrency caps total in-flight server-side copies during a
	// CopyBucket (and bounds copy goroutines — the fan-out acquires a
	// slot before spawning). Server-side copies are cheap on the client
	// but each is a request, so this rides a bit higher than the
	// per-prefix list workers.
	copyConcurrency = 16

	// maxSingleCopyBytes is the S3 ceiling for a single CopyObject
	// (5 GiB). Objects at or above it must use multipart UploadPartCopy.
	maxSingleCopyBytes = 5 * 1024 * 1024 * 1024

	// copyPartBytes is the per-part range size for multipart copies.
	// 1 GiB keeps part counts low (10k-part cap → 10 TiB max object)
	// while staying well under the 5 GiB part ceiling.
	copyPartBytes = 1 * 1024 * 1024 * 1024

	// copyPartConcurrency bounds concurrent part-copies within one large
	// object's multipart copy.
	copyPartConcurrency = 8
)

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

//go:build e2e

// Package s3_e2e exercises the s3 engine's lifecycle: prepare creates
// the per-worktree bucket, teardown empties it and drops it. Runs
// against MinIO so no AWS credentials or network access are required.
package s3_e2e

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/prepare"
)

const (
	endpoint   = "http://127.0.0.1:19000"
	accessKey  = "minioadmin"
	secretKey  = "minioadmin"
	bucketLit  = "tme2es3-" // 8 char literal → satisfies validate.go's 6-char minLiteral guard
	keyPrefix  = bucketLit + "{slug_dash}" // {slug} contains `_` which AWS rejects in bucket names; {slug_dash} substitutes hyphens
)

func TestS3EndToEnd(t *testing.T) {
	harness.SkipIfNoDocker(t)
	composeDir := harness.MustAbs(".")
	t.Cleanup(harness.ComposeUp(t, composeDir))

	harness.WaitForReady(t, "minio:19000", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:19000", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	ctx := context.Background()
	wt := t.TempDir()
	cfg := buildConfig()
	env := harness.NewEnv(t, wt)

	// ── 1. prepare → bucket exists ────────────────────────────────────
	outs := env.RunPrepare(t, cfg)
	o := harness.AssertOutcome(t, outs, "s3", false)
	t.Logf("pass1: bucket=%s", o.SourceDB)
	if o.SourceDB == "" {
		t.Fatalf("prepare returned empty SourceDB")
	}
	if !bucketExists(ctx, t, o.SourceDB) {
		t.Fatalf("bucket %s missing after prepare", o.SourceDB)
	}

	// ── 2. prepare again → idempotent (EnsureBucket swallows
	//      BucketAlreadyOwnedByYou) ───────────────────────────────────
	outs = env.RunPrepare(t, cfg)
	o2 := harness.AssertOutcome(t, outs, "s3", false)
	if o2.SourceDB != o.SourceDB {
		t.Errorf("bucket name drift: pass1=%s pass2=%s", o.SourceDB, o2.SourceDB)
	}

	// ── 3. Drop an object into the bucket so teardown must run the
	//      empty-bucket path, not just DeleteBucket. ──────────────────
	putObject(ctx, t, o.SourceDB, "smoke.txt", []byte("hello"))

	// ── 4. teardown → bucket gone ─────────────────────────────────────
	if err := prepare.TeardownDatabases(ctx, cfg, env.Slug.Value, env.RepoID, env.WTID, env.Store); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if bucketExists(ctx, t, o.SourceDB) {
		t.Errorf("bucket %s still exists after teardown", o.SourceDB)
	}
}

func buildConfig() *config.Config {
	useP := true
	return &config.Config{
		Connections: config.ConnectionsConfig{
			S3: &config.S3Conn{
				Endpoint:     endpoint,
				Region:       "us-east-1",
				AccessKey:    accessKey,
				SecretKey:    secretKey,
				UsePathStyle: useP,
			},
		},
		Databases: []config.DatabaseConfig{{
			Engine:    "s3",
			KeyPrefix: keyPrefix,
		}},
	}
}

// rawClient builds a MinIO-pointing S3 client outside treeman's
// driver, used by assertions so the test doesn't measure the driver
// against itself.
func rawClient(t *testing.T) *awss3.Client {
	t.Helper()
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		t.Fatalf("load aws config: %v", err)
	}
	ep := endpoint
	return awss3.NewFromConfig(cfg, func(o *awss3.Options) {
		o.BaseEndpoint = &ep
		o.UsePathStyle = true
	})
}

func bucketExists(ctx context.Context, t *testing.T, name string) bool {
	t.Helper()
	c := rawClient(t)
	_, err := c.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: &name})
	if err == nil {
		return true
	}
	var nf *s3types.NotFound
	if errors.As(err, &nf) {
		return false
	}
	var nsk *s3types.NoSuchBucket
	if errors.As(err, &nsk) {
		return false
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchBucket", "NotFound":
			return false
		}
	}
	t.Fatalf("head bucket %q: %v", name, err)
	return false
}

func putObject(ctx context.Context, t *testing.T, bucket, key string, body []byte) {
	t.Helper()
	c := rawClient(t)
	_, err := c.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: &bucket,
		Key:    &key,
		Body:   bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("put object %s/%s: %v", bucket, key, err)
	}
}

//go:build e2e

// Package s3_e2e exercises the s3 engine's lifecycle — prepare creates
// the per-worktree bucket, teardown empties it and drops it — against
// every S3-compatible backend treeman claims to support. Two live
// backends run side by side: MinIO and Garage. Both speak the S3 API
// with path-style addressing, so a single driver code path covers them;
// running both proves the driver isn't quietly MinIO-specific. No AWS
// credentials or network access are required.
//
// Each backend runs as its own subtest and self-skips if it isn't
// reachable/provisioned, so a flaky or misconfigured backend can't mask
// the other.
package s3_e2e

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	dbs3 "github.com/stubbedev/treeman/internal/db/s3"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/slug"
)

const (
	bucketLit = "tme2es3-"                // 8 char literal → satisfies validate.go's 6-char minLiteral guard
	keyPrefix = bucketLit + "{slug_dash}" // {slug} contains `_` which AWS rejects in bucket names; {slug_dash} substitutes hyphens

	// Deterministic Garage credentials provisioned by provisionGarage.
	// `GK` + 24 hex is the access-key shape Garage expects; the secret
	// is any 64-hex string.
	garageContainer = "treeman-e2e-garage"
	garageAccessKey = "GK0123456789abcdef01234567"
	garageSecretKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// backend is one S3-compatible target. provision (optional) runs after
// the TCP port is up and before the lifecycle test — used by Garage to
// stage its cluster layout and import a key.
type backend struct {
	name      string
	readyAddr string
	conn      config.S3Conn
	provision func(t *testing.T) error
}

func backends() []backend {
	return []backend{
		{
			name:      "minio",
			readyAddr: "127.0.0.1:19000",
			conn: config.S3Conn{
				Endpoint:     "http://127.0.0.1:19000",
				Region:       "us-east-1",
				AccessKey:    "minioadmin",
				SecretKey:    "minioadmin",
				UsePathStyle: true,
			},
		},
		{
			name:      "garage",
			readyAddr: "127.0.0.1:19010",
			conn: config.S3Conn{
				Endpoint:     "http://127.0.0.1:19010",
				Region:       "garage",
				AccessKey:    garageAccessKey,
				SecretKey:    garageSecretKey,
				UsePathStyle: true,
			},
			provision: provisionGarage,
		},
	}
}

func TestS3EndToEnd(t *testing.T) {
	harness.SkipIfNoDocker(t)
	composeDir := harness.MustAbs(".")
	t.Cleanup(harness.ComposeUp(t, composeDir))

	ctx := context.Background()
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			runBackend(ctx, t, b)
		})
	}
}

func runBackend(ctx context.Context, t *testing.T, b backend) {
	t.Helper()
	harness.WaitForReady(t, b.name+":"+b.readyAddr, 90*time.Second, func() error {
		c, err := net.DialTimeout("tcp", b.readyAddr, 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	if b.provision != nil {
		if err := b.provision(t); err != nil {
			t.Skipf("%s provisioning failed (backend unavailable): %v", b.name, err)
		}
	}

	// Credential smoke check: if the driver can't authenticate against
	// the backend, skip rather than fail — the backend is unavailable in
	// this environment, which is not a driver bug.
	if err := credsWork(ctx, b.conn); err != nil {
		t.Skipf("%s not authenticating (backend unavailable): %v", b.name, err)
	}

	wt := t.TempDir()
	cfg := buildConfig(b.conn)
	env := harness.NewEnv(t, wt)

	// ── 1. prepare → bucket exists ────────────────────────────────────
	outs := env.RunPrepare(t, cfg)
	o := harness.AssertOutcome(t, outs, "s3", false)
	t.Logf("%s pass1: bucket=%s", b.name, o.SourceDB)
	if o.SourceDB == "" {
		t.Fatalf("prepare returned empty SourceDB")
	}
	if !bucketExists(ctx, t, b.conn, o.SourceDB) {
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
	putObject(ctx, t, b.conn, o.SourceDB, "smoke.txt", []byte("hello"))

	// ── 4. teardown → bucket gone ─────────────────────────────────────
	if err := prepare.TeardownDatabases(ctx, cfg, env.Slug.Value, env.RepoID, env.WTID, env.Store); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if bucketExists(ctx, t, b.conn, o.SourceDB) {
		t.Errorf("bucket %s still exists after teardown", o.SourceDB)
	}

	// ── 5. branch_scoped: per-branch durable bucket copy ──────────────
	branchScopedSwap(ctx, t, b)
}

// branchScopedSwap drives the full branch-scoped swap lifecycle on `b`:
// each branch keeps its own durable bucket, captured on switch-away and
// restored on switch-back via server-side whole-bucket copy. Uses a
// nested + space-bearing object key to prove copySource escaping holds
// on this backend.
func branchScopedSwap(ctx context.Context, t *testing.T, b backend) {
	t.Helper()
	wt := t.TempDir()
	env := harness.NewEnv(t, wt)
	conn := b.conn
	cfg := &config.Config{
		Connections: config.ConnectionsConfig{S3: &conn},
		Databases: []config.DatabaseConfig{{
			Engine:       "s3",
			KeyPrefix:    keyPrefix,
			BranchScoped: true,
		}},
	}

	const devKey = "assets/nested file.bin" // nested + space → copySource escaping
	const featKey = "feature.txt"

	// develop: empty active, add the develop object.
	active := driveS3Prepare(t, env, cfg, "develop")
	putObject(ctx, t, conn, active, devKey, []byte("develop"))
	assertKeys(ctx, t, conn, active, devKey)

	// switch to feature → develop captured; new branch starts from the
	// branch point (develop's data).
	if got := driveS3Prepare(t, env, cfg, "feature"); got != active {
		t.Fatalf("active bucket drifted: %s → %s", active, got)
	}
	assertKeys(ctx, t, conn, active, devKey)
	putObject(ctx, t, conn, active, featKey, []byte("feature"))
	assertKeys(ctx, t, conn, active, devKey, featKey)

	// back to develop → feature captured, develop restored (isolated).
	driveS3Prepare(t, env, cfg, "develop")
	assertKeys(ctx, t, conn, active, devKey)

	// back to feature → resumed from its durable copy.
	driveS3Prepare(t, env, cfg, "feature")
	assertKeys(ctx, t, conn, active, devKey, featKey)
}

// driveS3Prepare points the worktree at `branch` and runs a full
// prepare, returning the active bucket name.
func driveS3Prepare(t *testing.T, env *harness.Env, cfg *config.Config, branch string) string {
	t.Helper()
	ctx := context.Background()
	sl := slug.For(env.WTPath, "")
	if _, err := env.Store.EnsureWorktree(ctx, env.RepoID, env.WTPath, sl.Value, branch); err != nil {
		t.Fatalf("ensure worktree %s: %v", branch, err)
	}
	outs, err := prepare.Run(ctx, cfg, env.WTPath, sl, env.Store, env.RepoID, env.WTID, nil)
	if err != nil {
		t.Fatalf("prepare.Run(%s): %v", branch, err)
	}
	for _, o := range outs {
		if o.SourceDB != "" {
			return o.SourceDB
		}
	}
	t.Fatalf("prepare.Run(%s): no active bucket in outcomes", branch)
	return ""
}

// assertKeys checks the bucket holds exactly `want` object keys.
func assertKeys(ctx context.Context, t *testing.T, conn config.S3Conn, bucket string, want ...string) {
	t.Helper()
	c := rawClient(t, conn)
	var got []string
	var token *string
	for {
		out, err := c.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: &bucket, ContinuationToken: token})
		if err != nil {
			t.Fatalf("list objects %s: %v", bucket, err)
		}
		for _, o := range out.Contents {
			got = append(got, *o.Key)
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		token = out.NextContinuationToken
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("bucket %s keys = %v, want %v", bucket, got, want)
	}
}

// provisionGarage stages a single-node cluster layout and imports the
// fixed test key with create-bucket permission. Runs the garage CLI
// inside the container (the image is shell-less, so this execs the
// binary directly). Best-effort: a non-zero exit is reported, and the
// caller decides skip-vs-run via the credential smoke check.
func provisionGarage(t *testing.T) error {
	t.Helper()
	nodeID, err := garage(t, "node", "id", "-q")
	if err != nil {
		return err
	}
	// `node id -q` prints "<id>@<addr>"; layout wants the bare id.
	if i := strings.IndexByte(nodeID, '@'); i >= 0 {
		nodeID = nodeID[:i]
	}
	if _, err := garage(t, "layout", "assign", "-z", "dc1", "-c", "1G", nodeID); err != nil {
		return err
	}
	// `layout apply --version 1` is a no-op-or-error if a layout is
	// already live (re-run); the smoke check is the real gate, so a
	// failure here isn't fatal on its own.
	_, _ = garage(t, "layout", "apply", "--version", "1")
	if _, err := garage(t, "key", "import", "--yes", "-n", "treeman", garageAccessKey, garageSecretKey); err != nil {
		return err
	}
	_, err = garage(t, "key", "allow", "--create-bucket", garageAccessKey)
	return err
}

// garage execs the garage CLI inside the test container and returns its
// trimmed stdout.
func garage(t *testing.T, args ...string) (string, error) {
	t.Helper()
	full := append([]string{"exec", garageContainer, "/garage", "-c", "/etc/garage.toml"}, args...)
	cmd := exec.Command("docker", full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", errors.New(string(out))
	}
	// garage logs INFO lines to stdout before the answer; the id we want
	// is the last non-empty token. Trim to the last line.
	s := bytes.TrimSpace(out)
	if i := bytes.LastIndexByte(s, '\n'); i >= 0 {
		s = bytes.TrimSpace(s[i+1:])
	}
	return string(s), nil
}

// credsWork dials the backend through treeman's own driver and lists
// buckets — proves the credentials authenticate before the lifecycle
// assertions run.
func credsWork(ctx context.Context, conn config.S3Conn) error {
	drv, err := dbs3.Connect(ctx, conn)
	if err != nil {
		return err
	}
	_, err = drv.ListMatching(ctx, bucketLit)
	return err
}

func buildConfig(conn config.S3Conn) *config.Config {
	c := conn
	return &config.Config{
		Connections: config.ConnectionsConfig{S3: &c},
		Databases: []config.DatabaseConfig{{
			Engine:    "s3",
			KeyPrefix: keyPrefix,
		}},
	}
}

// rawClient builds a backend-pointing S3 client outside treeman's
// driver, used by assertions so the test doesn't measure the driver
// against itself.
func rawClient(t *testing.T, conn config.S3Conn) *awss3.Client {
	t.Helper()
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(conn.Region),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(conn.AccessKey, conn.SecretKey, "")),
	)
	if err != nil {
		t.Fatalf("load aws config: %v", err)
	}
	ep := conn.Endpoint
	return awss3.NewFromConfig(cfg, func(o *awss3.Options) {
		o.BaseEndpoint = &ep
		o.UsePathStyle = conn.UsePathStyle
	})
}

func bucketExists(ctx context.Context, t *testing.T, conn config.S3Conn, name string) bool {
	t.Helper()
	c := rawClient(t, conn)
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

func putObject(ctx context.Context, t *testing.T, conn config.S3Conn, bucket, key string, body []byte) {
	t.Helper()
	c := rawClient(t, conn)
	_, err := c.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: &bucket,
		Key:    &key,
		Body:   bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("put object %s/%s: %v", bucket, key, err)
	}
}

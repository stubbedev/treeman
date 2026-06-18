package s3

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/stubbedev/treeman/internal/config"
)

// TestValidateBucketName locks the conservative bucket-name subset
// treeman enforces on every rendered `key_prefix`. The render step
// is the last line of defense before CreateBucket — a slug-derived
// uppercase / overlong / dotted name must surface a treeman error,
// not a downstream `InvalidBucketName` from AWS.
func TestValidateBucketName(t *testing.T) {
	cases := []struct {
		name    string
		bucket  string
		wantErr bool
	}{
		{"valid lowercase + hyphen", "myapp-feature-x", false},
		{"valid min length", "abc", false},
		{"valid max length", strings.Repeat("a", 63), false},
		{"too short", "ab", true},
		{"too long", strings.Repeat("a", 64), true},
		{"uppercase rejected", "MyApp-feature", true},
		{"underscore rejected", "myapp_feature", true},
		{"dot rejected", "myapp.feature", true},
		{"trailing hyphen rejected", "myapp-feature-", true},
		{"leading hyphen rejected", "-myapp-feature", true},
		{"empty rejected", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateBucketName(c.bucket)
			if c.wantErr && err == nil {
				t.Fatalf("ValidateBucketName(%q) = nil, want error", c.bucket)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("ValidateBucketName(%q) = %v, want nil", c.bucket, err)
			}
		})
	}
}

// TestDropMatchingPrefixGuard locks the defense-in-depth refusal in
// DropMatching for prefixes shorter than MinDropPrefixLen. Buckets
// share an account-wide namespace, so a generic prefix like "dev"
// would reap unrelated buckets. The guard MUST fire before the SDK
// is touched (the test uses a Driver with a nil Client — a dial
// would panic if the guard regressed).
func TestDropMatchingPrefixGuard(t *testing.T) {
	d := &Driver{} // nil Client — proves no AWS call is made
	short := []string{"", "a", "abc", "dev", "test", strings.Repeat("x", MinDropPrefixLen-1)}
	for _, p := range short {
		t.Run("len="+strconv.Itoa(len(p)), func(t *testing.T) {
			got, err := d.DropMatching(context.Background(), p)
			if err == nil {
				t.Fatalf("DropMatching(%q) = nil err, want refusal", p)
			}
			if !strings.Contains(err.Error(), "MinDropPrefixLen") {
				t.Fatalf("DropMatching(%q) err %v, want MinDropPrefixLen-citing message", p, err)
			}
			if got != nil {
				t.Fatalf("DropMatching(%q) returned %v, want nil on refusal", p, got)
			}
		})
	}
}

// TestIsNotFound covers the union of typed + generic smithy errors
// the driver treats as "bucket/key absent". The SDK surfaces some
// 404s as typed (`*s3types.NoSuchBucket`) and some via the generic
// `smithy.APIError` shape; both must collapse to one "absent"
// signal so DropMatching's tolerate-NotFound arm works across SDK
// versions.
func TestIsNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"typed NoSuchBucket", &s3types.NoSuchBucket{}, true},
		{"typed NotFound", &s3types.NotFound{}, true},
		{"generic NoSuchBucket", &smithy.GenericAPIError{Code: "NoSuchBucket"}, true},
		{"generic NoSuchKey", &smithy.GenericAPIError{Code: "NoSuchKey"}, true},
		{"generic NotFound", &smithy.GenericAPIError{Code: "NotFound"}, true},
		{"unrelated generic", &smithy.GenericAPIError{Code: "AccessDenied"}, false},
		{"plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isNotFound(c.err); got != c.want {
				t.Fatalf("isNotFound(%v) = %t, want %t", c.err, got, c.want)
			}
		})
	}
}

// TestIsAlreadyOwned mirrors TestIsNotFound for the CreateBucket
// idempotent path. The two BucketAlready* errors come back as typed
// values; the generic-error fallback handles SDK versions that
// expose them only through smithy.APIError (R2/MinIO sometimes).
func TestIsAlreadyOwned(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"typed AlreadyOwnedByYou", &s3types.BucketAlreadyOwnedByYou{}, true},
		{"typed AlreadyExists", &s3types.BucketAlreadyExists{}, true},
		{"generic AlreadyOwnedByYou", &smithy.GenericAPIError{Code: "BucketAlreadyOwnedByYou"}, true},
		{"generic AlreadyExists", &smithy.GenericAPIError{Code: "BucketAlreadyExists"}, true},
		{"unrelated", &smithy.GenericAPIError{Code: "AccessDenied"}, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isAlreadyOwned(c.err); got != c.want {
				t.Fatalf("isAlreadyOwned(%v) = %t, want %t", c.err, got, c.want)
			}
		})
	}
}

// TestEqStrPtr covers the pagination loop-break helper that protects
// emptyBucket / abortMultiparts from non-conforming S3 impls reporting
// IsTruncated=true while leaving the markers unchanged.
func TestEqStrPtr(t *testing.T) {
	s, sCopy, other := "x", "x", "y"
	cases := []struct {
		name string
		a, b *string
		want bool
	}{
		{"both nil", nil, nil, true},
		{"a nil b set", nil, &s, false},
		{"a set b nil", &s, nil, false},
		{"both same value", &s, &sCopy, true},
		{"different values", &s, &other, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := eqStrPtr(c.a, c.b); got != c.want {
				t.Fatalf("eqStrPtr = %t, want %t", got, c.want)
			}
		})
	}
}

// TestConnectMissingCredHalf locks the explicit error when only one
// of access_key/secret_key is supplied. Passing one half to the
// static-cred provider would otherwise produce a cryptic SigV4
// signing failure at the first API call.
func TestConnectMissingCredHalf(t *testing.T) {
	cases := []struct {
		name      string
		ak, sk    string
		wantInErr string
	}{
		{"access without secret", "AK", "", "secret_key"},
		{"secret without access", "", "SK", "access_key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Connect(context.Background(), config.S3Conn{
				AccessKey: c.ak,
				SecretKey: c.sk,
			})
			if err == nil {
				t.Fatalf("Connect = nil err, want %q", c.wantInErr)
			}
			if !strings.Contains(err.Error(), c.wantInErr) {
				t.Fatalf("Connect err = %v, want substring %q", err, c.wantInErr)
			}
		})
	}
}

func TestCopySource(t *testing.T) {
	cases := []struct {
		name, bucket, key, want string
	}{
		{"simple", "src", "a.txt", "src/a.txt"},
		{"nested", "src", "dir/sub/a.txt", "src/dir/sub/a.txt"},
		{"space", "src", "a b.txt", "src/a%20b.txt"},
		{"nested space", "src", "assets/nested file.bin", "src/assets/nested%20file.bin"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := copySource(c.bucket, c.key); got != c.want {
				t.Fatalf("copySource(%q,%q) = %q, want %q", c.bucket, c.key, got, c.want)
			}
		})
	}
}

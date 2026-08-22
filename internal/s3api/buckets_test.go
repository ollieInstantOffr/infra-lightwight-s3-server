package s3api

import (
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// apiErrorCode extracts the S3 error code an SDK saw, which is what client code
// branches on.
func apiErrorCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}

func TestBucketLifecycle(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()

	// Create.
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String("my-test-bucket"),
	}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	// Exists.
	if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String("my-test-bucket"),
	}); err != nil {
		t.Fatalf("HeadBucket: %v", err)
	}

	// Appears in the listing.
	list, err := client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	if len(list.Buckets) != 1 || aws.ToString(list.Buckets[0].Name) != "my-test-bucket" {
		t.Fatalf("ListBuckets returned %d buckets, want the one just created", len(list.Buckets))
	}
	if list.Buckets[0].CreationDate == nil {
		t.Error("CreationDate is nil; clients display it")
	}

	// Delete.
	if _, err := client.DeleteBucket(ctx, &s3.DeleteBucketInput{
		Bucket: aws.String("my-test-bucket"),
	}); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}

	// Gone.
	_, err = client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("my-test-bucket")})
	if err == nil {
		t.Fatal("HeadBucket succeeded after the bucket was deleted")
	}
}

func TestListBucketsWhenEmpty(t *testing.T) {
	client, _ := newIntegrationServer(t)

	list, err := client.ListBuckets(t.Context(), &s3.ListBucketsInput{})
	if err != nil {
		t.Fatalf("ListBuckets on an empty server: %v", err)
	}
	if len(list.Buckets) != 0 {
		t.Errorf("got %d buckets, want 0", len(list.Buckets))
	}
}

// Recreating a bucket you already own is reported as BucketAlreadyOwnedByYou,
// which the SDKs treat as benign, rather than as a generic conflict.
func TestCreateExistingBucket(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()

	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String("duplicate-bucket"),
	}); err != nil {
		t.Fatalf("first CreateBucket: %v", err)
	}

	_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String("duplicate-bucket"),
	})
	if err == nil {
		t.Fatal("recreating an existing bucket succeeded silently")
	}
	var owned *types.BucketAlreadyOwnedByYou
	if !errors.As(err, &owned) {
		t.Errorf("error code = %q, want BucketAlreadyOwnedByYou", apiErrorCode(err))
	}
}

func TestHeadMissingBucket(t *testing.T) {
	client, _ := newIntegrationServer(t)

	_, err := client.HeadBucket(t.Context(), &s3.HeadBucketInput{
		Bucket: aws.String("no-such-bucket"),
	})
	if err == nil {
		t.Fatal("HeadBucket succeeded for a bucket that does not exist")
	}
	// HEAD has no body, so the SDK can only see the status. It must still be a
	// recognisable not-found rather than a 500.
	var notFound *types.NotFound
	if !errors.As(err, &notFound) && apiErrorCode(err) != "NotFound" {
		t.Errorf("error = %v (code %q), want NotFound", err, apiErrorCode(err))
	}
}

func TestDeleteMissingBucket(t *testing.T) {
	client, _ := newIntegrationServer(t)

	_, err := client.DeleteBucket(t.Context(), &s3.DeleteBucketInput{
		Bucket: aws.String("no-such-bucket"),
	})
	if err == nil {
		t.Fatal("DeleteBucket succeeded for a bucket that does not exist")
	}
	if code := apiErrorCode(err); code != "NoSuchBucket" {
		t.Errorf("error code = %q, want NoSuchBucket", code)
	}
}

func TestGetBucketLocation(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()

	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String("located-bucket"),
	}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	out, err := client.GetBucketLocation(ctx, &s3.GetBucketLocationInput{
		Bucket: aws.String("located-bucket"),
	})
	if err != nil {
		t.Fatalf("GetBucketLocation: %v", err)
	}
	// us-east-1 is encoded as an empty constraint, exactly as S3 does.
	if out.LocationConstraint != "" {
		t.Errorf("LocationConstraint = %q, want empty for us-east-1", out.LocationConstraint)
	}
}

// Bucket names are constrained by DNS, not just by taste. The server must
// reject the same names S3 does, or a bucket created here would be unusable
// against real S3.
func TestBucketNameValidation(t *testing.T) {
	valid := []string{
		"abc", "my-bucket", "my.bucket.name", "bucket123", "1bucket",
		strings.Repeat("a", 63),
	}
	for _, name := range valid {
		if err := ValidateBucketName(name); err != nil {
			t.Errorf("ValidateBucketName(%q) = %v, want nil", name, err)
		}
	}

	invalid := map[string]string{
		"ab":                    "too short",
		strings.Repeat("a", 64): "too long",
		"MyBucket":              "uppercase",
		"my_bucket":             "underscore",
		"-bucket":               "leading hyphen",
		"bucket-":               "trailing hyphen",
		".bucket":               "leading period",
		"bucket.":               "trailing period",
		"my..bucket":            "consecutive periods",
		"my.-bucket":            "period next to hyphen",
		"my-.bucket":            "hyphen next to period",
		"192.168.1.1":           "IP address shaped",
		"xn--bucket":            "reserved prefix",
		"sthree-bucket":         "reserved prefix",
		"bucket-s3alias":        "reserved suffix",
		"bucket--ol-s3":         "reserved suffix",
		"bucket with spaces":    "spaces",
		"bucket/slash":          "slash",
	}
	for name, why := range invalid {
		if err := ValidateBucketName(name); err == nil {
			t.Errorf("ValidateBucketName(%q) accepted a name that is invalid: %s", name, why)
		}
	}
}

// The rejection has to reach the client as InvalidBucketName, not as a database
// constraint violation surfacing as a 500.
func TestCreateBucketWithInvalidNameIsRejected(t *testing.T) {
	client, _ := newIntegrationServer(t)

	_, err := client.CreateBucket(t.Context(), &s3.CreateBucketInput{
		Bucket: aws.String("Invalid_Bucket_Name"),
	})
	if err == nil {
		t.Fatal("CreateBucket accepted an invalid bucket name")
	}
	if code := apiErrorCode(err); code != "InvalidBucketName" {
		t.Errorf("error code = %q, want InvalidBucketName", code)
	}
}

// httpStatusOf extracts the HTTP status from an SDK error, which is how
// conditional-request outcomes (304, 412) are observed: the SDK surfaces them
// as errors rather than as responses.
func httpStatusOf(err error) int {
	var responseErr *awshttp.ResponseError
	if errors.As(err, &responseErr) {
		return responseErr.HTTPStatusCode()
	}
	return 0
}

package s3api

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// seedKeys uploads a set of keys with trivial bodies.
func seedKeys(t *testing.T, client *s3.Client, bucket string, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if _, err := client.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket: aws.String(bucket), Key: aws.String(key),
			Body: strings.NewReader("x"),
		}); err != nil {
			t.Fatalf("PutObject(%q): %v", key, err)
		}
	}
}

func listedKeys(out *s3.ListObjectsV2Output) []string {
	keys := make([]string, 0, len(out.Contents))
	for _, o := range out.Contents {
		keys = append(keys, aws.ToString(o.Key))
	}
	return keys
}

func listedPrefixes(out *s3.ListObjectsV2Output) []string {
	prefixes := make([]string, 0, len(out.CommonPrefixes))
	for _, p := range out.CommonPrefixes {
		prefixes = append(prefixes, aws.ToString(p.Prefix))
	}
	return prefixes
}

func TestListObjectsFlat(t *testing.T) {
	client, _ := newIntegrationServer(t)
	makeBucket(t, client, "listing")
	seedKeys(t, client, "listing", "c.txt", "a.txt", "b.txt")

	out, err := client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
		Bucket: aws.String("listing"),
	})
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}

	got := listedKeys(out)
	want := []string{"a.txt", "b.txt", "c.txt"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("keys = %v, want %v (S3 returns keys in sorted order)", got, want)
	}
	if aws.ToInt32(out.KeyCount) != 3 {
		t.Errorf("KeyCount = %d, want 3", aws.ToInt32(out.KeyCount))
	}
	if aws.ToBool(out.IsTruncated) {
		t.Error("IsTruncated is true for a complete listing")
	}
	for _, o := range out.Contents {
		if o.LastModified == nil || o.ETag == nil || o.Size == nil {
			t.Errorf("entry %q is missing LastModified, ETag or Size", aws.ToString(o.Key))
		}
		if string(o.StorageClass) != storageClassStandard {
			t.Errorf("StorageClass = %q, want STANDARD", o.StorageClass)
		}
	}
}

// The delimiter is what turns a flat key space into browsable folders, and is
// the single most-used listing feature.
func TestListObjectsWithDelimiter(t *testing.T) {
	client, _ := newIntegrationServer(t)
	makeBucket(t, client, "listing")
	seedKeys(t, client, "listing",
		"root.txt",
		"photos/2025/jan.jpg",
		"photos/2025/feb.jpg",
		"photos/2026/mar.jpg",
		"docs/readme.md",
	)

	out, err := client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
		Bucket: aws.String("listing"), Delimiter: aws.String("/"),
	})
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}

	if got, want := listedKeys(out), []string{"root.txt"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("keys = %v, want %v (only top-level keys)", got, want)
	}
	got := listedPrefixes(out)
	sort.Strings(got)
	if want := []string{"docs/", "photos/"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("common prefixes = %v, want %v", got, want)
	}
	// KeyCount counts objects and prefixes together, as S3 does.
	if aws.ToInt32(out.KeyCount) != 3 {
		t.Errorf("KeyCount = %d, want 3 (1 key + 2 prefixes)", aws.ToInt32(out.KeyCount))
	}
}

func TestListObjectsWithPrefixAndDelimiter(t *testing.T) {
	client, _ := newIntegrationServer(t)
	makeBucket(t, client, "listing")
	seedKeys(t, client, "listing",
		"photos/2025/jan.jpg",
		"photos/2025/feb.jpg",
		"photos/2026/mar.jpg",
		"photos/cover.jpg",
		"docs/readme.md",
	)

	out, err := client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
		Bucket: aws.String("listing"),
		Prefix: aws.String("photos/"), Delimiter: aws.String("/"),
	})
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}

	if got, want := listedKeys(out), []string{"photos/cover.jpg"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("keys = %v, want %v", got, want)
	}
	got := listedPrefixes(out)
	sort.Strings(got)
	if want := []string{"photos/2025/", "photos/2026/"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("common prefixes = %v, want %v", got, want)
	}
}

// The paginator is where listing usually breaks: a cursor that compares under a
// different ordering than the query silently skips or repeats keys.
func TestListObjectsPaginationVisitsEveryKeyExactlyOnce(t *testing.T) {
	client, _ := newIntegrationServer(t)
	makeBucket(t, client, "listing")

	const total = 250
	keys := make([]string, 0, total)
	for i := range total {
		keys = append(keys, fmt.Sprintf("objects/%04d.txt", i))
	}
	seedKeys(t, client, "listing", keys...)

	seen := make(map[string]int)
	paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: aws.String("listing"), MaxKeys: aws.Int32(17),
	})
	pages := 0
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(t.Context())
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		if pages > 100 {
			t.Fatal("paginator did not terminate; the continuation cursor is not advancing")
		}
		for _, o := range page.Contents {
			seen[aws.ToString(o.Key)]++
		}
	}

	if len(seen) != total {
		t.Errorf("saw %d distinct keys across %d pages, want %d", len(seen), pages, total)
	}
	for key, count := range seen {
		if count != 1 {
			t.Errorf("key %q appeared %d times", key, count)
		}
	}
}

// Pagination must also be correct when the page boundary falls in the middle of
// a set of common prefixes.
func TestListObjectsPaginationWithDelimiter(t *testing.T) {
	client, _ := newIntegrationServer(t)
	makeBucket(t, client, "listing")

	var keys []string
	for folder := range 25 {
		for file := range 4 {
			keys = append(keys, fmt.Sprintf("folder%02d/file%d.txt", folder, file))
		}
	}
	seedKeys(t, client, "listing", keys...)

	seen := make(map[string]int)
	var token *string
	for page := 0; ; page++ {
		if page > 60 {
			t.Fatal("pagination did not terminate")
		}
		out, err := client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
			Bucket: aws.String("listing"), Delimiter: aws.String("/"),
			MaxKeys: aws.Int32(7), ContinuationToken: token,
		})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, p := range out.CommonPrefixes {
			seen[aws.ToString(p.Prefix)]++
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		token = out.NextContinuationToken
	}

	if len(seen) != 25 {
		t.Errorf("saw %d distinct prefixes, want 25", len(seen))
	}
	for prefix, count := range seen {
		if count != 1 {
			t.Errorf("prefix %q appeared %d times", prefix, count)
		}
	}
}

func TestListObjectsStartAfter(t *testing.T) {
	client, _ := newIntegrationServer(t)
	makeBucket(t, client, "listing")
	seedKeys(t, client, "listing", "a.txt", "b.txt", "c.txt", "d.txt")

	out, err := client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
		Bucket: aws.String("listing"), StartAfter: aws.String("b.txt"),
	})
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}
	// start-after is exclusive.
	if got, want := listedKeys(out), []string{"c.txt", "d.txt"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("keys = %v, want %v", got, want)
	}
}

func TestListObjectsEmptyBucket(t *testing.T) {
	client, _ := newIntegrationServer(t)
	makeBucket(t, client, "listing")

	out, err := client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
		Bucket: aws.String("listing"),
	})
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}
	if aws.ToInt32(out.KeyCount) != 0 || len(out.Contents) != 0 {
		t.Errorf("empty bucket returned %d keys", len(out.Contents))
	}
	if aws.ToBool(out.IsTruncated) {
		t.Error("IsTruncated is true for an empty bucket")
	}
}

// S3 orders keys by UTF-8 bytes. A database collation that ignores case or
// punctuation produces a different order, which breaks both client expectations
// and cursor-based pagination.
func TestListObjectsUsesByteOrdering(t *testing.T) {
	client, _ := newIntegrationServer(t)
	makeBucket(t, client, "listing")

	// Under a typical en_US.UTF-8 collation these sort differently: punctuation
	// is treated as secondary and case is folded.
	keys := []string{"Zebra.txt", "apple.txt", "a-dash.txt", "a_underscore.txt", "Apple.txt"}
	seedKeys(t, client, "listing", keys...)

	out, err := client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
		Bucket: aws.String("listing"),
	})
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}

	want := append([]string(nil), keys...)
	sort.Strings(want) // Go sorts strings by bytes, exactly as S3 does
	if got := listedKeys(out); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("keys = %v\nwant %v\n(listing must use UTF-8 byte order, not the database collation)", got, want)
	}
}

// A prefix containing SQL LIKE metacharacters must be treated literally.
func TestListObjectsPrefixWithMetacharacters(t *testing.T) {
	client, _ := newIntegrationServer(t)
	makeBucket(t, client, "listing")
	seedKeys(t, client, "listing", "100%/real.txt", "100X/decoy.txt", "under_score/real.txt", "underXscore/decoy.txt")

	for prefix, want := range map[string][]string{
		"100%/":        {"100%/real.txt"},
		"under_score/": {"under_score/real.txt"},
	} {
		out, err := client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
			Bucket: aws.String("listing"), Prefix: aws.String(prefix),
		})
		if err != nil {
			t.Fatalf("ListObjectsV2(%q): %v", prefix, err)
		}
		if got := listedKeys(out); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("prefix %q returned %v, want %v; metacharacters must be literal", prefix, got, want)
		}
	}
}

func TestListObjectsV1(t *testing.T) {
	client, _ := newIntegrationServer(t)
	makeBucket(t, client, "listing")
	seedKeys(t, client, "listing", "a.txt", "b.txt", "c.txt")

	out, err := client.ListObjects(t.Context(), &s3.ListObjectsInput{
		Bucket: aws.String("listing"), MaxKeys: aws.Int32(2),
	})
	if err != nil {
		t.Fatalf("ListObjects (v1): %v", err)
	}
	if len(out.Contents) != 2 {
		t.Errorf("got %d keys, want 2", len(out.Contents))
	}
	if !aws.ToBool(out.IsTruncated) {
		t.Error("IsTruncated is false despite more keys remaining")
	}
	// V1 paginates with a marker rather than a continuation token.
	if aws.ToString(out.NextMarker) == "" {
		t.Error("NextMarker is empty on a truncated v1 listing")
	}

	next, err := client.ListObjects(t.Context(), &s3.ListObjectsInput{
		Bucket: aws.String("listing"), Marker: out.NextMarker,
	})
	if err != nil {
		t.Fatalf("ListObjects (v1) page 2: %v", err)
	}
	if got, want := []string{aws.ToString(next.Contents[0].Key)}, []string{"c.txt"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("second page = %v, want %v", got, want)
	}
}

// Keys can contain characters that are not legal in XML. encoding-type=url is
// how S3 makes those listable at all.
func TestListObjectsURLEncoding(t *testing.T) {
	client, _ := newIntegrationServer(t)
	makeBucket(t, client, "listing")
	seedKeys(t, client, "listing", "with space.txt", "with+plus.txt")

	out, err := client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
		Bucket: aws.String("listing"), EncodingType: "url",
	})
	if err != nil {
		t.Fatalf("ListObjectsV2 with encoding-type=url: %v", err)
	}

	// The Go SDK does not decode these itself — unlike the JS and Java SDKs,
	// it leaves the encoded form for the application. So the wire format is
	// asserted directly, then decoded here to prove it round-trips.
	if out.EncodingType != "url" {
		t.Errorf("EncodingType = %q, want url; clients key off this to decide whether to decode",
			out.EncodingType)
	}
	got := listedKeys(out)
	sort.Strings(got)
	if want := []string{"with%20space.txt", "with%2Bplus.txt"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("encoded keys = %v, want %v", got, want)
	}

	var decoded []string
	for _, key := range got {
		plain, err := url.QueryUnescape(key)
		if err != nil {
			t.Fatalf("key %q is not valid percent-encoding: %v", key, err)
		}
		decoded = append(decoded, plain)
	}
	sort.Strings(decoded)
	if want := []string{"with space.txt", "with+plus.txt"}; fmt.Sprint(decoded) != fmt.Sprint(want) {
		t.Errorf("decoded keys = %v, want %v", decoded, want)
	}
}

func TestListObjectsRejectsBadContinuationToken(t *testing.T) {
	client, _ := newIntegrationServer(t)
	makeBucket(t, client, "listing")
	seedKeys(t, client, "listing", "a.txt")

	_, err := client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
		Bucket: aws.String("listing"), ContinuationToken: aws.String("!!!not-base64!!!"),
	})
	if err == nil {
		t.Fatal("a malformed continuation token was accepted; a client would loop over page one forever")
	}
}

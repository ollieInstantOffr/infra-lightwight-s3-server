package s3api

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
)

// storageClassStandard is the only class this server has. Objects are stored
// once on local disk; there is no tiering to describe.
const storageClassStandard = "STANDARD"

// encodeContinuationToken hides the cursor behind an opaque token.
//
// S3 documents the token as opaque, and clients treat it that way. Encoding it
// keeps callers from constructing one by hand and depending on the cursor being
// a raw object key, which would make the pagination scheme impossible to change.
func encodeContinuationToken(cursor string) string {
	if cursor == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(cursor))
}

// decodeContinuationToken recovers a cursor. A malformed token is rejected
// rather than silently treated as the start of the bucket, which would make a
// paginating client loop forever over the first page.
func decodeContinuationToken(token string) (string, error) {
	if token == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", ErrInvalidArgument.WithMessage("The continuation token provided is incorrect.")
	}
	return string(raw), nil
}

// handleListObjects serves both ListObjectsV2 and the original ListObjects.
//
// They arrive on the same route and are told apart by list-type=2, which is how
// S3 versions this call.
func (s *Server) handleListObjects(w http.ResponseWriter, r *http.Request, bucket *db.Bucket) {
	query := r.URL.Query()
	isV2 := query.Get("list-type") == "2"

	maxKeys, err := parseMaxKeys(query.Get("max-keys"))
	if err != nil {
		WriteError(w, r, err)
		return
	}

	opts := db.ListOptions{
		Prefix:    query.Get("prefix"),
		Delimiter: query.Get("delimiter"),
		MaxKeys:   maxKeys,
	}

	// V2 resumes from a continuation token, falling back to start-after on the
	// first page. V1 uses a marker, which is the raw key rather than a token.
	var continuationToken, startAfter, marker string
	if isV2 {
		continuationToken = query.Get("continuation-token")
		startAfter = query.Get("start-after")
		if continuationToken != "" {
			cursor, err := decodeContinuationToken(continuationToken)
			if err != nil {
				WriteError(w, r, err)
				return
			}
			opts.StartAfter = cursor
		} else {
			opts.StartAfter = startAfter
		}
	} else {
		marker = query.Get("marker")
		opts.StartAfter = marker
	}

	result, err := db.ListObjects(r.Context(), s.DB, bucket.ID, opts)
	if err != nil {
		s.internal(w, r, "list objects", err)
		return
	}

	// encoding-type=url exists because XML cannot carry certain control
	// characters that are otherwise legal in a key. Clients that ask for it
	// decode on receipt.
	encodeURL := strings.EqualFold(query.Get("encoding-type"), "url")
	encode := func(s string) string {
		if encodeURL {
			return uriEncode(s, true)
		}
		return s
	}

	response := ListBucketResult{
		Xmlns:       s3Namespace,
		Name:        bucket.Name,
		Prefix:      encode(opts.Prefix),
		Delimiter:   encode(opts.Delimiter),
		MaxKeys:     maxKeys,
		IsTruncated: result.IsTruncated,
		Contents:    make([]ObjectEntry, 0, len(result.Objects)),
	}
	if encodeURL {
		response.EncodingType = "url"
	}

	for _, object := range result.Objects {
		response.Contents = append(response.Contents, ObjectEntry{
			Key:          encode(object.Key),
			LastModified: formatXMLTime(object.UpdatedAt),
			ETag:         quoteETag(object.ETag),
			Size:         object.Size,
			StorageClass: storageClassStandard,
		})
	}
	for _, prefix := range result.CommonPrefixes {
		response.CommonPrefixes = append(response.CommonPrefixes, CommonPrefix{Prefix: encode(prefix)})
	}
	response.KeyCount = len(response.Contents) + len(response.CommonPrefixes)

	if isV2 {
		response.ContinuationToken = continuationToken
		response.StartAfter = encode(startAfter)
		if result.IsTruncated {
			response.NextContinuationToken = encodeContinuationToken(result.NextStartAfter)
		}
	} else {
		response.Marker = encode(marker)
		if result.IsTruncated {
			// V1 returns the next marker as a plain key, not a token.
			response.NextMarker = encode(result.NextStartAfter)
		}
	}

	writeXML(w, r, http.StatusOK, response)
}

// parseMaxKeys validates the max-keys parameter.
func parseMaxKeys(raw string) (int, error) {
	if raw == "" {
		return 1000, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, ErrInvalidArgument.WithMessage(
			"Argument max-keys must be an integer between 0 and 1000, got %q.", raw)
	}
	if n > 1000 {
		// S3 silently clamps rather than rejecting, and clients rely on that.
		return 1000, nil
	}
	return n, nil
}

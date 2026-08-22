package s3api

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
)

// The AWS SDKs switch to multipart above roughly 8 MiB without being asked, so
// a server without this simply cannot accept large files. It is also the only
// way to upload something whose size is not known in advance.

const (
	minPartNumber = 1
	maxPartNumber = 10000

	// minPartSize is S3's floor for every part except the last. Enforcing it
	// matters because clients size their parts against this expectation, and a
	// server that quietly accepts smaller ones produces objects that cannot be
	// re-uploaded to real S3 with the same part layout.
	minPartSize = 5 << 20

	// maxCompleteBodySize bounds the CompleteMultipartUpload document. Ten
	// thousand parts of roughly 200 bytes each is the theoretical maximum.
	maxCompleteBodySize = 4 << 20
)

// CompleteMultipartUploadRequest is the body listing the parts to assemble.
type CompleteMultipartUploadRequest struct {
	XMLName xml.Name           `xml:"CompleteMultipartUpload"`
	Parts   []CompletedPartXML `xml:"Part"`
}

// CompletedPartXML is one part reference in a completion request.
type CompletedPartXML struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

// InitiateMultipartUploadResult is the CreateMultipartUpload response.
type InitiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

// CompleteMultipartUploadResult is the completion response.
type CompleteMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

// ListPartsResult is the ListParts response.
type ListPartsResult struct {
	XMLName      xml.Name  `xml:"ListPartsResult"`
	Xmlns        string    `xml:"xmlns,attr"`
	Bucket       string    `xml:"Bucket"`
	Key          string    `xml:"Key"`
	UploadID     string    `xml:"UploadId"`
	StorageClass string    `xml:"StorageClass"`
	MaxParts     int       `xml:"MaxParts"`
	IsTruncated  bool      `xml:"IsTruncated"`
	Parts        []PartXML `xml:"Part"`
}

// PartXML is one part in a ListParts response.
type PartXML struct {
	PartNumber   int    `xml:"PartNumber"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
}

// ListMultipartUploadsResult is the ListMultipartUploads response.
type ListMultipartUploadsResult struct {
	XMLName     xml.Name    `xml:"ListMultipartUploadsResult"`
	Xmlns       string      `xml:"xmlns,attr"`
	Bucket      string      `xml:"Bucket"`
	KeyMarker   string      `xml:"KeyMarker"`
	MaxUploads  int         `xml:"MaxUploads"`
	IsTruncated bool        `xml:"IsTruncated"`
	Uploads     []UploadXML `xml:"Upload"`
}

// UploadXML is one in-progress upload.
type UploadXML struct {
	Key       string `xml:"Key"`
	UploadID  string `xml:"UploadId"`
	Initiated string `xml:"Initiated"`
}

// handleCreateMultipartUpload implements CreateMultipartUpload.
func (s *Server) handleCreateMultipartUpload(w http.ResponseWriter, r *http.Request, bucket *db.Bucket) {
	key := keyOf(r)
	if err := ValidateObjectKey(key); err != nil {
		WriteError(w, r, err)
		return
	}
	metadata, err := userMetadata(r.Header)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	upload := &db.MultipartUpload{
		BucketID:    bucket.ID,
		Key:         key,
		ContentType: contentTypeOf(r),
		Metadata:    metadata,
	}
	if err := db.CreateMultipartUpload(r.Context(), s.DB, upload); err != nil {
		s.internal(w, r, "create multipart upload", err)
		return
	}

	writeXML(w, r, http.StatusOK, InitiateMultipartUploadResult{
		Xmlns:    s3Namespace,
		Bucket:   bucket.Name,
		Key:      key,
		UploadID: upload.UploadID,
	})
}

// handleUploadPart implements UploadPart. Each part streams to its own blob.
func (s *Server) handleUploadPart(w http.ResponseWriter, r *http.Request, bucket *db.Bucket, uploadID string) {
	partNumber, err := parsePartNumber(r.URL.Query().Get("partNumber"))
	if err != nil {
		WriteError(w, r, err)
		return
	}

	upload, err := s.requireUpload(w, r, bucket, uploadID)
	if err != nil {
		return
	}

	identity, ok := IdentityFrom(r.Context())
	if !ok {
		WriteError(w, r, ErrAccessDenied)
		return
	}

	body := s.Verifier.Body(r, identity)
	defer body.Close()

	blob, err := s.Blobs.Put(r.Context(), body)
	if err != nil {
		if apiErr := AsAPIError(err); apiErr != ErrInternalError {
			WriteError(w, r, err)
			return
		}
		s.internal(w, r, "write part data", err)
		return
	}

	part := &db.MultipartPart{
		PartNumber: partNumber,
		BlobDigest: blob.Digest,
		Size:       blob.Size,
		ETag:       blob.ETag,
	}
	if err := db.PutMultipartPart(r.Context(), s.DB, upload.ID, part); err != nil {
		s.internal(w, r, "record part", err)
		return
	}

	w.Header().Set("ETag", quoteETag(blob.ETag))
	w.WriteHeader(http.StatusOK)
}

// handleCompleteMultipartUpload implements CompleteMultipartUpload.
func (s *Server) handleCompleteMultipartUpload(w http.ResponseWriter, r *http.Request, bucket *db.Bucket, uploadID string) {
	upload, err := s.requireUpload(w, r, bucket, uploadID)
	if err != nil {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxCompleteBodySize))
	if err != nil {
		WriteError(w, r, ErrIncompleteBody)
		return
	}
	var request CompleteMultipartUploadRequest
	if err := xml.Unmarshal(body, &request); err != nil {
		WriteError(w, r, ErrMalformedXML)
		return
	}
	if len(request.Parts) == 0 {
		WriteError(w, r, ErrInvalidPart.WithMessage("The completion request listed no parts."))
		return
	}

	uploaded, err := db.ListMultipartParts(r.Context(), s.DB, upload.ID)
	if err != nil {
		s.internal(w, r, "list parts", err)
		return
	}
	byNumber := make(map[int]db.MultipartPart, len(uploaded))
	for _, part := range uploaded {
		byNumber[part.PartNumber] = part
	}

	selected, err := selectParts(request.Parts, byNumber)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	digests := make([]string, 0, len(selected))
	var totalSize int64
	for _, part := range selected {
		digests = append(digests, part.BlobDigest)
		totalSize += part.Size
	}

	// The parts are joined into one addressable blob. Streamed, so a hundred
	// large parts cost no more memory than two small ones.
	assembled, err := s.Blobs.Concat(r.Context(), digests)
	if err != nil {
		s.internal(w, r, "assemble multipart object", err)
		return
	}
	if assembled.Size != totalSize {
		s.internal(w, r, "assemble multipart object",
			fmt.Errorf("assembled %d bytes from parts totalling %d", assembled.Size, totalSize))
		return
	}

	object := &db.Object{
		BucketID:    bucket.ID,
		Key:         upload.Key,
		BlobDigest:  assembled.Digest,
		Size:        assembled.Size,
		ETag:        compositeETag(selected),
		ContentType: upload.ContentType,
		Metadata:    upload.Metadata,
	}
	if err := db.CompleteMultipartUpload(r.Context(), s.DB, upload, object); err != nil {
		s.internal(w, r, "complete multipart upload", err)
		return
	}

	w.Header().Set("ETag", quoteETag(object.ETag))
	writeXML(w, r, http.StatusOK, CompleteMultipartUploadResult{
		Xmlns:    s3Namespace,
		Location: s.objectLocation(bucket.Name, upload.Key),
		Bucket:   bucket.Name,
		Key:      upload.Key,
		ETag:     quoteETag(object.ETag),
	})
}

// handleAbortMultipartUpload implements AbortMultipartUpload.
func (s *Server) handleAbortMultipartUpload(w http.ResponseWriter, r *http.Request, bucket *db.Bucket, uploadID string) {
	upload, err := s.requireUpload(w, r, bucket, uploadID)
	if err != nil {
		return
	}
	if err := db.AbortMultipartUpload(r.Context(), s.DB, upload.ID); err != nil {
		s.internal(w, r, "abort multipart upload", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListParts implements ListParts.
func (s *Server) handleListParts(w http.ResponseWriter, r *http.Request, bucket *db.Bucket, uploadID string) {
	upload, err := s.requireUpload(w, r, bucket, uploadID)
	if err != nil {
		return
	}
	parts, err := db.ListMultipartParts(r.Context(), s.DB, upload.ID)
	if err != nil {
		s.internal(w, r, "list parts", err)
		return
	}

	result := ListPartsResult{
		Xmlns:        s3Namespace,
		Bucket:       bucket.Name,
		Key:          upload.Key,
		UploadID:     upload.UploadID,
		StorageClass: storageClassStandard,
		MaxParts:     maxPartNumber,
		Parts:        make([]PartXML, 0, len(parts)),
	}
	for _, part := range parts {
		result.Parts = append(result.Parts, PartXML{
			PartNumber:   part.PartNumber,
			LastModified: formatXMLTime(part.UploadedAt),
			ETag:         quoteETag(part.ETag),
			Size:         part.Size,
		})
	}
	writeXML(w, r, http.StatusOK, result)
}

// handleListMultipartUploads implements ListMultipartUploads.
func (s *Server) handleListMultipartUploads(w http.ResponseWriter, r *http.Request, bucket *db.Bucket) {
	const maxUploads = 1000

	uploads, err := db.ListMultipartUploads(r.Context(), s.DB, bucket.ID,
		r.URL.Query().Get("key-marker"), maxUploads)
	if err != nil {
		s.internal(w, r, "list multipart uploads", err)
		return
	}

	result := ListMultipartUploadsResult{
		Xmlns:      s3Namespace,
		Bucket:     bucket.Name,
		KeyMarker:  r.URL.Query().Get("key-marker"),
		MaxUploads: maxUploads,
		Uploads:    make([]UploadXML, 0, len(uploads)),
	}
	for _, upload := range uploads {
		result.Uploads = append(result.Uploads, UploadXML{
			Key:       upload.Key,
			UploadID:  upload.UploadID,
			Initiated: formatXMLTime(upload.InitiatedAt),
		})
	}
	writeXML(w, r, http.StatusOK, result)
}

// selectParts matches the client's requested parts against what was actually
// uploaded, enforcing S3's ordering and size rules.
func selectParts(requested []CompletedPartXML, uploaded map[int]db.MultipartPart) ([]db.MultipartPart, error) {
	selected := make([]db.MultipartPart, 0, len(requested))
	previousNumber := 0

	for i, want := range requested {
		if want.PartNumber <= previousNumber {
			return nil, ErrInvalidPartOrder
		}
		previousNumber = want.PartNumber

		have, ok := uploaded[want.PartNumber]
		if !ok {
			return nil, ErrInvalidPart.WithMessage(
				"Part number %d was not uploaded.", want.PartNumber)
		}
		// The ETag is the client's own record of what it sent, so a mismatch
		// means the client and server disagree about the part's contents.
		if unquoteETag(want.ETag) != have.ETag {
			return nil, ErrInvalidPart.WithMessage(
				"The entity tag for part number %d does not match the part that was uploaded.",
				want.PartNumber)
		}
		// Every part but the last must meet the minimum. Enforced so that an
		// object assembled here has a part layout real S3 would also accept.
		if i < len(requested)-1 && have.Size < minPartSize {
			return nil, ErrEntityTooSmall.WithMessage(
				"Part number %d is %d bytes; every part except the last must be at least %d bytes.",
				want.PartNumber, have.Size, minPartSize)
		}
		selected = append(selected, have)
	}
	return selected, nil
}

// compositeETag builds the multipart ETag: the MD5 of the concatenated *raw*
// part digests, suffixed with the part count.
//
// The digests are concatenated as bytes, not as hex text. Using the hex form
// produces a plausible-looking value that no S3 client agrees with, and the
// mistake only shows up when something compares against a real S3 ETag.
func compositeETag(parts []db.MultipartPart) string {
	sum := md5.New()
	for _, part := range parts {
		raw, err := hex.DecodeString(part.ETag)
		if err != nil {
			// A part ETag is always hex; if one is not, the composite would be
			// silently wrong, so fall back to the text form rather than
			// pretending the result is meaningful.
			sum.Write([]byte(part.ETag))
			continue
		}
		sum.Write(raw)
	}
	return fmt.Sprintf("%s-%d", hex.EncodeToString(sum.Sum(nil)), len(parts))
}

// requireUpload resolves an upload, writing NoSuchUpload if it is unknown.
func (s *Server) requireUpload(w http.ResponseWriter, r *http.Request, bucket *db.Bucket, uploadID string) (*db.MultipartUpload, error) {
	upload, err := db.GetMultipartUpload(r.Context(), s.DB, bucket.ID, uploadID)
	if errors.Is(err, db.ErrUploadNotFound) {
		WriteError(w, r, ErrNoSuchUpload)
		return nil, err
	}
	if err != nil {
		s.internal(w, r, "get multipart upload", err)
		return nil, err
	}
	return upload, nil
}

func parsePartNumber(raw string) (int, error) {
	n, err := strconv.Atoi(raw)
	if err != nil || n < minPartNumber || n > maxPartNumber {
		return 0, ErrInvalidArgument.WithMessage(
			"Part number must be an integer between %d and %d, got %q.",
			minPartNumber, maxPartNumber, raw)
	}
	return n, nil
}

// unquoteETag strips the double quotes clients wrap ETags in.
func unquoteETag(etag string) string {
	if len(etag) >= 2 && etag[0] == '"' && etag[len(etag)-1] == '"' {
		return etag[1 : len(etag)-1]
	}
	return etag
}

// objectLocation is the absolute URL of an object, returned on completion.
func (s *Server) objectLocation(bucket, key string) string {
	return fmt.Sprintf("%s/%s/%s", s.PublicURL, bucket, uriEncode(key, false))
}

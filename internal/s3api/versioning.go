package s3api

import (
	"encoding/xml"
	"io"
	"net/http"
	"strings"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
)

// The S3 surface for versioning: the switch, the listing, and addressing a
// single version.

// VersioningConfiguration is the ?versioning subresource, both directions.
//
// Status is omitted entirely on a bucket that was never versioned. That absence
// is meaningful — it is how a client tells "never turned on" from "turned on
// and then suspended" — so it is an omitempty field rather than a third string.
type VersioningConfiguration struct {
	XMLName xml.Name `xml:"VersioningConfiguration"`
	Xmlns   string   `xml:"xmlns,attr,omitempty"`
	Status  string   `xml:"Status,omitempty"`
}

const (
	versioningEnabled   = "Enabled"
	versioningSuspended = "Suspended"
)

// handleGetBucketVersioning reports a bucket's versioning state.
func (s *Server) handleGetBucketVersioning(w http.ResponseWriter, r *http.Request, bucket *db.Bucket) {
	settings, err := db.GetBucketSettings(r.Context(), s.DB, bucket.ID)
	if err != nil {
		s.internal(w, r, "read bucket settings", err)
		return
	}

	config := VersioningConfiguration{Xmlns: s3Namespace}
	switch settings.Versioning {
	case db.VersioningEnabled:
		config.Status = versioningEnabled
	case db.VersioningSuspended:
		config.Status = versioningSuspended
	}
	writeXML(w, r, http.StatusOK, config)
}

// handlePutBucketVersioning turns versioning on, or suspends it.
//
// S3 accepts only Enabled and Suspended here. There is no way back to the
// unversioned state, and refusing that explicitly is kinder than accepting the
// request and quietly doing something else.
func (s *Server) handlePutBucketVersioning(w http.ResponseWriter, r *http.Request, bucket *db.Bucket) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxDeleteBodySize))
	if err != nil {
		WriteError(w, r, ErrIncompleteBody)
		return
	}

	var config VersioningConfiguration
	if err := xml.Unmarshal(body, &config); err != nil {
		WriteError(w, r, ErrMalformedXML)
		return
	}

	var requested db.VersioningState
	switch config.Status {
	case versioningEnabled:
		requested = db.VersioningEnabled
	case versioningSuspended:
		requested = db.VersioningSuspended
	default:
		WriteError(w, r, ErrMalformedXML.WithMessage(
			"The versioning status must be Enabled or Suspended, got %q.", config.Status))
		return
	}

	settings, err := db.GetBucketSettings(r.Context(), s.DB, bucket.ID)
	if err != nil {
		s.internal(w, r, "read bucket settings", err)
		return
	}
	next, err := settings.Versioning.TransitionTo(requested)
	if err != nil {
		WriteError(w, r, ErrInvalidArgument.WithMessage("%s.", err))
		return
	}

	settings.Versioning = next
	if err := db.SaveBucketSettings(r.Context(), s.DB, settings); err != nil {
		s.internal(w, r, "save bucket settings", err)
		return
	}

	s.Log.Info("bucket versioning changed",
		"bucket", bucket.Name, "state", string(next),
		"request_id", RequestIDFrom(r.Context()))
	w.WriteHeader(http.StatusOK)
}

// ─── ListObjectVersions ──────────────────────────────────────────────────────

// ListVersionsResult is the ?versions response.
//
// Version and DeleteMarker are separate element names carrying one ordering,
// which is why they are separate slices marshalled from a single walk rather
// than one slice of a common type. Go's XML encoder emits each field in
// declaration order, so the two would otherwise be grouped — and S3 clients
// read the interleaving as the history it is.
type ListVersionsResult struct {
	XMLName xml.Name `xml:"ListVersionsResult"`
	Xmlns   string   `xml:"xmlns,attr"`

	Name                string `xml:"Name"`
	Prefix              string `xml:"Prefix"`
	Delimiter           string `xml:"Delimiter,omitempty"`
	KeyMarker           string `xml:"KeyMarker"`
	VersionIDMarker     string `xml:"VersionIdMarker"`
	NextKeyMarker       string `xml:"NextKeyMarker,omitempty"`
	NextVersionIDMarker string `xml:"NextVersionIdMarker,omitempty"`
	MaxKeys             int    `xml:"MaxKeys"`
	IsTruncated         bool   `xml:"IsTruncated"`
	EncodingType        string `xml:"EncodingType,omitempty"`

	Entries        []versionEntryXML `xml:",any"`
	CommonPrefixes []CommonPrefix    `xml:"CommonPrefixes,omitempty"`
}

// versionEntryXML is one history entry. Its element name is chosen per entry,
// so a single ordered slice can emit both Version and DeleteMarker elements
// interleaved the way S3 does.
type versionEntryXML struct {
	XMLName      xml.Name
	Key          string `xml:"Key"`
	VersionID    string `xml:"VersionId"`
	IsLatest     bool   `xml:"IsLatest"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag,omitempty"`
	Size         *int64 `xml:"Size,omitempty"`
	StorageClass string `xml:"StorageClass,omitempty"`
	Owner        *Owner `xml:"Owner,omitempty"`
}

// handleListObjectVersions implements the ?versions subresource.
func (s *Server) handleListObjectVersions(w http.ResponseWriter, r *http.Request, bucket *db.Bucket) {
	query := r.URL.Query()

	maxKeys, err := parseMaxKeys(query.Get("max-keys"))
	if err != nil {
		WriteError(w, r, err)
		return
	}

	requestedPrefix := query.Get("prefix")
	limit := listingLimitFor(r, bucket.Name)

	opts := db.VersionListOptions{
		Prefix:          limit.narrow(requestedPrefix),
		Delimiter:       query.Get("delimiter"),
		MaxKeys:         maxKeys,
		KeyMarker:       query.Get("key-marker"),
		VersionIDMarker: query.Get("version-id-marker"),
	}

	result, err := db.ListObjectVersions(r.Context(), s.DB, bucket.ID, opts)
	if err != nil {
		s.internal(w, r, "list object versions", err)
		return
	}

	encodeURL := strings.EqualFold(query.Get("encoding-type"), "url")
	encode := func(value string) string {
		if encodeURL {
			return uriEncode(value, true)
		}
		return value
	}

	response := ListVersionsResult{
		Xmlns:               s3Namespace,
		Name:                bucket.Name,
		Prefix:              encode(requestedPrefix),
		Delimiter:           encode(opts.Delimiter),
		KeyMarker:           encode(opts.KeyMarker),
		VersionIDMarker:     opts.VersionIDMarker,
		MaxKeys:             maxKeys,
		IsTruncated:         result.IsTruncated,
		NextKeyMarker:       encode(result.NextKeyMarker),
		NextVersionIDMarker: result.NextVersionID,
	}
	if encodeURL {
		response.EncodingType = "url"
	}

	for _, entry := range result.Versions {
		// Scope narrowing applies here exactly as it does to a plain listing:
		// a prefix-scoped key must not learn another tenant's keys by asking
		// for versions instead of objects.
		if !limit.allowsKey(entry.Key) {
			continue
		}
		response.Entries = append(response.Entries, versionEntry(entry, encode))
	}
	for _, prefix := range result.CommonPrefixes {
		if !limit.allowsCommonPrefix(prefix) {
			continue
		}
		response.CommonPrefixes = append(response.CommonPrefixes, CommonPrefix{Prefix: encode(prefix)})
	}

	writeXML(w, r, http.StatusOK, response)
}

// versionEntry renders one history entry, as a Version or a DeleteMarker.
func versionEntry(entry db.ObjectVersionEntry, encode func(string) string) versionEntryXML {
	out := versionEntryXML{
		Key:          encode(entry.Key),
		VersionID:    entry.VersionID,
		IsLatest:     entry.IsLatest,
		LastModified: formatXMLTime(entry.LastModified),
		Owner:        ownerPointer(),
	}
	if entry.IsDeleteMarker {
		// A delete marker has no bytes, so no ETag, size or storage class. S3
		// omits them rather than reporting zeroes, and a client that saw a size
		// of 0 would have no way to tell a marker from an empty object.
		out.XMLName = xml.Name{Local: "DeleteMarker"}
		return out
	}
	size := entry.Size
	out.XMLName = xml.Name{Local: "Version"}
	out.ETag = quoteETag(entry.ETag)
	out.Size = &size
	out.StorageClass = storageClassStandard
	return out
}

func ownerPointer() *Owner {
	owner := defaultOwner()
	return &owner
}

// ─── Addressing one version ──────────────────────────────────────────────────

// requestedVersionID returns the ?versionId a request names, if any.
func requestedVersionID(r *http.Request) string {
	return r.URL.Query().Get("versionId")
}

// writeVersionHeaders reports which version a response describes.
//
// Only set when the object actually has one: a bucket that was never versioned
// has no version id to report, and sending "null" on every response would be
// noise on the overwhelmingly common path.
func writeVersionHeaders(w http.ResponseWriter, versionID string) {
	if versionID != "" {
		w.Header().Set("x-amz-version-id", versionID)
	}
}

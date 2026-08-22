package s3api

import (
	"encoding/xml"
	"net/http"
	"time"
)

// s3Namespace appears on every S3 response document. Some clients validate it,
// and omitting it makes them fail to unmarshal an otherwise correct response.
const s3Namespace = "http://s3.amazonaws.com/doc/2006-03-01/"

// This server has a single tenant, so ownership is constant. The values still
// have to be present and internally consistent: clients parse them, and some
// compare the owner id across responses.
const (
	ownerID          = "s3d"
	ownerDisplayName = "s3d"
)

// Owner identifies who owns a bucket or object.
type Owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

func defaultOwner() Owner {
	return Owner{ID: ownerID, DisplayName: ownerDisplayName}
}

// ListAllMyBucketsResult is the ListBuckets response.
type ListAllMyBucketsResult struct {
	XMLName xml.Name   `xml:"ListAllMyBucketsResult"`
	Xmlns   string     `xml:"xmlns,attr"`
	Owner   Owner      `xml:"Owner"`
	Buckets bucketList `xml:"Buckets"`
}

type bucketList struct {
	Bucket []BucketEntry `xml:"Bucket"`
}

// BucketEntry is one bucket in a listing.
type BucketEntry struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

// CreateBucketConfiguration is the optional body of a CreateBucket request.
// Clients outside us-east-1 send it to name the region.
type CreateBucketConfiguration struct {
	XMLName            xml.Name `xml:"CreateBucketConfiguration"`
	LocationConstraint string   `xml:"LocationConstraint"`
}

// LocationConstraint is the GetBucketLocation response.
type LocationConstraint struct {
	XMLName xml.Name `xml:"LocationConstraint"`
	Xmlns   string   `xml:"xmlns,attr"`
	Value   string   `xml:",chardata"`
}

// iso8601Millis is the timestamp format S3 uses in XML bodies. It is not
// RFC3339: the fractional part is always exactly three digits, and some clients
// are strict about that.
const iso8601Millis = "2006-01-02T15:04:05.000Z"

func formatXMLTime(t time.Time) string {
	return t.UTC().Format(iso8601Millis)
}

// formatHTTPTime renders a timestamp for a header such as Last-Modified, which
// uses RFC 1123 in GMT rather than the XML format.
func formatHTTPTime(t time.Time) string {
	return t.UTC().Format(http.TimeFormat)
}

// writeXML sends an S3 XML response.
//
// The document is marshalled before the status is written, so a marshalling
// failure can still be reported as an error rather than appended to a
// half-written 200.
func writeXML(w http.ResponseWriter, r *http.Request, status int, payload any) {
	body, err := xml.Marshal(payload)
	if err != nil {
		WriteError(w, r, ErrInternalError)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(body)
}

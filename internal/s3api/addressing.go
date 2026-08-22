package s3api

import (
	"net/http"
	"strings"
)

// S3 has two ways of naming a bucket in a request. Path style puts it in the
// path — s3.example.com/mybucket/key — and virtual-host style puts it in the
// hostname — mybucket.s3.example.com/key. SDKs pick between them by
// configuration, and both have to work or half of them break.
//
// Virtual-host style needs a wildcard DNS record and certificate, so it is
// opt-in: with S3_DOMAIN unset the server is path-style only.

// bucketFromHost extracts a bucket name from a virtual-host style request.
//
// It returns the bucket and true only when the host is a single label followed
// by the configured domain. A deeper subdomain is not a bucket: bucket names
// cannot contain dots in a virtual-host context, since each dot would become a
// DNS label and break the wildcard certificate.
func bucketFromHost(host, s3Domain string) (string, bool) {
	if s3Domain == "" {
		return "", false
	}

	// The port is not part of the name.
	if colon := strings.LastIndex(host, ":"); colon >= 0 && !strings.Contains(host[colon:], "]") {
		host = host[:colon]
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	domain := strings.ToLower(strings.TrimSuffix(s3Domain, "."))

	suffix := "." + domain
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	candidate := strings.TrimSuffix(host, suffix)
	if candidate == "" || strings.Contains(candidate, ".") {
		return "", false
	}
	// Anything that is not a legal bucket name is not a bucket, and treating it
	// as one would turn a typo'd hostname into a confusing NoSuchBucket rather
	// than a plain routing miss.
	if ValidateBucketName(candidate) != nil {
		return "", false
	}
	return candidate, true
}

// resolveAddressing works out which bucket and key a request names, honouring
// both addressing styles.
//
// The host is taken from the trusted-proxy resolver rather than from
// Request.Host directly, because behind nginx the Host header holds the
// upstream address while the bucket lives in X-Forwarded-Host.
func (s *Server) resolveAddressing(r *http.Request) (bucket, key string, err error) {
	pathBucket, pathKey, err := splitPath(r.URL.EscapedPath())
	if err != nil {
		return "", "", err
	}

	host := s.Verifier.Proxies.Host(r)
	if hostBucket, ok := bucketFromHost(host, s.S3Domain); ok {
		// In virtual-host style the whole path is the key, so what path-style
		// parsing took for a bucket is really the first path segment.
		key = pathBucket
		if pathKey != "" {
			key += "/" + pathKey
		}
		return hostBucket, key, nil
	}
	return pathBucket, pathKey, nil
}

package console

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/s3api"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/storage"
)

// The console talks to storage through this API rather than through the S3 API,
// so the browser never has to implement SigV4. A session cookie authenticates
// instead, which is something a browser sends for free.

const (
	// maxUploadSize bounds a single console upload. Larger files go through the
	// S3 API with a presigned URL, where multipart handles them properly.
	maxUploadSize = 512 << 20

	// defaultPageSize is how many entries the object browser fetches at once.
	defaultPageSize = 200
	maxPageSize     = 1000

	// shareLinkTTL is the default lifetime of a share link.
	shareLinkTTL = 24 * time.Hour
)

// handleDashboard reports the totals shown on the console's front page.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	stats, err := db.ListBucketsWithStats(r.Context(), s.DB)
	if err != nil {
		s.internalError(w, r, "list bucket stats", err)
		return
	}

	var objects, bytes int64
	for _, stat := range stats {
		objects += stat.ObjectCount
		bytes += stat.TotalBytes
	}

	usage, err := s.Blobs.Usage()
	if err != nil {
		s.internalError(w, r, "read disk usage", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"buckets":     len(stats),
		"objects":     objects,
		"bytesStored": bytes,
		"diskFree":    usage.FreeBytes,
		"diskTotal":   usage.TotalBytes,
		// Stated on the dashboard rather than buried in documentation. One copy
		// is a deliberate choice here, and anyone reading a storage dashboard
		// should know what it does and does not promise.
		"durabilityNote": "Objects are stored as a single copy. There is no replication or redundancy.",
	})
}

// handleListBuckets returns buckets with their sizes.
func (s *Server) handleListBuckets(w http.ResponseWriter, r *http.Request) {
	stats, err := db.ListBucketsWithStats(r.Context(), s.DB)
	if err != nil {
		s.internalError(w, r, "list buckets", err)
		return
	}

	out := make([]map[string]any, 0, len(stats))
	for _, stat := range stats {
		out = append(out, map[string]any{
			"name":        stat.Name,
			"createdAt":   stat.CreatedAt,
			"objectCount": stat.ObjectCount,
			"totalBytes":  stat.TotalBytes,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"buckets": out})
}

type createBucketRequest struct {
	Name string `json:"name"`
}

// handleCreateBucket creates a bucket, reporting the exact naming rule broken.
func (s *Server) handleCreateBucket(w http.ResponseWriter, r *http.Request) {
	var request createBucketRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "Send a JSON body with a bucket name.")
		return
	}

	// The same validator the S3 API uses, so a bucket made here is one the S3
	// API and real S3 would both accept.
	if err := s3api.ValidateBucketName(request.Name); err != nil {
		writeError(w, http.StatusBadRequest, s3api.AsAPIError(err).Message)
		return
	}

	user, _ := UserFrom(r.Context())
	if _, err := db.CreateBucket(r.Context(), s.DB, request.Name, &user.ID); err != nil {
		if errors.Is(err, db.ErrBucketExists) {
			writeError(w, http.StatusConflict, "A bucket with that name already exists.")
			return
		}
		s.internalError(w, r, "create bucket", err)
		return
	}

	s.Log.Info("created a bucket", "bucket", request.Name, "by", user.Email)
	s.audit(r, db.ActionBucketCreate, "bucket", request.Name, nil)
	writeJSON(w, http.StatusCreated, map[string]string{"name": request.Name})
}

// handleDeleteBucket removes an empty bucket.
func (s *Server) handleDeleteBucket(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("bucket")
	user, _ := UserFrom(r.Context())

	switch err := db.DeleteBucket(r.Context(), s.DB, name); {
	case err == nil:
		s.Log.Info("deleted a bucket", "bucket", name, "by", user.Email)
		s.audit(r, db.ActionBucketDelete, "bucket", name, nil)
		writeJSON(w, http.StatusOK, map[string]string{"message": "Bucket deleted."})
	case errors.Is(err, db.ErrBucketNotFound):
		writeError(w, http.StatusNotFound, "No such bucket.")
	case errors.Is(err, db.ErrBucketNotEmpty):
		writeError(w, http.StatusConflict, "That bucket still contains objects. Empty it first.")
	default:
		s.internalError(w, r, "delete bucket", err)
	}
}

// handleListObjects returns one page of a prefix, grouped into folders.
//
// It deliberately mirrors ListObjectsV2 rather than inventing its own scheme,
// so the browser shows exactly what an S3 client would see.
func (s *Server) handleListObjects(w http.ResponseWriter, r *http.Request) {
	bucket, ok := s.requireBucket(w, r)
	if !ok {
		return
	}

	query := r.URL.Query()
	pageSize := defaultPageSize
	if raw := query.Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			pageSize = min(n, maxPageSize)
		}
	}

	// An empty delimiter lists every key flat, which the UI offers as a search.
	delimiter := "/"
	if query.Has("flat") {
		delimiter = ""
	}

	result, err := db.ListObjects(r.Context(), s.DB, bucket.ID, db.ListOptions{
		Prefix:     query.Get("prefix"),
		Delimiter:  delimiter,
		StartAfter: query.Get("after"),
		MaxKeys:    pageSize,
	})
	if err != nil {
		s.internalError(w, r, "list objects", err)
		return
	}

	objects := make([]map[string]any, 0, len(result.Objects))
	for _, object := range result.Objects {
		objects = append(objects, map[string]any{
			"key":          object.Key,
			"name":         displayName(object.Key, query.Get("prefix")),
			"size":         object.Size,
			"etag":         object.ETag,
			"contentType":  object.ContentType,
			"lastModified": object.UpdatedAt,
		})
	}

	folders := make([]map[string]any, 0, len(result.CommonPrefixes))
	for _, prefix := range result.CommonPrefixes {
		folders = append(folders, map[string]any{
			"prefix": prefix,
			"name":   folderName(prefix, query.Get("prefix")),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"bucket":      bucket.Name,
		"prefix":      query.Get("prefix"),
		"folders":     folders,
		"objects":     objects,
		"isTruncated": result.IsTruncated,
		"nextAfter":   result.NextStartAfter,
	})
}

// handleUploadObject accepts a browser upload and streams it to disk.
func (s *Server) handleUploadObject(w http.ResponseWriter, r *http.Request) {
	bucket, ok := s.requireBucket(w, r)
	if !ok {
		return
	}

	key := r.URL.Query().Get("key")
	if err := s3api.ValidateObjectKey(key); err != nil {
		writeError(w, http.StatusBadRequest, s3api.AsAPIError(err).Message)
		return
	}
	if r.ContentLength > maxUploadSize {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"Console uploads are limited to %d MB. Use a presigned URL or the S3 API for larger files.",
			maxUploadSize>>20))
		return
	}

	// The body is sent raw rather than as multipart/form-data, so it streams
	// straight through without the parser buffering it to a temporary file.
	body := http.MaxBytesReader(w, r.Body, maxUploadSize)
	defer body.Close()

	blob, err := s.Blobs.Put(r.Context(), body, "")
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "That file is too large for a console upload.")
			return
		}
		s.internalError(w, r, "write upload", err)
		return
	}

	object := &db.Object{
		BucketID:    bucket.ID,
		Key:         key,
		BlobDigest:  blob.Digest,
		Size:        blob.Size,
		ETag:        blob.ETag,
		ContentType: uploadContentType(r, key),
	}
	if err := db.PutObject(r.Context(), s.DB, object, s.writeOptions(r.Context(), bucket)); err != nil {
		s.internalError(w, r, "record upload", err)
		return
	}

	user, _ := UserFrom(r.Context())
	s.Log.Info("uploaded an object",
		"bucket", bucket.Name, "key", key, "bytes", blob.Size, "by", user.Email)
	s.audit(r, db.ActionObjectUpload, "object", bucket.Name+"/"+key,
		map[string]any{"bytes": blob.Size, "contentType": object.ContentType})

	writeJSON(w, http.StatusCreated, map[string]any{
		"key": key, "size": blob.Size, "etag": object.ETag, "contentType": object.ContentType,
	})
}

type deleteObjectsRequest struct {
	Keys []string `json:"keys"`
	// Prefix deletes everything beneath it, which is what "delete folder"
	// means in a store that has no folders.
	Prefix string `json:"prefix"`
}

// handleDeleteObjects removes selected keys, or an entire prefix.
func (s *Server) handleDeleteObjects(w http.ResponseWriter, r *http.Request) {
	bucket, ok := s.requireBucket(w, r)
	if !ok {
		return
	}

	var request deleteObjectsRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "Send a JSON body with keys or a prefix.")
		return
	}
	if len(request.Keys) == 0 && request.Prefix == "" {
		writeError(w, http.StatusBadRequest, "Nothing was selected.")
		return
	}

	ctx := r.Context()
	keys := request.Keys

	if request.Prefix != "" {
		// Every key under the prefix is collected first, paging through, so a
		// folder with more entries than one page is fully removed rather than
		// partially.
		after := ""
		for {
			page, err := db.ListObjects(ctx, s.DB, bucket.ID, db.ListOptions{
				Prefix: request.Prefix, StartAfter: after, MaxKeys: maxPageSize,
			})
			if err != nil {
				s.internalError(w, r, "list objects for prefix delete", err)
				return
			}
			for _, object := range page.Objects {
				keys = append(keys, object.Key)
			}
			if !page.IsTruncated {
				break
			}
			after = page.NextStartAfter
		}
	}

	options := s.writeOptions(ctx, bucket)
	deleted := 0
	var failures []string
	for _, key := range keys {
		if _, err := db.DeleteObject(ctx, s.DB, bucket.ID, key, options); err != nil {
			s.Log.Error("could not delete object", "bucket", bucket.Name, "key", key, "error", err)
			failures = append(failures, key)
			continue
		}
		deleted++
	}

	user, _ := UserFrom(ctx)
	s.Log.Info("deleted objects",
		"bucket", bucket.Name, "count", deleted, "failed", len(failures), "by", user.Email)
	s.audit(r, db.ActionObjectDelete, "bucket", bucket.Name,
		map[string]any{"deleted": deleted, "failed": len(failures), "prefix": request.Prefix})

	status := http.StatusOK
	if len(failures) > 0 {
		// Partial success is reported as such rather than as a flat failure, so
		// the UI can say what actually happened.
		status = http.StatusMultiStatus
	}
	writeJSON(w, status, map[string]any{"deleted": deleted, "failed": failures})
}

// handleDownloadObject streams an object to the browser.
func (s *Server) handleDownloadObject(w http.ResponseWriter, r *http.Request) {
	bucket, ok := s.requireBucket(w, r)
	if !ok {
		return
	}
	key := r.URL.Query().Get("key")

	object, err := db.GetObject(r.Context(), s.DB, bucket.ID, key)
	if errors.Is(err, db.ErrObjectNotFound) {
		writeError(w, http.StatusNotFound, "No such object.")
		return
	}
	if err != nil {
		s.internalError(w, r, "get object", err)
		return
	}

	file, err := s.Blobs.Open(object.BlobDigest)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			s.internalError(w, r, "open object", fmt.Errorf(
				"object %q references missing blob %s", key, object.BlobDigest))
			return
		}
		s.internalError(w, r, "open object", err)
		return
	}
	defer file.Close()

	w.Header().Set("Content-Type", object.ContentType)
	w.Header().Set("ETag", `"`+object.ETag+`"`)
	// Content-Disposition decides whether the browser shows the file or saves
	// it. inline is deliberate for previewing; ?download=1 forces a save.
	disposition := "inline"
	if r.URL.Query().Has("download") {
		disposition = "attachment"
	}
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("%s; filename*=UTF-8''%s", disposition, urlEncodeFilename(filepath.Base(key))))
	// Untrusted content served from the console's own origin is a stored-XSS
	// risk: an uploaded HTML file would otherwise run with the console's
	// cookies. The sandbox and CSP together neutralise it.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")

	http.ServeContent(w, r, key, object.UpdatedAt, file)
}

type shareRequest struct {
	Key            string `json:"key"`
	ExpiresSeconds int    `json:"expiresSeconds"`
}

// handleShareObject mints a presigned URL for an object.
//
// It reuses the S3 presigner, so a share link is an ordinary S3 URL that works
// from anywhere without the console being involved in the download.
func (s *Server) handleShareObject(w http.ResponseWriter, r *http.Request) {
	bucket, ok := s.requireBucket(w, r)
	if !ok {
		return
	}

	var request shareRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "Send a JSON body with a key.")
		return
	}

	ctx := r.Context()
	if _, err := db.GetObject(ctx, s.DB, bucket.ID, request.Key); err != nil {
		if errors.Is(err, db.ErrObjectNotFound) {
			writeError(w, http.StatusNotFound, "No such object.")
			return
		}
		s.internalError(w, r, "get object", err)
		return
	}

	expiry := shareLinkTTL
	if request.ExpiresSeconds > 0 {
		expiry = time.Duration(request.ExpiresSeconds) * time.Second
	}

	// A share link needs a real credential to sign with. An active one is used
	// rather than a hidden internal key, so revoking it also kills every link
	// signed with it — which is the only way to withdraw a link already sent.
	credential, err := s.signingCredential(ctx)
	if err != nil {
		if errors.Is(err, errNoSigningCredential) {
			writeError(w, http.StatusConflict,
				"Create an S3 credential first. Share links are signed with one, so revoking it also revokes the links.")
			return
		}
		s.internalError(w, r, "find a signing credential", err)
		return
	}

	url, err := s3api.Presign(http.MethodGet, s.PublicS3URL, bucket.Name, request.Key, nil,
		credential.AccessKeyID, credential.SecretKey, s.Region, s.now(), expiry)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.audit(r, db.ActionShareLink, "object", bucket.Name+"/"+request.Key,
		map[string]any{"expiresSeconds": int(expiry.Seconds())})

	writeJSON(w, http.StatusOK, map[string]any{
		"url":       url,
		"expiresAt": s.now().Add(expiry),
	})
}

var errNoSigningCredential = errors.New("no active S3 credential")

// signingCredential returns an active credential to sign share links with.
func (s *Server) signingCredential(ctx context.Context) (*db.Credential, error) {
	credentials, err := db.ListCredentials(ctx, s.DB)
	if err != nil {
		return nil, err
	}
	for i := range credentials {
		if credentials[i].Revoked() {
			continue
		}
		// Listing omits secrets, so the chosen credential is re-read in full.
		return db.LookupCredential(ctx, s.DB, s.Cipher, credentials[i].AccessKeyID)
	}
	return nil, errNoSigningCredential
}

// writeOptions resolves the per-bucket behaviour a write depends on, and
// attributes the change to the signed-in user so version history says who.
func (s *Server) writeOptions(ctx context.Context, bucket *db.Bucket) db.WriteOptions {
	settings, err := db.GetBucketSettings(ctx, s.DB, bucket.ID)
	if err != nil {
		s.Log.Warn("could not read bucket settings; proceeding without versioning",
			"bucket", bucket.Name, "error", err)
		return db.WriteOptions{}
	}
	actor := "console"
	if user, ok := UserFrom(ctx); ok {
		actor = user.Email
	}
	return db.WriteOptions{Versioning: settings.Versioning, Actor: actor}
}

// requireBucket resolves the bucket named in the path.
func (s *Server) requireBucket(w http.ResponseWriter, r *http.Request) (*db.Bucket, bool) {
	bucket, err := db.GetBucket(r.Context(), s.DB, r.PathValue("bucket"))
	if errors.Is(err, db.ErrBucketNotFound) {
		writeError(w, http.StatusNotFound, "No such bucket.")
		return nil, false
	}
	if err != nil {
		s.internalError(w, r, "get bucket", err)
		return nil, false
	}
	return bucket, true
}

// displayName is the part of a key below the current prefix, which is what the
// browser shows as a filename.
func displayName(key, prefix string) string {
	return strings.TrimPrefix(key, prefix)
}

// folderName is a common prefix rendered as a folder name.
func folderName(fullPrefix, currentPrefix string) string {
	return strings.TrimSuffix(strings.TrimPrefix(fullPrefix, currentPrefix), "/")
}

// uploadContentType uses what the browser declared, falling back to the file
// extension. A browser that sends nothing would otherwise make every upload
// an opaque download.
func uploadContentType(r *http.Request, key string) string {
	if declared := r.Header.Get("Content-Type"); declared != "" && declared != "application/octet-stream" {
		return declared
	}
	if byExtension := mime.TypeByExtension(filepath.Ext(key)); byExtension != "" {
		return byExtension
	}
	return "binary/octet-stream"
}

// urlEncodeFilename percent-encodes a filename for Content-Disposition's
// RFC 5987 form, which is how a non-ASCII name survives the header.
func urlEncodeFilename(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

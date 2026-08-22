package console

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
)

// handleListCredentials returns the S3 access keys, never their secrets.
func (s *Server) handleListCredentials(w http.ResponseWriter, r *http.Request) {
	credentials, err := db.ListCredentials(r.Context(), s.DB)
	if err != nil {
		s.internalError(w, r, "list credentials", err)
		return
	}
	out := make([]map[string]any, 0, len(credentials))
	for i := range credentials {
		c := &credentials[i]
		out = append(out, map[string]any{
			"accessKeyId": c.AccessKeyID,
			"description": c.Description,
			"createdAt":   c.CreatedAt,
			"lastUsedAt":  c.LastUsedAt,
			"revoked":     c.Revoked(),
			"revokedAt":   c.RevokedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": out})
}

type createCredentialRequest struct {
	Description string `json:"description"`
}

// handleCreateCredential issues an access key pair.
//
// This is the only moment the secret exists in a form anyone can read: only its
// encrypted form is stored, and there is no endpoint that returns it again. The
// response says so explicitly so the UI can be unambiguous about it.
func (s *Server) handleCreateCredential(w http.ResponseWriter, r *http.Request) {
	var request createCredentialRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "Send a JSON body with a description.")
		return
	}

	owner, _ := UserFrom(r.Context())
	credential, err := db.CreateCredential(r.Context(), s.DB, s.Cipher, request.Description, &owner.ID)
	if err != nil {
		s.internalError(w, r, "create credential", err)
		return
	}

	s.Log.Info("created an S3 credential",
		"access_key_id", credential.AccessKeyID, "by", owner.Email)
	s.audit(r, db.ActionCredentialCreate, "credential", credential.AccessKeyID,
		map[string]any{"description": credential.Description})

	writeJSON(w, http.StatusCreated, map[string]any{
		"accessKeyId":     credential.AccessKeyID,
		"secretAccessKey": credential.SecretKey,
		"description":     credential.Description,
		"createdAt":       credential.CreatedAt,
		"endpoint":        s.PublicS3URL,
		"region":          s.Region,
		"warning":         "This secret is shown once and cannot be recovered. Store it now.",
		"snippets":        connectionSnippets(s.PublicS3URL, s.Region, credential.AccessKeyID, credential.SecretKey),
	})
}

// handleRevokeCredential disables an access key immediately.
func (s *Server) handleRevokeCredential(w http.ResponseWriter, r *http.Request) {
	accessKeyID := r.PathValue("accessKeyId")
	actor, _ := UserFrom(r.Context())

	switch err := db.RevokeCredential(r.Context(), s.DB, accessKeyID); {
	case err == nil:
		s.Log.Info("revoked an S3 credential", "access_key_id", accessKeyID, "by", actor.Email)
		s.audit(r, db.ActionCredentialRevoke, "credential", accessKeyID, nil)
		writeJSON(w, http.StatusOK, map[string]string{
			"message": "Revoked. It stops working on the next request.",
		})
	case errors.Is(err, db.ErrCredentialNotFound):
		writeError(w, http.StatusNotFound, "No such credential, or it is already revoked.")
	default:
		s.internalError(w, r, "revoke credential", err)
	}
}

// connectionSnippets builds ready-to-paste configuration for the common
// clients.
//
// Every one of them needs the endpoint overridden and path-style addressing
// forced, and getting either wrong produces an error that says nothing about
// the cause. Handing over working snippets removes the entire class of problem
// from a user's first five minutes.
func connectionSnippets(endpoint, region, accessKeyID, secretKey string) map[string]string {
	return map[string]string{
		"env": fmt.Sprintf(
			"export AWS_ACCESS_KEY_ID=%s\nexport AWS_SECRET_ACCESS_KEY=%s\nexport AWS_DEFAULT_REGION=%s\nexport AWS_ENDPOINT_URL=%s",
			accessKeyID, secretKey, region, endpoint),

		"awscli": fmt.Sprintf(
			"# ~/.aws/config\n[profile s3d]\nregion = %s\nendpoint_url = %s\ns3 =\n    addressing_style = path\n\n"+
				"# ~/.aws/credentials\n[s3d]\naws_access_key_id = %s\naws_secret_access_key = %s\n\n"+
				"# then:\naws --profile s3d s3 ls",
			region, endpoint, accessKeyID, secretKey),

		"boto3": fmt.Sprintf(
			"import boto3\nfrom botocore.config import Config\n\n"+
				"s3 = boto3.client(\n"+
				"    \"s3\",\n"+
				"    endpoint_url=%q,\n"+
				"    aws_access_key_id=%q,\n"+
				"    aws_secret_access_key=%q,\n"+
				"    region_name=%q,\n"+
				"    # Required: this server is SigV4 only, and botocore\n"+
				"    # otherwise falls back to SigV2 when presigning.\n"+
				"    config=Config(signature_version=\"s3v4\",\n"+
				"                  s3={\"addressing_style\": \"path\"}),\n)",
			endpoint, accessKeyID, secretKey, region),

		"go": fmt.Sprintf(
			"client := s3.New(s3.Options{\n"+
				"    Region:       %q,\n"+
				"    BaseEndpoint: aws.String(%q),\n"+
				"    UsePathStyle: true,\n"+
				"    Credentials: credentials.NewStaticCredentialsProvider(\n"+
				"        %q, %q, \"\"),\n"+
				"})",
			region, endpoint, accessKeyID, secretKey),

		"nodejs": fmt.Sprintf(
			"import { S3Client } from \"@aws-sdk/client-s3\";\n\n"+
				"const s3 = new S3Client({\n"+
				"  region: %q,\n"+
				"  endpoint: %q,\n"+
				"  forcePathStyle: true,\n"+
				"  credentials: {\n"+
				"    accessKeyId: %q,\n"+
				"    secretAccessKey: %q,\n"+
				"  },\n"+
				"});",
			region, endpoint, accessKeyID, secretKey),
	}
}

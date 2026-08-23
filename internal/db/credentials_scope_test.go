package db

import (
	"context"
	"testing"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/secrets"
)

func testCipher(t *testing.T) *secrets.Cipher {
	t.Helper()
	cipher, err := secrets.NewCipher("db-package-test-credentials-key-32ch")
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return cipher
}

// The scope has to survive a real column, not just encoding/json. This also
// proves the migration's default applies to rows written without one.
func TestCredentialScopeRoundTripsThroughPostgres(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cipher := testCipher(t)

	scope := Grant{Rules: []GrantRule{
		{Bucket: "assets", Prefix: "", Permissions: []Permission{PermissionRead}},
		{Bucket: "uploads", Prefix: "in/", Permissions: []Permission{PermissionWrite, PermissionDelete}},
	}}

	created, err := CreateCredential(ctx, pool, cipher, "scoped", nil, scope)
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	looked, err := LookupCredential(ctx, pool, cipher, created.AccessKeyID)
	if err != nil {
		t.Fatalf("LookupCredential: %v", err)
	}
	if looked.Scope.Unrestricted {
		t.Fatal("a scoped key came back unrestricted")
	}
	if !looked.Scope.Allows("uploads", "in/x", PermissionWrite) {
		t.Error("permission lost in the database round trip")
	}
	if looked.Scope.Allows("uploads", "out/x", PermissionWrite) {
		t.Error("prefix lost in the database round trip")
	}

	// The default keeps every pre-existing key working, which is what makes
	// this migration safe to apply to a running deployment.
	plain, err := CreateCredential(ctx, pool, cipher, "unscoped", nil, UnrestrictedGrant())
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE credentials SET access_scope = DEFAULT WHERE access_key_id = $1`,
		plain.AccessKeyID); err != nil {
		t.Fatalf("reset to column default: %v", err)
	}
	defaulted, err := LookupCredential(ctx, pool, cipher, plain.AccessKeyID)
	if err != nil {
		t.Fatalf("LookupCredential: %v", err)
	}
	if !defaulted.Scope.Unrestricted {
		t.Error("the column default is not unrestricted; upgrading would lock existing keys out")
	}

	// Narrowing takes effect without reissuing the secret.
	if err := SetCredentialScope(ctx, pool, created.AccessKeyID, scoped()); err != nil {
		t.Fatalf("SetCredentialScope: %v", err)
	}
	narrowed, err := LookupCredential(ctx, pool, cipher, created.AccessKeyID)
	if err != nil {
		t.Fatalf("LookupCredential: %v", err)
	}
	if narrowed.Scope.Allows("uploads", "in/x", PermissionWrite) {
		t.Error("narrowing a key did not take effect")
	}

	// A listing must report scopes too, or the console cannot show them.
	all, err := ListCredentials(ctx, pool)
	if err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("no credentials listed")
	}
	for _, c := range all {
		if c.AccessKeyID == plain.AccessKeyID && !c.Scope.Unrestricted {
			t.Error("listing lost the unrestricted flag")
		}
	}
}

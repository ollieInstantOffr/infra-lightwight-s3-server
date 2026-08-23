package console

import (
	"strings"
	"testing"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
)

// AccessDenied has two quite different causes once keys can be scoped, and
// naming the wrong one sends an operator to check credentials that are fine.
func TestAccessDeniedDiagnosisDistinguishesScopeFromMissingSignature(t *testing.T) {
	scoped := likelyCause(db.ErrorBreakdown{
		ErrorCode: "AccessDenied",
		Reason:    "the access key is not permitted to write assets-prod/tenant-b/f",
	})
	if !strings.Contains(scoped, "access does not cover this") {
		t.Errorf("a scope denial was not diagnosed as one: %q", scoped)
	}
	if strings.Contains(scoped, "no signature") {
		t.Errorf("a scope denial was blamed on a missing signature: %q", scoped)
	}

	unsigned := likelyCause(db.ErrorBreakdown{
		ErrorCode: "AccessDenied",
		Reason:    "request is not signed",
	})
	if !strings.Contains(unsigned, "no signature") {
		t.Errorf("an unsigned request was not diagnosed as one: %q", unsigned)
	}
}

func TestScopePayloadConvertsToGrant(t *testing.T) {
	t.Run("absent means unrestricted", func(t *testing.T) {
		// A caller that predates scoping sends no scope and must keep getting
		// the key it always got.
		var absent *scopePayload
		if !absent.grant().Unrestricted {
			t.Error("an absent scope did not produce an unrestricted key")
		}
	})

	t.Run("rules convert", func(t *testing.T) {
		payload := &scopePayload{Rules: []scopeRulePayload{
			{Bucket: "b", Prefix: "p/", Permissions: []string{"read", "write"}},
		}}
		grant := payload.grant()
		if grant.Unrestricted {
			t.Fatal("a scoped payload produced an unrestricted grant")
		}
		if !grant.Allows("b", "p/x", db.PermissionWrite) {
			t.Error("a permission did not convert")
		}
		if grant.Allows("b", "q/x", db.PermissionWrite) {
			t.Error("the prefix did not convert")
		}
	})

	t.Run("an invented permission is rejected by validation", func(t *testing.T) {
		payload := &scopePayload{Rules: []scopeRulePayload{
			{Bucket: "b", Permissions: []string{"admin"}},
		}}
		if err := payload.grant().Validate(); err == nil {
			t.Error("an invented permission was accepted")
		}
	})
}

func TestScopeResponseCarriesASummary(t *testing.T) {
	// The console, the audit log and the request log should describe a scope in
	// the same words, so the summary is rendered server-side.
	out := scopeResponse(db.Grant{Rules: []db.GrantRule{
		{Bucket: "assets", Permissions: []db.Permission{db.PermissionRead}},
	}})
	if out["summary"] != "assets (read)" {
		t.Errorf("summary = %v", out["summary"])
	}
	if out["unrestricted"] != false {
		t.Errorf("unrestricted = %v", out["unrestricted"])
	}
}

func TestCredentialScopeEndpoints(t *testing.T) {
	console := newConsole(t)
	console.signIn(t, "admin@example.com")

	// Created with a scope.
	status, body := console.do(t, "POST", "/api/credentials", map[string]any{
		"description": "scoped",
		"scope": map[string]any{
			"unrestricted": false,
			"rules": []map[string]any{
				{"bucket": "assets", "prefix": "a/", "permissions": []string{"read"}},
			},
		},
	})
	if status != 201 {
		t.Fatalf("create: status %d, body %v", status, body)
	}
	accessKeyID, _ := body["accessKeyId"].(string)
	scope, _ := body["scope"].(map[string]any)
	if scope["summary"] != "assets/a/ (read)" {
		t.Errorf("created scope summary = %v", scope["summary"])
	}

	// An invalid scope is refused rather than stored.
	status, _ = console.do(t, "POST", "/api/credentials", map[string]any{
		"description": "bad",
		"scope": map[string]any{
			"unrestricted": false,
			"rules":        []map[string]any{{"bucket": "assets", "permissions": []string{"sudo"}}},
		},
	})
	if status != 400 {
		t.Errorf("an invented permission was accepted: status %d", status)
	}

	// Narrowed afterwards, without reissuing the secret.
	status, body = console.do(t, "PUT", "/api/credentials/"+accessKeyID+"/scope", map[string]any{
		"scope": map[string]any{"unrestricted": false, "rules": []map[string]any{}},
	})
	if status != 200 {
		t.Fatalf("set scope: status %d, body %v", status, body)
	}

	// And the listing reflects it.
	status, body = console.do(t, "GET", "/api/credentials", nil)
	if status != 200 {
		t.Fatalf("list: status %d", status)
	}
	credentials, _ := body["credentials"].([]any)
	var found bool
	for _, entry := range credentials {
		c, _ := entry.(map[string]any)
		if c["accessKeyId"] != accessKeyID {
			continue
		}
		found = true
		listed, _ := c["scope"].(map[string]any)
		if listed["summary"] != "no access" {
			t.Errorf("listed summary = %v, want the narrowed scope", listed["summary"])
		}
	}
	if !found {
		t.Error("the credential was not in the listing")
	}
}

// A client that reads a resource, edits it and sends the whole thing back is
// doing the normal thing, and it is what the console actually does — it holds
// the scope the server gave it in React state and posts that.
//
// decodeJSON rejects unknown fields, and the scope the server sends carries a
// server-rendered summary the request shape did not accept. Every create and
// every scope change was a 400. The earlier tests missed it by constructing
// request bodies by hand rather than echoing a response, which is precisely
// the shape of client behaviour that broke.
func TestScopeSurvivesARoundTrip(t *testing.T) {
	console := newConsole(t)
	console.signIn(t, "admin@example.com")

	// Create with a scope, then send back exactly what came out.
	status, body := console.do(t, "POST", "/api/credentials", map[string]any{
		"description": "round-trip",
		"scope": map[string]any{
			"unrestricted": false,
			"rules": []map[string]any{
				{"bucket": "assets", "prefix": "a/", "permissions": []string{"read"}},
			},
		},
	})
	if status != 201 {
		t.Fatalf("create: status %d, body %v", status, body)
	}
	accessKeyID, _ := body["accessKeyId"].(string)
	returned, _ := body["scope"].(map[string]any)
	if _, ok := returned["summary"]; !ok {
		t.Fatal("the response carries no summary, so this test is not exercising the round trip")
	}

	// Straight back, unedited.
	status, body = console.do(t, "PUT", "/api/credentials/"+accessKeyID+"/scope",
		map[string]any{"scope": returned})
	if status != 200 {
		t.Fatalf("sending back the scope the server just sent was refused: status %d, body %v", status, body)
	}

	// And creating with one, which is what the console's create form does after
	// its state has been seeded from an unrestricted scope.
	status, body = console.do(t, "POST", "/api/credentials", map[string]any{
		"description": "from-returned",
		"scope":       returned,
	})
	if status != 201 {
		t.Fatalf("creating with a returned scope was refused: status %d, body %v", status, body)
	}
}

func TestUnrestrictedScopeFromTheConsoleIsAccepted(t *testing.T) {
	// The exact body the create form sends with the limit toggle off, summary
	// and all. This is what the screenshot was hitting.
	console := newConsole(t)
	console.signIn(t, "admin@example.com")

	status, body := console.do(t, "POST", "/api/credentials", map[string]any{
		"description": "perf",
		"scope": map[string]any{
			"unrestricted": true,
			"rules":        []map[string]any{},
			"summary":      "unrestricted",
		},
	})
	if status != 201 {
		t.Fatalf("status %d, body %v", status, body)
	}
}

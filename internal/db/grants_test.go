package db

import "testing"

// rule is a terse constructor, so the tables below read as permissions rather
// than as struct literals.
func rule(bucket, prefix string, perms ...Permission) GrantRule {
	return GrantRule{Bucket: bucket, Prefix: prefix, Permissions: perms}
}

func scoped(rules ...GrantRule) Grant {
	return Grant{Unrestricted: false, Rules: rules}
}

func TestZeroGrantAllowsNothing(t *testing.T) {
	// The single most important property here. An authorizer that fails open
	// is worse than none, because it reads as protection that is not there —
	// and the zero value is what a decoding bug or a missed field produces.
	var empty Grant

	for _, perm := range AllPermissions {
		if empty.Allows("any", "key", perm) {
			t.Errorf("zero grant allowed %s on an object", perm)
		}
		if empty.AllowsAnywhereIn("any", perm) {
			t.Errorf("zero grant allowed %s somewhere in a bucket", perm)
		}
		if empty.AllowsWholeBucket("any", perm) {
			t.Errorf("zero grant allowed %s across a whole bucket", perm)
		}
	}
	if empty.Visible("any") {
		t.Error("zero grant made a bucket visible")
	}
}

func TestUnrestrictedGrantAllowsEverything(t *testing.T) {
	g := UnrestrictedGrant()

	for _, perm := range AllPermissions {
		if !g.Allows("anything", "at/all", perm) {
			t.Errorf("unrestricted grant refused %s", perm)
		}
		if !g.AllowsWholeBucket("anything", perm) {
			t.Errorf("unrestricted grant refused %s across a bucket", perm)
		}
	}
	if !g.Visible("anything") {
		t.Error("unrestricted grant hid a bucket")
	}
	if g.ReadablePrefixes("anything") != nil {
		t.Error("unrestricted grant reported prefix limits; nil means no limit")
	}
}

func TestAllowsRespectsBucketPermissionAndPrefix(t *testing.T) {
	g := scoped(
		rule("assets", "", PermissionRead),
		rule("uploads", "incoming/", PermissionRead, PermissionWrite),
	)

	cases := []struct {
		name   string
		bucket string
		key    string
		perm   Permission
		want   bool
	}{
		{"read the bucket it was granted", "assets", "logo.png", PermissionRead, true},
		{"write a bucket granted only read", "assets", "logo.png", PermissionWrite, false},
		{"delete a bucket granted only read", "assets", "logo.png", PermissionDelete, false},
		{"read a bucket not mentioned at all", "secrets", "key.pem", PermissionRead, false},
		{"write inside the granted prefix", "uploads", "incoming/a.txt", PermissionWrite, true},
		{"write outside the granted prefix", "uploads", "archive/a.txt", PermissionWrite, false},
		{"read at the prefix boundary itself", "uploads", "incoming/", PermissionRead, true},
		{"delete inside a prefix granted no delete", "uploads", "incoming/a.txt", PermissionDelete, false},
		{"the bucket root of a prefix-scoped rule", "uploads", "", PermissionRead, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := g.Allows(tc.bucket, tc.key, tc.perm); got != tc.want {
				t.Errorf("Allows(%q, %q, %s) = %v, want %v",
					tc.bucket, tc.key, tc.perm, got, tc.want)
			}
		})
	}
}

func TestPrefixIsABytePrefixNotAPathSegment(t *testing.T) {
	// Worth pinning because it occasionally surprises people. S3 prefixes work
	// this way everywhere else in this server, and a scope that silently used
	// different matching rules from ListObjects would be far worse.
	g := scoped(rule("data", "logs", PermissionRead))

	if !g.Allows("data", "logs/today", PermissionRead) {
		t.Error("prefix did not match the obvious case")
	}
	if !g.Allows("data", "logsomething", PermissionRead) {
		t.Error("prefix should match by bytes, as S3 prefixes do everywhere else")
	}
	if g.Allows("data", "log", PermissionRead) {
		t.Error("a key shorter than the prefix matched")
	}
}

func TestAllowsWholeBucketRequiresAnUnprefixedRule(t *testing.T) {
	// Creating and deleting a bucket affect everything in it, so a key confined
	// to a prefix must not be able to do either.
	prefixed := scoped(rule("tenant", "tenant-a/", PermissionRead, PermissionWrite, PermissionDelete))
	whole := scoped(rule("tenant", "", PermissionDelete))

	if prefixed.AllowsWholeBucket("tenant", PermissionDelete) {
		t.Error("a prefix-scoped key could act on the whole bucket")
	}
	if !prefixed.AllowsAnywhereIn("tenant", PermissionDelete) {
		t.Error("a prefix-scoped key should still be permitted somewhere in the bucket")
	}
	if !whole.AllowsWholeBucket("tenant", PermissionDelete) {
		t.Error("an unprefixed rule did not permit acting on the whole bucket")
	}
}

func TestVisibleHidesBucketsTheKeyCannotUse(t *testing.T) {
	g := scoped(rule("shown", "some/prefix/", PermissionRead))

	if !g.Visible("shown") {
		t.Error("a bucket the key has a rule for was hidden")
	}
	if g.Visible("hidden") {
		t.Error("a bucket the key has no rule for was visible; names leak what a deployment is for")
	}
	// A rule granting nothing is not a reason to reveal a bucket.
	if scoped(rule("empty", "")).Visible("empty") {
		t.Error("a rule with no permissions made a bucket visible")
	}
}

func TestReadablePrefixes(t *testing.T) {
	t.Run("nil means the whole bucket", func(t *testing.T) {
		g := scoped(rule("b", "", PermissionRead))
		if got := g.ReadablePrefixes("b"); got != nil {
			t.Errorf("got %v, want nil for an unprefixed read rule", got)
		}
	})

	t.Run("an unprefixed rule wins over prefixed ones", func(t *testing.T) {
		// Otherwise the listing constraint would narrow a key that is in fact
		// allowed to read everything in the bucket.
		g := scoped(
			rule("b", "a/", PermissionRead),
			rule("b", "", PermissionRead),
		)
		if got := g.ReadablePrefixes("b"); got != nil {
			t.Errorf("got %v, want nil when one rule covers the whole bucket", got)
		}
	})

	t.Run("collects the prefixes it may read", func(t *testing.T) {
		g := scoped(
			rule("b", "a/", PermissionRead),
			rule("b", "b/", PermissionRead, PermissionWrite),
			rule("b", "c/", PermissionWrite),
			rule("other", "d/", PermissionRead),
		)
		got := g.ReadablePrefixes("b")
		want := []string{"a/", "b/"}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	})

	t.Run("empty and non-nil when nothing is readable", func(t *testing.T) {
		g := scoped(rule("b", "a/", PermissionWrite))
		got := g.ReadablePrefixes("b")
		if got == nil {
			t.Fatal("got nil, which means unrestricted; want an empty slice meaning nothing")
		}
		if len(got) != 0 {
			t.Fatalf("got %v, want empty", got)
		}
	})
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		grant   Grant
		wantErr bool
	}{
		{"unrestricted", UnrestrictedGrant(), false},
		{"unrestricted carrying rules", Grant{Unrestricted: true, Rules: []GrantRule{rule("b", "", PermissionRead)}}, true},
		{"no rules at all is a valid disabled key", scoped(), false},
		{"a good rule", scoped(rule("b", "p/", PermissionRead)), false},
		{"rule naming no bucket", scoped(rule("", "", PermissionRead)), true},
		{"rule granting nothing", scoped(rule("b", "")), true},
		{"rule with an invented permission", scoped(rule("b", "", Permission("admin"))), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.grant.Validate()
			if tc.wantErr && err == nil {
				t.Error("expected an error, got none")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestGrantSurvivesEncoding(t *testing.T) {
	original := scoped(
		rule("assets", "", PermissionRead),
		rule("uploads", "incoming/", PermissionWrite, PermissionDelete),
	)

	encoded, err := marshalGrant(original)
	if err != nil {
		t.Fatalf("marshalGrant: %v", err)
	}
	decoded, err := unmarshalGrant(encoded)
	if err != nil {
		t.Fatalf("unmarshalGrant: %v", err)
	}

	if decoded.Unrestricted {
		t.Error("a scoped grant decoded as unrestricted")
	}
	if len(decoded.Rules) != 2 {
		t.Fatalf("decoded %d rules, want 2", len(decoded.Rules))
	}
	if !decoded.Allows("uploads", "incoming/x", PermissionWrite) {
		t.Error("a permission did not survive the round trip")
	}
	if decoded.Allows("uploads", "elsewhere/x", PermissionWrite) {
		t.Error("the prefix did not survive the round trip")
	}
}

func TestUnmarshalGrantRefusesRatherThanGuessing(t *testing.T) {
	// Both directions of guessing are wrong: defaulting to unrestricted turns a
	// corrupt row into a privilege escalation, and defaulting to no-access
	// looks like a mysterious permission bug. An error says what happened.
	for _, raw := range []string{"", "not json", "[]", "null"} {
		if _, err := unmarshalGrant([]byte(raw)); err == nil {
			t.Errorf("unmarshalGrant(%q) succeeded; want an error", raw)
		}
	}
}

func TestDescribe(t *testing.T) {
	if got := UnrestrictedGrant().Describe(); got != "unrestricted" {
		t.Errorf("got %q", got)
	}
	if got := scoped().Describe(); got != "no access" {
		t.Errorf("got %q", got)
	}
	got := scoped(rule("assets", "", PermissionRead), rule("up", "in/", PermissionWrite)).Describe()
	want := "assets (read); up/in/ (write)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

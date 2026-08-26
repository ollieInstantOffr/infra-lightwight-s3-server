package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const testPassword = "correct-horse-battery-staple"

func newTestUser(t *testing.T, pool *Pool, email string) *User {
	t.Helper()
	user, err := CreateUser(context.Background(), pool, email, RoleMember)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return user
}

func TestPasswordRoundTrip(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	user := newTestUser(t, pool, "round-trip@example.com")

	if err := SetPassword(ctx, pool, user.ID, testPassword, false); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	got, err := VerifyPassword(ctx, pool, "round-trip@example.com", testPassword)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if got.ID != user.ID {
		t.Errorf("verified as %s, want %s", got.ID, user.ID)
	}
	if got.MustChangePassword {
		t.Error("MustChangePassword set on a password the user chose themselves")
	}
}

// The address is stored lowercased, so signing in with different capitalisation
// has to work — otherwise a user who types their address the way their mail
// client displays it is locked out.
func TestVerifyPasswordIsCaseInsensitiveOnTheAddress(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	user := newTestUser(t, pool, "mixedcase@example.com")
	if err := SetPassword(ctx, pool, user.ID, testPassword, false); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	if _, err := VerifyPassword(ctx, pool, "MixedCase@Example.COM", testPassword); err != nil {
		t.Errorf("VerifyPassword with different capitalisation: %v", err)
	}
}

func TestVerifyPasswordRejectsTheWrongPassword(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	user := newTestUser(t, pool, "wrong@example.com")
	if err := SetPassword(ctx, pool, user.ID, testPassword, false); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	_, err := VerifyPassword(ctx, pool, "wrong@example.com", "not-the-password-at-all")
	if !errors.Is(err, ErrPasswordIncorrect) {
		t.Errorf("VerifyPassword with a wrong password = %v, want ErrPasswordIncorrect", err)
	}
}

// An unknown address and a wrong password must be indistinguishable to the
// caller. Returning a different error for an address with no account tells an
// unauthenticated caller which addresses have accounts.
func TestUnknownAddressIsIndistinguishableFromAWrongPassword(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	user := newTestUser(t, pool, "known@example.com")
	if err := SetPassword(ctx, pool, user.ID, testPassword, false); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	_, unknownErr := VerifyPassword(ctx, pool, "nobody@example.com", testPassword)
	_, wrongErr := VerifyPassword(ctx, pool, "known@example.com", "some-other-password")

	if !errors.Is(unknownErr, ErrPasswordIncorrect) {
		t.Fatalf("unknown address = %v, want ErrPasswordIncorrect", unknownErr)
	}
	if !errors.Is(wrongErr, ErrPasswordIncorrect) {
		t.Fatalf("wrong password = %v, want ErrPasswordIncorrect", wrongErr)
	}
	if unknownErr.Error() != wrongErr.Error() {
		t.Errorf("errors differ in text and would leak which addresses exist:\n  unknown: %q\n  wrong:   %q",
			unknownErr, wrongErr)
	}
}

// The errors matching is not enough on its own: returning early for an unknown
// address makes the miss orders of magnitude faster than a hit, and that gap is
// measurable over a network. This is what the dummy hash in VerifyPassword is
// for, and the test fails without it.
func TestUnknownAddressCostsTheSameTimeAsAKnownOne(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test; skipped under -short")
	}
	pool := testPool(t)
	ctx := context.Background()
	user := newTestUser(t, pool, "timed@example.com")
	if err := SetPassword(ctx, pool, user.ID, testPassword, false); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	// Median of several runs: a single sample is at the mercy of scheduling.
	median := func(f func()) time.Duration {
		const runs = 5
		samples := make([]time.Duration, runs)
		for i := range samples {
			start := time.Now()
			f()
			samples[i] = time.Since(start)
		}
		for i := 1; i < len(samples); i++ {
			for j := i; j > 0 && samples[j] < samples[j-1]; j-- {
				samples[j], samples[j-1] = samples[j-1], samples[j]
			}
		}
		return samples[runs/2]
	}

	known := median(func() {
		_, _ = VerifyPassword(ctx, pool, "timed@example.com", "some-other-password")
	})
	unknown := median(func() {
		_, _ = VerifyPassword(ctx, pool, "nobody-at-all@example.com", "some-other-password")
	})

	// Generous: this is catching a return-immediately path, which is hundreds
	// of times faster, not a subtle few-percent difference.
	if unknown < known/2 {
		t.Errorf("an unknown address returns in %v against %v for a known one, "+
			"which is fast enough to enumerate accounts by timing", unknown, known)
	}
}

// An account with no password must not be signable-in-to. The zero-length hash
// case is easy to get wrong: bcrypt comparing against empty bytes errors, but
// only by luck rather than by intent.
func TestAnAccountWithNoPasswordCannotSignIn(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	newTestUser(t, pool, "nopassword@example.com")

	for _, attempt := range []string{"", " ", testPassword} {
		_, err := VerifyPassword(ctx, pool, "nopassword@example.com", attempt)
		if !errors.Is(err, ErrPasswordUnset) {
			t.Errorf("VerifyPassword(%q) on a passwordless account = %v, want ErrPasswordUnset", attempt, err)
		}
	}
}

func TestHasPassword(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	user := newTestUser(t, pool, "haspassword@example.com")

	present, err := HasPassword(ctx, pool, "haspassword@example.com")
	if err != nil {
		t.Fatalf("HasPassword: %v", err)
	}
	if present {
		t.Error("a freshly created account reports having a password")
	}

	if err := SetPassword(ctx, pool, user.ID, testPassword, false); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	present, err = HasPassword(ctx, pool, "haspassword@example.com")
	if err != nil {
		t.Fatalf("HasPassword: %v", err)
	}
	if !present {
		t.Error("an account with a password reports not having one")
	}

	if _, err := HasPassword(ctx, pool, "never-existed@example.com"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("HasPassword for an unknown address = %v, want ErrUserNotFound", err)
	}
}

func TestMustChangePasswordSurvivesVerification(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	user := newTestUser(t, pool, "forced@example.com")

	// An administrator chose this one, so it is a shared secret until replaced.
	if err := SetPassword(ctx, pool, user.ID, testPassword, true); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	got, err := VerifyPassword(ctx, pool, "forced@example.com", testPassword)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !got.MustChangePassword {
		t.Fatal("a password set by an administrator did not come back flagged for change")
	}

	if err := ClearMustChangePassword(ctx, pool, user.ID); err != nil {
		t.Fatalf("ClearMustChangePassword: %v", err)
	}
	got, err = VerifyPassword(ctx, pool, "forced@example.com", testPassword)
	if err != nil {
		t.Fatalf("VerifyPassword after clearing: %v", err)
	}
	if got.MustChangePassword {
		t.Error("the flag survived being cleared")
	}
}

func TestValidatePassword(t *testing.T) {
	cases := map[string]struct {
		password string
		wantErr  error
	}{
		"long enough":         {password: "twelve-chars", wantErr: nil},
		"exactly the minimum": {password: strings.Repeat("a", MinPasswordLength), wantErr: nil},
		"one short":           {password: strings.Repeat("a", MinPasswordLength-1), wantErr: ErrPasswordTooShort},
		"empty":               {password: "", wantErr: ErrPasswordTooShort},
		"at the byte limit":   {password: strings.Repeat("a", MaxPasswordLength), wantErr: nil},
		"over the byte limit": {password: strings.Repeat("a", MaxPasswordLength+1), wantErr: ErrPasswordTooLong},
		// Counted in runes for the floor and bytes for the ceiling: 30 emoji are
		// plainly long enough to be a password, but bcrypt would silently
		// truncate them, making two different passwords interchangeable.
		"multi-byte over the limit": {password: strings.Repeat("😀", 30), wantErr: ErrPasswordTooLong},
		"multi-byte within limits":  {password: strings.Repeat("é", 12), wantErr: nil},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidatePassword(c.password)
			if c.wantErr == nil {
				if err != nil {
					t.Errorf("ValidatePassword rejected a valid password: %v", err)
				}
				return
			}
			if !errors.Is(err, c.wantErr) {
				t.Errorf("ValidatePassword = %v, want %v", err, c.wantErr)
			}
		})
	}
}

// A password over bcrypt's limit must be refused rather than stored truncated.
// Storing it would mean any password sharing the first 72 bytes also works.
func TestSetPasswordRefusesAnUnstorablePassword(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	user := newTestUser(t, pool, "toolong@example.com")

	long := strings.Repeat("a", MaxPasswordLength+10)
	if err := SetPassword(ctx, pool, user.ID, long, false); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("SetPassword with an over-long password = %v, want ErrPasswordTooLong", err)
	}
	// And it must not have been stored: the account still cannot sign in.
	if _, err := VerifyPassword(ctx, pool, "toolong@example.com", long); !errors.Is(err, ErrPasswordUnset) {
		t.Errorf("after a rejected SetPassword the account = %v, want ErrPasswordUnset", err)
	}
}

func TestSetPasswordByEmailAndUnknownUsers(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	newTestUser(t, pool, "by-email@example.com")

	if err := SetPasswordByEmail(ctx, pool, "By-Email@Example.com", testPassword, true); err != nil {
		t.Fatalf("SetPasswordByEmail: %v", err)
	}
	if _, err := VerifyPassword(ctx, pool, "by-email@example.com", testPassword); err != nil {
		t.Errorf("VerifyPassword after SetPasswordByEmail: %v", err)
	}

	if err := SetPasswordByEmail(ctx, pool, "nobody@example.com", testPassword, false); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("SetPasswordByEmail for an unknown address = %v, want ErrUserNotFound", err)
	}
}

// The two counters exist so that an attacker guessing at one account cannot
// lock its owner out through a shared counter, and a botnet cannot spread
// guessing thin enough to never trip anything. That only holds if they really
// are counted independently.
func TestLoginFailuresAreCountedByAddressAndAddressSeparately(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// Three failures against one account from one address.
	for i := 0; i < 3; i++ {
		if err := RecordLoginAttempt(ctx, pool, "victim@example.com", "10.0.0.1", false); err != nil {
			t.Fatalf("RecordLoginAttempt: %v", err)
		}
	}
	// Two more from the same IP, but against a different account.
	for i := 0; i < 2; i++ {
		if err := RecordLoginAttempt(ctx, pool, "someone-else@example.com", "10.0.0.1", false); err != nil {
			t.Fatalf("RecordLoginAttempt: %v", err)
		}
	}

	byEmail, byIP, err := CountRecentFailures(ctx, pool, "victim@example.com", "10.0.0.1", time.Hour)
	if err != nil {
		t.Fatalf("CountRecentFailures: %v", err)
	}
	if byEmail != 3 {
		t.Errorf("failures for the address = %d, want 3", byEmail)
	}
	if byIP != 5 {
		t.Errorf("failures from the IP = %d, want 5 (all accounts, not just this one)", byIP)
	}

	// A different IP entirely sees the account's failures but none of its own.
	byEmail, byIP, err = CountRecentFailures(ctx, pool, "victim@example.com", "192.0.2.7", time.Hour)
	if err != nil {
		t.Fatalf("CountRecentFailures: %v", err)
	}
	if byEmail != 3 || byIP != 0 {
		t.Errorf("from an unrelated IP got (email=%d, ip=%d), want (3, 0)", byEmail, byIP)
	}
}

func TestSuccessfulAttemptsAreNotCountedAsFailures(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	if err := RecordLoginAttempt(ctx, pool, "fine@example.com", "10.0.0.2", true); err != nil {
		t.Fatalf("RecordLoginAttempt: %v", err)
	}
	byEmail, byIP, err := CountRecentFailures(ctx, pool, "fine@example.com", "10.0.0.2", time.Hour)
	if err != nil {
		t.Fatalf("CountRecentFailures: %v", err)
	}
	if byEmail != 0 || byIP != 0 {
		t.Errorf("a successful sign-in counted as a failure: (email=%d, ip=%d)", byEmail, byIP)
	}
}

// Failures outside the window must not count, or a lockout would be permanent
// rather than temporary.
func TestOldFailuresFallOutOfTheWindow(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	if err := RecordLoginAttempt(ctx, pool, "stale@example.com", "10.0.0.3", false); err != nil {
		t.Fatalf("RecordLoginAttempt: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE login_attempts SET at = now() - interval '2 hours' WHERE email = $1`,
		"stale@example.com"); err != nil {
		t.Fatalf("age the attempt: %v", err)
	}

	byEmail, _, err := CountRecentFailures(ctx, pool, "stale@example.com", "10.0.0.3", 15*time.Minute)
	if err != nil {
		t.Fatalf("CountRecentFailures: %v", err)
	}
	if byEmail != 0 {
		t.Errorf("a two-hour-old failure counted inside a 15-minute window: %d", byEmail)
	}
}

// Signing in successfully has to clear the count, or a user who mistypes twice
// and then succeeds stays part-way to a lockout for the rest of the window.
func TestClearLoginFailures(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		if err := RecordLoginAttempt(ctx, pool, "recovered@example.com", "10.0.0.4", false); err != nil {
			t.Fatalf("RecordLoginAttempt: %v", err)
		}
	}
	if err := ClearLoginFailures(ctx, pool, "recovered@example.com"); err != nil {
		t.Fatalf("ClearLoginFailures: %v", err)
	}
	byEmail, _, err := CountRecentFailures(ctx, pool, "recovered@example.com", "10.0.0.4", time.Hour)
	if err != nil {
		t.Fatalf("CountRecentFailures: %v", err)
	}
	if byEmail != 0 {
		t.Errorf("failures remained after clearing: %d", byEmail)
	}
}

func TestPurgeLoginAttempts(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	if err := RecordLoginAttempt(ctx, pool, "old@example.com", "10.0.0.5", false); err != nil {
		t.Fatalf("RecordLoginAttempt: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE login_attempts SET at = now() - interval '40 days'`); err != nil {
		t.Fatalf("age the attempt: %v", err)
	}
	if err := PurgeLoginAttempts(ctx, pool, 30*24*time.Hour); err != nil {
		t.Fatalf("PurgeLoginAttempts: %v", err)
	}

	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM login_attempts`).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d attempts survived the purge, want 0", remaining)
	}
}

// Two accounts with the same password must not share a hash. bcrypt salts
// automatically, but a future change to the hashing helper could lose that,
// and identical hashes would make one cracked password crack every account
// that shares it.
func TestIdenticalPasswordsGetDifferentHashes(t *testing.T) {
	first, err := HashPassword(testPassword)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	second, err := HashPassword(testPassword)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if string(first) == string(second) {
		t.Error("the same password hashed twice produced identical hashes, so the salt is not being applied")
	}
}

// The forced-change flag is only enforceable if it survives the session
// lookup: the middleware that blocks a flagged user reads the user from
// TouchSession on every request, not from the sign-in response.
func TestTouchSessionCarriesTheForcedChangeFlag(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	user := newTestUser(t, pool, "session-flag@example.com")
	if err := SetPassword(ctx, pool, user.ID, testPassword, true); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	token, hash, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	_ = token
	if _, err := CreateSession(ctx, pool, user.ID, hash, time.Hour, 24*time.Hour, "test", "10.0.0.9"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := TouchSession(ctx, pool, hash, time.Hour)
	if err != nil {
		t.Fatalf("TouchSession: %v", err)
	}
	if !got.MustChangePassword {
		t.Error("TouchSession dropped must_change_password, so the forced change could be skipped " +
			"by navigating to any other page")
	}
}

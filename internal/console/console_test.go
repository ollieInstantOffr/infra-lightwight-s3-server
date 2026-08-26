package console

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/httpx"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/metrics"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/s3api"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/secrets"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/storage"
)

// captureMailer records what would have been sent, so the sign-in link can be
// followed without any email leaving the process.
type captureMailer struct {
	mu       sync.Mutex
	messages []sentMessage
	failWith error
}

type sentMessage struct{ to, subject, text string }

func (m *captureMailer) Send(_ context.Context, to, subject, text, _ string) error {
	if m.failWith != nil {
		return m.failWith
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, sentMessage{to, subject, text})
	return nil
}

func (m *captureMailer) last(t *testing.T) sentMessage {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.messages) == 0 {
		t.Fatal("no email was sent")
	}
	return m.messages[len(m.messages)-1]
}

func (m *captureMailer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.messages)
}

// consoleFixture is a running console with a client that keeps cookies.
type consoleFixture struct {
	url    string
	client *http.Client
	mailer *captureMailer
	pool   *db.Pool
	admin  *db.User
	// server is exposed so a test can adjust configuration the fixture does not
	// take as an argument, such as the metrics token.
	server *Server
}

func newConsole(t *testing.T) *consoleFixture {
	t.Helper()

	dsn := testDSN(t, "test_console_pkg")
	ctx := context.Background()

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := db.Migrate(ctx, pool, quiet); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, stmt := range []string{
		`DELETE FROM buckets`, `DELETE FROM blobs`, `DELETE FROM credentials`,
		`DELETE FROM sessions`, `DELETE FROM magic_links`, `DELETE FROM invites`, `DELETE FROM users`,
		// Shared schema again: without this, the throttle tests leave failures
		// behind that lock every later test out of signing in.
		`DELETE FROM login_attempts`,
		// request_logs and request_metrics are shared across every test in this
		// package (one schema, test_console_pkg) — without clearing them, a
		// test that seeds either accumulates rows from every prior run rather
		// than starting from a known state.
		`DELETE FROM request_logs`, `DELETE FROM request_metrics`,
		`DELETE FROM server_events`, `DELETE FROM alerts`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("reset (%s): %v", stmt, err)
		}
	}

	admin, err := db.EnsureAdmin(ctx, pool, "admin@example.com")
	if err != nil {
		t.Fatalf("EnsureAdmin: %v", err)
	}
	// EnsureAdmin cannot invent a password, so the fixture sets one the way
	// `s3d user set-password` does on a real deployment.
	if err := db.SetPassword(ctx, pool, admin.ID, testAdminPassword, false); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	cipher, err := secrets.NewCipher("console-test-credentials-key-32-chars-ok")
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	blobs, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	trust, _ := httpx.NewProxyTrust(nil)
	mailer := &captureMailer{}

	server := &Server{
		DB: pool, Blobs: blobs, Cipher: cipher, Mailer: mailer, Proxies: trust,
		Log: quiet, Region: "us-east-1", SessionSecret: strings.Repeat("s", 40),
		Registry: metrics.NewRegistry("test"),
		Live:     metrics.NewLiveWindow(),
		InFlight: s3api.NewInFlight(),
	}

	httpSrv := httptest.NewServer(server.Handler())
	t.Cleanup(httpSrv.Close)
	server.PublicURL = httpSrv.URL
	server.PublicS3URL = httpSrv.URL

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar: jar,
		// Redirects are followed manually so the callback's Location can be
		// inspected, which is where sign-in failures are reported.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	return &consoleFixture{
		url: httpSrv.URL, client: client, mailer: mailer,
		pool: pool, admin: admin, server: server,
	}
}

// do sends a request and returns the status and decoded body.
func (c *consoleFixture) do(t *testing.T, method, path string, payload any) (int, map[string]any) {
	t.Helper()

	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("encode payload: %v", err)
		}
		body = bytes.NewReader(encoded)
	}

	request, err := http.NewRequest(method, c.url+path, body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()

	raw, _ := io.ReadAll(response.Body)
	decoded := map[string]any{}
	if len(raw) > 0 && strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("%s %s returned unparseable JSON: %v\n%s", method, path, err, raw)
		}
	}
	return response.StatusCode, decoded
}

// testAdminPassword is the bootstrap admin's password in every fixture.
const testAdminPassword = "fixture-admin-password"

// signIn leaves the client holding a session cookie.
func (c *consoleFixture) signIn(t *testing.T, email string) {
	t.Helper()
	c.signInWith(t, email, testAdminPassword)
}

// signInWith is the same, for tests that care which password was used.
func (c *consoleFixture) signInWith(t *testing.T, email, password string) {
	t.Helper()
	status, body := c.do(t, http.MethodPost, "/api/auth/login",
		map[string]string{"email": email, "password": password})
	if status != http.StatusOK {
		t.Fatalf("signing in as %s returned %d: %v", email, status, body)
	}
}

func TestPasswordSignIn(t *testing.T) {
	c := newConsole(t)
	c.signIn(t, "admin@example.com")

	status, body := c.do(t, http.MethodGet, "/api/auth/me", nil)
	if status != http.StatusOK {
		t.Fatalf("/api/auth/me returned %d after signing in", status)
	}
	if body["email"] != "admin@example.com" {
		t.Errorf("signed in as %v, want admin@example.com", body["email"])
	}
	if body["isAdmin"] != true {
		t.Error("the bootstrap admin is not an admin")
	}
}

func TestSignInRejectsTheWrongPassword(t *testing.T) {
	c := newConsole(t)

	status, _ := c.do(t, http.MethodPost, "/api/auth/login",
		map[string]string{"email": "admin@example.com", "password": "not-the-password"})
	if status != http.StatusUnauthorized {
		t.Fatalf("a wrong password returned %d, want 401", status)
	}
	// And no session was issued.
	if meStatus, _ := c.do(t, http.MethodGet, "/api/auth/me", nil); meStatus != http.StatusUnauthorized {
		t.Errorf("/api/auth/me returned %d after a failed sign-in, want 401", meStatus)
	}
}

// The endpoint is unauthenticated by necessity, so it must not reveal which
// addresses have accounts — the same property the magic-link endpoint it
// replaced had to hold.
func TestSignInDoesNotRevealWhoHasAnAccount(t *testing.T) {
	c := newConsole(t)

	knownStatus, knownBody := c.do(t, http.MethodPost, "/api/auth/login",
		map[string]string{"email": "admin@example.com", "password": "wrong-password-here"})
	unknownStatus, unknownBody := c.do(t, http.MethodPost, "/api/auth/login",
		map[string]string{"email": "stranger@example.com", "password": "wrong-password-here"})

	if knownStatus != unknownStatus {
		t.Errorf("status differs: known %d, unknown %d", knownStatus, unknownStatus)
	}
	if fmt.Sprint(knownBody) != fmt.Sprint(unknownBody) {
		t.Errorf("response differs and would reveal which addresses exist:\n known: %v\n unknown: %v",
			knownBody, unknownBody)
	}
}

// An account that exists but has no password — every account does, immediately
// after the 0008 migration — must be refused, and refused indistinguishably.
func TestAnAccountWithNoPasswordCannotSignIn(t *testing.T) {
	c := newConsole(t)
	ctx := context.Background()

	if _, err := db.CreateUser(ctx, c.pool, "nopassword@example.com", db.RoleMember); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	status, body := c.do(t, http.MethodPost, "/api/auth/login",
		map[string]string{"email": "nopassword@example.com", "password": "anything-at-all"})
	if status != http.StatusUnauthorized {
		t.Fatalf("an account with no password returned %d, want 401", status)
	}
	if body["error"] != signInFailed {
		t.Errorf("error was %q, which differs from an ordinary failure and reveals the account exists",
			body["error"])
	}
}

// Without a bound, the endpoint is an offline password cracker with unlimited
// attempts.
func TestSignInIsThrottled(t *testing.T) {
	c := newConsole(t)

	var lastStatus int
	for i := 0; i < loginFailureLimitPerEmail+2; i++ {
		lastStatus, _ = c.do(t, http.MethodPost, "/api/auth/login",
			map[string]string{"email": "admin@example.com", "password": "wrong-every-time"})
	}
	if lastStatus != http.StatusTooManyRequests {
		t.Fatalf("after %d failures the status was %d, want 429",
			loginFailureLimitPerEmail+2, lastStatus)
	}

	// And the throttle holds even once the right password is offered, or it
	// would be no bound at all.
	status, _ := c.do(t, http.MethodPost, "/api/auth/login",
		map[string]string{"email": "admin@example.com", "password": testAdminPassword})
	if status != http.StatusTooManyRequests {
		t.Errorf("the correct password bypassed the throttle with %d, want 429", status)
	}
}

// A successful sign-in has to clear the count, or a user who mistypes a few
// times and then succeeds stays part-way to a lockout.
func TestASuccessfulSignInClearsTheFailureCount(t *testing.T) {
	c := newConsole(t)

	for i := 0; i < loginFailureLimitPerEmail-1; i++ {
		c.do(t, http.MethodPost, "/api/auth/login",
			map[string]string{"email": "admin@example.com", "password": "wrong"})
	}
	c.signIn(t, "admin@example.com")

	// Fresh failures must start from zero rather than tripping immediately.
	for i := 0; i < loginFailureLimitPerEmail-1; i++ {
		status, _ := c.do(t, http.MethodPost, "/api/auth/login",
			map[string]string{"email": "admin@example.com", "password": "wrong"})
		if status == http.StatusTooManyRequests {
			t.Fatalf("throttled on failure %d after a successful sign-in should have cleared the count", i+1)
		}
	}
}

func TestUnauthenticatedRequestsAreRejected(t *testing.T) {
	c := newConsole(t)

	for _, path := range []string{"/api/auth/me", "/api/buckets", "/api/credentials", "/api/users", "/api/dashboard"} {
		status, _ := c.do(t, http.MethodGet, path, nil)
		if status != http.StatusUnauthorized {
			t.Errorf("GET %s without a session returned %d, want 401", path, status)
		}
	}
}

// Signing out must end the session on the server too. Clearing the cookie alone
// would leave a copied token valid.
func TestLogoutRevokesTheSessionServerSide(t *testing.T) {
	c := newConsole(t)
	c.signIn(t, "admin@example.com")

	// Capture the cookie so it can be replayed after signing out.
	parsed, _ := url.Parse(c.url)
	cookies := c.client.Jar.Cookies(parsed)
	var sessionToken string
	for _, cookie := range cookies {
		if cookie.Name == sessionCookieName {
			sessionToken = cookie.Value
		}
	}
	if sessionToken == "" {
		t.Fatal("no session cookie was set")
	}

	if status, _ := c.do(t, http.MethodPost, "/api/auth/logout", nil); status != http.StatusOK {
		t.Fatalf("logout returned %d", status)
	}

	// Replay the captured cookie against a fresh client.
	request, _ := http.NewRequest(http.MethodGet, c.url+"/api/auth/me", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionToken})
	response, err := (&http.Client{}).Do(request)
	if err != nil {
		t.Fatalf("replay request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("a session token replayed after logout returned %d, want 401", response.StatusCode)
	}
}

// The session cookie must be inaccessible to scripts, or a cross-site scripting
// bug becomes a session theft.
func TestSessionCookieIsHardened(t *testing.T) {
	c := newConsole(t)

	payload, err := json.Marshal(map[string]string{
		"email": "admin@example.com", "password": testAdminPassword,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	response, err := c.client.Post(c.url+"/api/auth/login", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("signing in returned %d", response.StatusCode)
	}

	for _, cookie := range response.Cookies() {
		if cookie.Name != sessionCookieName {
			continue
		}
		if !cookie.HttpOnly {
			t.Error("the session cookie is not HttpOnly, so a script could read it")
		}
		// Lax rather than Strict so a top-level navigation into the console
		// from an external link still carries the session; Strict would drop
		// it and show a signed-out console until the user reloaded.
		if cookie.SameSite != http.SameSiteLaxMode {
			t.Errorf("SameSite = %v, want Lax", cookie.SameSite)
		}
		return
	}
	t.Fatal("no session cookie was set")
}

// A state-changing request carrying another site's Origin must be refused.
func TestCrossOriginWritesAreRejected(t *testing.T) {
	c := newConsole(t)
	c.signIn(t, "admin@example.com")

	payload, _ := json.Marshal(map[string]string{"name": "evil-bucket"})
	request, _ := http.NewRequest(http.MethodPost, c.url+"/api/buckets", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://attacker.example.com")

	response, err := c.client.Do(request)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusForbidden {
		t.Errorf("a cross-origin write returned %d, want 403", response.StatusCode)
	}
}

func TestCreateUserFlow(t *testing.T) {
	c := newConsole(t)
	c.signIn(t, "admin@example.com")

	status, body := c.do(t, http.MethodPost, "/api/users",
		map[string]string{"email": "colleague@example.com", "role": db.RoleMember})
	if status != http.StatusCreated {
		t.Fatalf("creating a user returned %d: %v", status, body)
	}

	// The starting password comes back once, and is the only copy: the server
	// stores a hash.
	password, _ := body["password"].(string)
	if password == "" {
		t.Fatal("no starting password was returned, so the account cannot be handed over")
	}

	created := newClientFor(t, c)
	created.signInWith(t, "colleague@example.com", password)

	status, me := created.do(t, http.MethodGet, "/api/auth/me", nil)
	if status != http.StatusOK {
		t.Fatalf("the new user could not read their own profile: %d", status)
	}
	if me["email"] != "colleague@example.com" {
		t.Errorf("signed in as %v", me["email"])
	}
	if me["isAdmin"] != false {
		t.Error("a new member was made an admin")
	}
	// An administrator chose this password, so it is a shared secret until the
	// owner replaces it.
	if me["mustChangePassword"] != true {
		t.Error("a password set by an administrator did not come back flagged for change")
	}
}

// An address with no account must not be able to sign in, whatever it sends.
func TestAnAddressWithNoAccountCannotSignIn(t *testing.T) {
	c := newConsole(t)

	status, _ := c.do(t, http.MethodPost, "/api/auth/login",
		map[string]string{"email": "stranger@example.com", "password": testAdminPassword})
	if status != http.StatusUnauthorized {
		t.Fatalf("an unknown address returned %d, want 401", status)
	}
}

// A user whose password was chosen for them may change it and leave, and
// nothing else. Enforced server-side, because a check that only lives in the
// app is bypassed by calling the API directly.
func TestAForcedPasswordChangeCannotBeSkipped(t *testing.T) {
	c := newConsole(t)
	c.signIn(t, "admin@example.com")

	_, body := c.do(t, http.MethodPost, "/api/users",
		map[string]string{"email": "forced@example.com", "role": db.RoleMember})
	password, _ := body["password"].(string)

	forced := newClientFor(t, c)
	forced.signInWith(t, "forced@example.com", password)

	// Every ordinary route is refused, with a code the app can route on.
	for _, path := range []string{"/api/buckets", "/api/credentials", "/api/dashboard"} {
		status, refused := forced.do(t, http.MethodGet, path, nil)
		if status != http.StatusForbidden {
			t.Errorf("GET %s with a forced change pending returned %d, want 403", path, status)
		}
		if refused["code"] != codePasswordChangeRequired {
			t.Errorf("GET %s returned code %v, want %q", path, refused["code"], codePasswordChangeRequired)
		}
	}

	// But the two things they are allowed to do still work.
	if status, _ := forced.do(t, http.MethodGet, "/api/auth/me", nil); status != http.StatusOK {
		t.Errorf("/api/auth/me was refused with %d, leaving the app unable to explain itself", status)
	}

	status, changed := forced.do(t, http.MethodPost, "/api/account/password",
		map[string]string{"currentPassword": password, "newPassword": "a-brand-new-password"})
	if status != http.StatusOK {
		t.Fatalf("changing the password returned %d: %v", status, changed)
	}

	// And the block lifts.
	if status, _ := forced.do(t, http.MethodGet, "/api/buckets", nil); status != http.StatusOK {
		t.Errorf("GET /api/buckets after the change returned %d, want 200", status)
	}
}

// Re-entering the administrator's password would clear the flag while leaving
// the shared secret in place, which is the one case the flag exists to prevent.
func TestAForcedChangeRefusesTheSamePassword(t *testing.T) {
	c := newConsole(t)
	c.signIn(t, "admin@example.com")

	_, body := c.do(t, http.MethodPost, "/api/users",
		map[string]string{"email": "same@example.com", "role": db.RoleMember})
	password, _ := body["password"].(string)

	forced := newClientFor(t, c)
	forced.signInWith(t, "same@example.com", password)

	status, _ := forced.do(t, http.MethodPost, "/api/account/password",
		map[string]string{"currentPassword": password, "newPassword": password})
	if status != http.StatusBadRequest {
		t.Errorf("reusing the same password returned %d, want 400", status)
	}
}

// Changing a password must end every other session. If the reason for changing
// it is that somebody else had it, leaving their session alive defeats the point.
func TestChangingAPasswordSignsOutOtherSessions(t *testing.T) {
	c := newConsole(t)
	c.signIn(t, "admin@example.com")

	// A second browser for the same account.
	other := newClientFor(t, c)
	other.signIn(t, "admin@example.com")
	if status, _ := other.do(t, http.MethodGet, "/api/auth/me", nil); status != http.StatusOK {
		t.Fatal("the second session was not established")
	}

	status, body := c.do(t, http.MethodPost, "/api/account/password",
		map[string]string{"currentPassword": testAdminPassword, "newPassword": "an-entirely-new-password"})
	if status != http.StatusOK {
		t.Fatalf("changing the password returned %d: %v", status, body)
	}

	if status, _ := other.do(t, http.MethodGet, "/api/auth/me", nil); status != http.StatusUnauthorized {
		t.Errorf("the other session survived the password change with %d, want 401", status)
	}
	// The session that made the change keeps working.
	if status, _ := c.do(t, http.MethodGet, "/api/auth/me", nil); status != http.StatusOK {
		t.Errorf("the changing session was signed out with %d", status)
	}
}

// A stolen session must not be enough to change the password: without the
// current one, a thief could lock the real owner out of their own account.
func TestChangingAPasswordNeedsTheCurrentOne(t *testing.T) {
	c := newConsole(t)
	c.signIn(t, "admin@example.com")

	status, _ := c.do(t, http.MethodPost, "/api/account/password",
		map[string]string{"currentPassword": "not-the-current-one", "newPassword": "a-new-password-here"})
	if status != http.StatusUnauthorized {
		t.Fatalf("changing without the current password returned %d, want 401", status)
	}
	// The old password still works, so nothing was changed.
	fresh := newClientFor(t, c)
	fresh.signIn(t, "admin@example.com")
}

// An administrator resetting someone's password must also end their sessions:
// the old password is no longer trusted, so neither is anything signed in with it.
func TestAnAdminResetSignsTheUserOut(t *testing.T) {
	c := newConsole(t)
	c.signIn(t, "admin@example.com")

	_, created := c.do(t, http.MethodPost, "/api/users",
		map[string]string{"email": "reset-me@example.com", "role": db.RoleMember})
	password, _ := created["password"].(string)
	userID, _ := created["user"].(map[string]any)["id"].(string)

	victim := newClientFor(t, c)
	victim.signInWith(t, "reset-me@example.com", password)
	// Clear the forced-change flag so the 401 below cannot be the flag's 403.
	if status, _ := victim.do(t, http.MethodPost, "/api/account/password",
		map[string]string{"currentPassword": password, "newPassword": "chosen-by-the-owner"}); status != http.StatusOK {
		t.Fatalf("the user could not set their own password: %d", status)
	}

	status, reset := c.do(t, http.MethodPost, "/api/users/"+userID+"/password", nil)
	if status != http.StatusOK {
		t.Fatalf("resetting returned %d: %v", status, reset)
	}
	if reset["password"] == "" || reset["password"] == nil {
		t.Error("no new password was returned, so it cannot be handed over")
	}

	if status, _ := victim.do(t, http.MethodGet, "/api/auth/me", nil); status != http.StatusUnauthorized {
		t.Errorf("the user's session survived an administrator reset with %d, want 401", status)
	}
}

// Members must not be able to perform admin actions.
func TestMembersCannotAdminister(t *testing.T) {
	c := newConsole(t)
	c.signIn(t, "admin@example.com")
	status, created := c.do(t, http.MethodPost, "/api/users",
		map[string]string{"email": "member@example.com", "role": db.RoleMember})
	if status != http.StatusCreated {
		t.Fatalf("creating a member returned %d: %v", status, created)
	}
	password, _ := created["password"].(string)

	member := newClientFor(t, c)
	member.signInWith(t, "member@example.com", password)
	// Clear the forced change, or every call below returns 403 for that reason
	// rather than because the member lacks the role.
	if status, _ := member.do(t, http.MethodPost, "/api/account/password",
		map[string]string{"currentPassword": password, "newPassword": "member-chosen-password"}); status != http.StatusOK {
		t.Fatalf("the member could not set their own password: %d", status)
	}

	for _, call := range []struct {
		method, path string
		payload      any
	}{
		{http.MethodGet, "/api/users", nil},
		{http.MethodPost, "/api/users", map[string]string{"email": "another@example.com"}},
		{http.MethodGet, "/api/credentials", nil},
		{http.MethodPost, "/api/credentials", map[string]string{"description": "sneaky"}},
	} {
		status, _ := member.do(t, call.method, call.path, call.payload)
		if status != http.StatusForbidden {
			t.Errorf("%s %s as a member returned %d, want 403", call.method, call.path, status)
		}
	}

	// But ordinary storage work is allowed.
	if status, _ := member.do(t, http.MethodGet, "/api/buckets", nil); status != http.StatusOK {
		t.Errorf("a member could not list buckets: %d", status)
	}
}

// The console must never be left with no administrator, or user management
// becomes permanently unreachable.
func TestLastAdminIsProtected(t *testing.T) {
	c := newConsole(t)
	c.signIn(t, "admin@example.com")

	status, body := c.do(t, http.MethodPut, "/api/users/"+c.admin.ID+"/role",
		map[string]string{"role": db.RoleMember})
	if status != http.StatusConflict {
		t.Errorf("demoting the only admin returned %d, want 409 (%v)", status, body)
	}

	status, _ = c.do(t, http.MethodDelete, "/api/users/"+c.admin.ID, nil)
	if status == http.StatusOK {
		t.Error("the only admin was removed")
	}
}

// newClientFor returns a second client against the same server, so two people
// can be signed in at once.
func newClientFor(t *testing.T, c *consoleFixture) *consoleFixture {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	return &consoleFixture{
		url:    c.url,
		client: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		mailer: c.mailer,
		pool:   c.pool,
		admin:  c.admin,
	}
}

func TestCredentialLifecycle(t *testing.T) {
	c := newConsole(t)
	c.signIn(t, "admin@example.com")

	status, created := c.do(t, http.MethodPost, "/api/credentials",
		map[string]string{"description": "backup script"})
	if status != http.StatusCreated {
		t.Fatalf("creating a credential returned %d: %v", status, created)
	}

	accessKeyID, _ := created["accessKeyId"].(string)
	secret, _ := created["secretAccessKey"].(string)
	if accessKeyID == "" || secret == "" {
		t.Fatal("the created credential has no key pair")
	}
	if len(secret) != 40 {
		t.Errorf("secret length = %d, want 40", len(secret))
	}
	// Connection snippets are what get a user from zero to a working client.
	snippets, _ := created["snippets"].(map[string]any)
	for _, want := range []string{"awscli", "boto3", "go", "nodejs", "env"} {
		if snippets[want] == nil {
			t.Errorf("no %s connection snippet was returned", want)
		}
	}

	// Listing must never include the secret again.
	status, listed := c.do(t, http.MethodGet, "/api/credentials", nil)
	if status != http.StatusOK {
		t.Fatalf("listing credentials returned %d", status)
	}
	raw, _ := json.Marshal(listed)
	if strings.Contains(string(raw), secret) {
		t.Error("the secret appears in the credential listing; it must be shown exactly once")
	}

	status, _ = c.do(t, http.MethodDelete, "/api/credentials/"+accessKeyID, nil)
	if status != http.StatusOK {
		t.Fatalf("revoking returned %d", status)
	}
	status, listed = c.do(t, http.MethodGet, "/api/credentials", nil)
	credentials, _ := listed["credentials"].([]any)
	if len(credentials) != 1 {
		t.Fatalf("got %d credentials after revoking, want 1 still listed", len(credentials))
	}
	if first, _ := credentials[0].(map[string]any); first["revoked"] != true {
		t.Error("the revoked credential is not marked revoked")
	}
}

func TestBucketAndObjectManagement(t *testing.T) {
	c := newConsole(t)
	c.signIn(t, "admin@example.com")

	if status, body := c.do(t, http.MethodPost, "/api/buckets",
		map[string]string{"name": "console-bucket"}); status != http.StatusCreated {
		t.Fatalf("creating a bucket returned %d: %v", status, body)
	}

	// An invalid name must be reported with the specific rule, not a 500.
	status, body := c.do(t, http.MethodPost, "/api/buckets", map[string]string{"name": "Invalid_Name"})
	if status != http.StatusBadRequest {
		t.Errorf("an invalid bucket name returned %d, want 400", status)
	}
	// The message must name the specific rule rather than falling back to S3's
	// generic "The specified bucket is not valid.", which tells a user nothing.
	message, _ := body["error"].(string)
	if message == "" || strings.Contains(message, "is not valid.") {
		t.Errorf("error message %q does not name the rule that was broken", message)
	}

	// Upload, list, download.
	upload := func(key, contents string) {
		t.Helper()
		request, _ := http.NewRequest(http.MethodPost,
			c.url+"/api/buckets/console-bucket/objects?key="+url.QueryEscape(key),
			strings.NewReader(contents))
		request.Header.Set("Content-Type", "text/plain")
		response, err := c.client.Do(request)
		if err != nil {
			t.Fatalf("upload %q: %v", key, err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusCreated {
			raw, _ := io.ReadAll(response.Body)
			t.Fatalf("uploading %q returned %d: %s", key, response.StatusCode, raw)
		}
	}
	upload("readme.txt", "hello from the console")
	upload("photos/one.txt", "photo one")
	upload("photos/two.txt", "photo two")

	status, listing := c.do(t, http.MethodGet, "/api/buckets/console-bucket/objects", nil)
	if status != http.StatusOK {
		t.Fatalf("listing objects returned %d", status)
	}
	objects, _ := listing["objects"].([]any)
	folders, _ := listing["folders"].([]any)
	if len(objects) != 1 || len(folders) != 1 {
		t.Errorf("got %d objects and %d folders at the root, want 1 and 1", len(objects), len(folders))
	}

	// Download returns the bytes, and must not let uploaded HTML run with the
	// console's own origin.
	response, err := c.client.Get(c.url + "/api/buckets/console-bucket/object?key=readme.txt")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer response.Body.Close()
	downloaded, _ := io.ReadAll(response.Body)
	if string(downloaded) != "hello from the console" {
		t.Errorf("downloaded %q", downloaded)
	}
	if csp := response.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Errorf("Content-Security-Policy = %q; uploaded HTML could run with the console's cookies", csp)
	}
	if response.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("X-Content-Type-Options is not nosniff")
	}

	// Deleting a whole prefix is what "delete folder" means here.
	status, deleted := c.do(t, http.MethodPost, "/api/buckets/console-bucket/objects/delete",
		map[string]any{"prefix": "photos/"})
	if status != http.StatusOK {
		t.Fatalf("deleting a prefix returned %d: %v", status, deleted)
	}
	if count, _ := deleted["deleted"].(float64); count != 2 {
		t.Errorf("deleted %v objects, want 2", deleted["deleted"])
	}

	// A non-empty bucket cannot be deleted.
	if status, _ := c.do(t, http.MethodDelete, "/api/buckets/console-bucket", nil); status != http.StatusConflict {
		t.Errorf("deleting a non-empty bucket returned %d, want 409", status)
	}
}

func TestDashboardTotals(t *testing.T) {
	c := newConsole(t)
	c.signIn(t, "admin@example.com")

	if status, _ := c.do(t, http.MethodPost, "/api/buckets", map[string]string{"name": "dash"}); status != http.StatusCreated {
		t.Fatal("could not create a bucket")
	}
	request, _ := http.NewRequest(http.MethodPost,
		c.url+"/api/buckets/dash/objects?key=file.bin", bytes.NewReader(make([]byte, 4096)))
	response, err := c.client.Do(request)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	response.Body.Close()

	status, body := c.do(t, http.MethodGet, "/api/dashboard", nil)
	if status != http.StatusOK {
		t.Fatalf("dashboard returned %d", status)
	}
	if body["buckets"].(float64) != 1 {
		t.Errorf("buckets = %v, want 1", body["buckets"])
	}
	if body["objects"].(float64) != 1 {
		t.Errorf("objects = %v, want 1", body["objects"])
	}
	if body["bytesStored"].(float64) != 4096 {
		t.Errorf("bytesStored = %v, want 4096", body["bytesStored"])
	}
	// The single-copy limitation is stated where someone reading a storage
	// dashboard will actually see it.
	if note, _ := body["durabilityNote"].(string); !strings.Contains(note, "single copy") {
		t.Errorf("durabilityNote = %q, want it to state the single-copy limitation", note)
	}
}

func TestShareLink(t *testing.T) {
	c := newConsole(t)
	c.signIn(t, "admin@example.com")

	// Without a credential to sign with there can be no share link, and the
	// message should say so rather than failing opaquely.
	if status, _ := c.do(t, http.MethodPost, "/api/buckets", map[string]string{"name": "shared"}); status != http.StatusCreated {
		t.Fatal("could not create a bucket")
	}
	request, _ := http.NewRequest(http.MethodPost,
		c.url+"/api/buckets/shared/objects?key=doc.txt", strings.NewReader("shared contents"))
	response, _ := c.client.Do(request)
	response.Body.Close()

	status, body := c.do(t, http.MethodPost, "/api/buckets/shared/share", map[string]any{"key": "doc.txt"})
	if status != http.StatusConflict {
		t.Errorf("sharing without a credential returned %d, want 409 (%v)", status, body)
	}

	if status, _ := c.do(t, http.MethodPost, "/api/credentials",
		map[string]string{"description": "share links"}); status != http.StatusCreated {
		t.Fatal("could not create a credential")
	}

	status, body = c.do(t, http.MethodPost, "/api/buckets/shared/share",
		map[string]any{"key": "doc.txt", "expiresSeconds": 600})
	if status != http.StatusOK {
		t.Fatalf("sharing returned %d: %v", status, body)
	}
	link, _ := body["url"].(string)
	if !strings.Contains(link, "X-Amz-Signature=") {
		t.Errorf("share link is not a presigned URL: %s", link)
	}
	if !strings.Contains(link, "X-Amz-Expires=600") {
		t.Errorf("share link does not carry the requested expiry: %s", link)
	}
}

func TestReadyzReportsDependencies(t *testing.T) {
	c := newConsole(t)

	response, err := c.client.Get(c.url + "/readyz")
	if err != nil {
		t.Fatalf("readyz: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("readyz returned %d", response.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode readyz: %v", err)
	}
	if body["ready"] != true {
		t.Errorf("ready = %v", body["ready"])
	}
	checks, _ := body["checks"].(map[string]any)
	if checks["database"] != "ok" || checks["storage"] != "ok" {
		t.Errorf("checks = %v", checks)
	}
}

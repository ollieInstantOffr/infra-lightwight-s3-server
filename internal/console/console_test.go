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
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("reset (%s): %v", stmt, err)
		}
	}

	admin, err := db.EnsureAdmin(ctx, pool, "admin@example.com")
	if err != nil {
		t.Fatalf("EnsureAdmin: %v", err)
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

// signIn performs the whole magic-link flow and leaves the client holding a
// session cookie.
func (c *consoleFixture) signIn(t *testing.T, email string) {
	t.Helper()

	status, _ := c.do(t, http.MethodPost, "/api/auth/magic-link", map[string]string{"email": email})
	if status != http.StatusOK {
		t.Fatalf("requesting a sign-in link returned %d", status)
	}

	link := extractLink(t, c.mailer.last(t).text)
	response, err := c.client.Get(link)
	if err != nil {
		t.Fatalf("follow sign-in link: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign-in link returned %d, want 303", response.StatusCode)
	}
	if location := response.Header.Get("Location"); strings.Contains(location, "error=") {
		t.Fatalf("sign-in failed: %s", location)
	}
}

// extractLink pulls the callback URL out of an email body.
func extractLink(t *testing.T, body string) string {
	t.Helper()
	for _, field := range strings.Fields(body) {
		if strings.Contains(field, "/api/auth/callback?token=") {
			return field
		}
	}
	t.Fatalf("no sign-in link found in the email:\n%s", body)
	return ""
}

func TestMagicLinkSignIn(t *testing.T) {
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

// A link is a bearer credential. Using one twice must fail, or an old email in
// a synced mailbox stays a way in forever.
func TestMagicLinkIsSingleUse(t *testing.T) {
	c := newConsole(t)

	status, _ := c.do(t, http.MethodPost, "/api/auth/magic-link", map[string]string{"email": "admin@example.com"})
	if status != http.StatusOK {
		t.Fatalf("requesting a link returned %d", status)
	}
	link := extractLink(t, c.mailer.last(t).text)

	first, err := c.client.Get(link)
	if err != nil {
		t.Fatalf("first use: %v", err)
	}
	first.Body.Close()
	if strings.Contains(first.Header.Get("Location"), "error=") {
		t.Fatalf("the first use of the link failed: %s", first.Header.Get("Location"))
	}

	second, err := c.client.Get(link)
	if err != nil {
		t.Fatalf("second use: %v", err)
	}
	defer second.Body.Close()
	if !strings.Contains(second.Header.Get("Location"), "error=expired") {
		t.Errorf("the link worked twice; Location = %q", second.Header.Get("Location"))
	}
}

// The endpoint is unauthenticated by necessity, so it must not reveal which
// addresses have accounts.
func TestMagicLinkDoesNotRevealWhoHasAnAccount(t *testing.T) {
	c := newConsole(t)

	knownStatus, knownBody := c.do(t, http.MethodPost, "/api/auth/magic-link",
		map[string]string{"email": "admin@example.com"})
	unknownStatus, unknownBody := c.do(t, http.MethodPost, "/api/auth/magic-link",
		map[string]string{"email": "stranger@example.com"})

	if knownStatus != unknownStatus {
		t.Errorf("status differs: known %d, unknown %d", knownStatus, unknownStatus)
	}
	if fmt.Sprint(knownBody) != fmt.Sprint(unknownBody) {
		t.Errorf("response differs:\n known: %v\n unknown: %v", knownBody, unknownBody)
	}
	// And crucially, no email was sent to the stranger.
	if c.mailer.count() != 1 {
		t.Errorf("%d emails sent, want 1 — a stranger was mailed", c.mailer.count())
	}
}

// Anyone can name any address here, so without a limit this is a way to flood
// someone's inbox.
func TestMagicLinkIsRateLimited(t *testing.T) {
	c := newConsole(t)

	for i := range magicLinkRateLimit + 3 {
		status, _ := c.do(t, http.MethodPost, "/api/auth/magic-link",
			map[string]string{"email": "admin@example.com"})
		if status != http.StatusOK {
			t.Fatalf("request %d returned %d", i, status)
		}
	}

	if sent := c.mailer.count(); sent > magicLinkRateLimit {
		t.Errorf("%d emails sent for %d requests; the limit of %d was not enforced",
			sent, magicLinkRateLimit+3, magicLinkRateLimit)
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

	status, _ := c.do(t, http.MethodPost, "/api/auth/magic-link", map[string]string{"email": "admin@example.com"})
	if status != http.StatusOK {
		t.Fatalf("requesting a link returned %d", status)
	}
	response, err := c.client.Get(extractLink(t, c.mailer.last(t).text))
	if err != nil {
		t.Fatalf("follow link: %v", err)
	}
	defer response.Body.Close()

	for _, cookie := range response.Cookies() {
		if cookie.Name != sessionCookieName {
			continue
		}
		if !cookie.HttpOnly {
			t.Error("the session cookie is not HttpOnly, so a script could read it")
		}
		// Lax rather than Strict, because the sign-in link is followed from an
		// email client and Strict would drop the cookie on that navigation.
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

func TestInviteFlow(t *testing.T) {
	c := newConsole(t)
	c.signIn(t, "admin@example.com")

	status, _ := c.do(t, http.MethodPost, "/api/users/invite",
		map[string]string{"email": "colleague@example.com", "role": db.RoleMember})
	if status != http.StatusCreated {
		t.Fatalf("inviting returned %d", status)
	}

	// The invited address can now sign in; the link goes through the same
	// callback, so accepting and signing in are one click.
	invited := newClientFor(t, c)
	invited.signIn(t, "colleague@example.com")

	status, body := invited.do(t, http.MethodGet, "/api/auth/me", nil)
	if status != http.StatusOK {
		t.Fatalf("the invited user could not read their own profile: %d", status)
	}
	if body["email"] != "colleague@example.com" {
		t.Errorf("signed in as %v", body["email"])
	}
	if body["isAdmin"] != false {
		t.Error("an invited member was made an admin")
	}
}

// An address with no account and no invitation must not be able to sign in.
func TestUninvitedAddressCannotSignIn(t *testing.T) {
	c := newConsole(t)

	status, _ := c.do(t, http.MethodPost, "/api/auth/magic-link",
		map[string]string{"email": "stranger@example.com"})
	if status != http.StatusOK {
		t.Fatalf("requesting a link returned %d", status)
	}
	if c.mailer.count() != 0 {
		t.Fatal("an email was sent to an address with no account and no invitation")
	}
}

// Members must not be able to perform admin actions.
func TestMembersCannotAdminister(t *testing.T) {
	c := newConsole(t)
	c.signIn(t, "admin@example.com")
	if status, _ := c.do(t, http.MethodPost, "/api/users/invite",
		map[string]string{"email": "member@example.com"}); status != http.StatusCreated {
		t.Fatalf("inviting returned %d", status)
	}

	member := newClientFor(t, c)
	member.signIn(t, "member@example.com")

	for _, call := range []struct {
		method, path string
		payload      any
	}{
		{http.MethodGet, "/api/users", nil},
		{http.MethodPost, "/api/users/invite", map[string]string{"email": "another@example.com"}},
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

func TestSignInEmailContainsAWorkingLink(t *testing.T) {
	c := newConsole(t)

	if status, _ := c.do(t, http.MethodPost, "/api/auth/magic-link",
		map[string]string{"email": "admin@example.com"}); status != http.StatusOK {
		t.Fatal("requesting a link failed")
	}

	message := c.mailer.last(t)
	if message.to != "admin@example.com" {
		t.Errorf("sent to %q", message.to)
	}
	if !strings.Contains(message.subject, "sign-in") && !strings.Contains(message.subject, "Sign-in") {
		t.Errorf("subject = %q", message.subject)
	}
	// A plain-text body matters: some clients strip HTML entirely, and a blank
	// login email is indistinguishable from one that never arrived.
	if !strings.Contains(message.text, "/api/auth/callback?token=") {
		t.Errorf("the plain-text body has no sign-in link:\n%s", message.text)
	}
	if !strings.Contains(message.text, "15 minutes") {
		t.Errorf("the email does not say how long the link lasts:\n%s", message.text)
	}
}

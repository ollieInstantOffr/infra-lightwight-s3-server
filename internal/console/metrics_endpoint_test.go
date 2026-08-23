package console

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// Metrics describe who is using this system and how much, so the endpoint is
// authenticated. These pin both ways in, and — more importantly — that the
// default with nothing configured is the closed one.

// scrape performs a GET /metrics with an optional bearer token, without the
// session cookie the fixture's client carries.
func scrape(t *testing.T, console *consoleFixture, token string) (int, string) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, console.url+"/metrics", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	// A bare client, so no session cookie comes along by accident.
	response, err := (&http.Client{}).Do(request)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(body)
}

func TestMetricsRefusesAnonymousScrapes(t *testing.T) {
	console := newConsole(t)

	status, body := scrape(t, console, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; an open metrics endpoint leaks bucket names and traffic", status)
	}
	// The most likely reader of this 401 is whoever is setting the scrape up.
	if !strings.Contains(body, "METRICS_TOKEN") {
		t.Errorf("the refusal does not say what to configure: %s", body)
	}
}

func TestMetricsRefusesAWrongToken(t *testing.T) {
	console := newConsole(t)
	console.server.MetricsToken = "the-real-token"

	if status, _ := scrape(t, console, "not-the-token"); status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for a wrong token", status)
	}
	// An empty bearer must not be treated as "no token supplied" and fall
	// through to the session path.
	if status, _ := scrape(t, console, ""); status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 with no credentials at all", status)
	}
}

func TestMetricsAcceptsTheConfiguredToken(t *testing.T) {
	console := newConsole(t)
	console.server.MetricsToken = "the-real-token"

	status, body := scrape(t, console, "the-real-token")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", status, body)
	}
	for _, want := range []string{"pail_requests_total", "pail_up_database", "pail_build_info"} {
		if !strings.Contains(body, want) {
			t.Errorf("the scrape is missing %s:\n%s", want, body)
		}
	}
}

func TestMetricsAcceptsASignedInAdministrator(t *testing.T) {
	// So an operator can read it from a browser without provisioning a token
	// just to look once.
	console := newConsole(t)
	console.signIn(t, "admin@example.com")

	response, err := console.client.Get(console.url + "/metrics")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a signed-in admin", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type = %q; scrapers parse by content type", got)
	}
}

func TestMetricsTokenDoesNotUnlockTheRestOfTheConsole(t *testing.T) {
	// The token authorises a scrape and nothing else. If it were accepted as a
	// general credential it would be an administrator password with none of
	// the handling one gets.
	console := newConsole(t)
	console.server.MetricsToken = "the-real-token"

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, console.url+"/api/credentials", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer the-real-token")
	response, err := (&http.Client{}).Do(request)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("the metrics token reached /api/credentials (status %d)", response.StatusCode)
	}
}

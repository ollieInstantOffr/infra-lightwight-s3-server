package config

import "testing"

// An empty ROLE must mean "everything", or an existing deployment upgrading to
// this version would come up serving nothing.
func TestEmptyRoleIsTheWholeServer(t *testing.T) {
	role, err := ParseRole("")
	if err != nil {
		t.Fatalf("ParseRole(\"\"): %v", err)
	}
	if role != RoleAll {
		t.Fatalf("ParseRole(\"\") = %q, want %q", role, RoleAll)
	}
	if !role.ServesS3() || !role.ServesConsole() || !role.RunsWorkers() {
		t.Error("the default role does not run everything, so an upgrade would silently stop serving")
	}
}

func TestParseRole(t *testing.T) {
	cases := map[string]struct {
		raw     string
		want    Role
		wantErr bool
	}{
		"s3":                  {raw: "s3", want: RoleS3},
		"console":             {raw: "console", want: RoleConsole},
		"worker":              {raw: "worker", want: RoleWorker},
		"all":                 {raw: "all", want: RoleAll},
		"uppercase":           {raw: "S3", want: RoleS3},
		"surrounded by space": {raw: "  worker  ", want: RoleWorker},
		"unknown":             {raw: "database", wantErr: true},
		"nearly right":        {raw: "workers", wantErr: true},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ParseRole(c.raw)
			if c.wantErr {
				if err == nil {
					t.Fatalf("ParseRole(%q) = %q, want an error", c.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRole(%q): %v", c.raw, err)
			}
			if got != c.want {
				t.Errorf("ParseRole(%q) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}

// A typo must name the alternatives. "unknown ROLE" on its own leaves an
// operator guessing at a closed set of four.
func TestAnUnknownRoleNamesTheValidOnes(t *testing.T) {
	_, err := ParseRole("s3api")
	if err == nil {
		t.Fatal("ParseRole accepted an unknown role")
	}
	for _, name := range []string{"all", "console", "s3", "worker"} {
		if !contains(err.Error(), name) {
			t.Errorf("the error does not mention %q: %v", name, err)
		}
	}
}

// The table that decides what each container starts. Getting one cell wrong
// means a service that silently does not do its job, so it is asserted whole
// rather than trusted to the three one-line methods.
func TestWhatEachRoleRuns(t *testing.T) {
	cases := []struct {
		role                          Role
		s3, console, workers, credKey bool
	}{
		{RoleAll, true, true, true, true},
		{RoleS3, true, false, false, true},
		{RoleConsole, false, true, false, true},
		// The worker listens on nothing but still needs the credentials key:
		// it sends alert notifications, and the Resend key is encrypted with
		// the same cipher.
		{RoleWorker, false, false, true, true},
	}

	for _, c := range cases {
		t.Run(string(c.role), func(t *testing.T) {
			if got := c.role.ServesS3(); got != c.s3 {
				t.Errorf("ServesS3() = %v, want %v", got, c.s3)
			}
			if got := c.role.ServesConsole(); got != c.console {
				t.Errorf("ServesConsole() = %v, want %v", got, c.console)
			}
			if got := c.role.RunsWorkers(); got != c.workers {
				t.Errorf("RunsWorkers() = %v, want %v", got, c.workers)
			}
			if got := c.role.NeedsCredentialsKey(); got != c.credKey {
				t.Errorf("NeedsCredentialsKey() = %v, want %v", got, c.credKey)
			}
		})
	}
}

// Every role must run somewhere, or a piece of the server would exist in no
// topology at all.
func TestEveryPieceOfTheServerRunsInSomeRole(t *testing.T) {
	var s3, console, workers bool
	for _, role := range []Role{RoleS3, RoleConsole, RoleWorker} {
		s3 = s3 || role.ServesS3()
		console = console || role.ServesConsole()
		workers = workers || role.RunsWorkers()
	}
	if !s3 || !console || !workers {
		t.Errorf("the split roles do not cover the whole server: s3=%v console=%v workers=%v",
			s3, console, workers)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

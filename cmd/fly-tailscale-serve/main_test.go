package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
	"time"
)

func validCLI() cli {
	return cli{
		AppCommand:                "echo hello",
		Port:                      8080,
		StartupTimeout:            30 * time.Second,
		ShutdownGrace:             3 * time.Second,
		WatchdogInterval:          1 * time.Minute,
		WatchdogMaxFailures:       3,
		AdvertiseTags:             []string{"tag:test"},
		ServiceName:               "svc:my-service",
		OIDCClientID:              "client-id",
		OIDCAudience:              "tailscale.com",
		Hostname:                  "test-host",
		LitestreamBinary:          "litestream",
		LitestreamStartupTimeout:  30 * time.Second,
		LitestreamShutdownTimeout: 15 * time.Second,
		LitestreamAWSAudience:     "sts.amazonaws.com",
		LitestreamAWSTokenFile:    "/run/fly-tailscale-serve/aws-web-identity-token",
		LitestreamAWSRefresh:      2 * time.Minute,
	}
}

func TestValidate(t *testing.T) {
	t.Run("valid config", func(t *testing.T) {
		c := validCLI()
		if err := c.validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("empty app command", func(t *testing.T) {
		c := validCLI()
		c.AppCommand = ""
		assertValidateError(t, c, "APP_COMMAND must contain at least one argument")
	})

	t.Run("whitespace-only app command", func(t *testing.T) {
		c := validCLI()
		c.AppCommand = "   "
		assertValidateError(t, c, "APP_COMMAND must contain at least one argument")
	})

	t.Run("port too low", func(t *testing.T) {
		c := validCLI()
		c.Port = 0
		assertValidateError(t, c, "PORT must be between 1 and 65535")
	})

	t.Run("port too high", func(t *testing.T) {
		c := validCLI()
		c.Port = 65536
		assertValidateError(t, c, "PORT must be between 1 and 65535")
	})

	t.Run("negative startup timeout", func(t *testing.T) {
		c := validCLI()
		c.StartupTimeout = -1
		assertValidateError(t, c, "STARTUP_TIMEOUT must be greater than 0")
	})

	t.Run("negative shutdown grace", func(t *testing.T) {
		c := validCLI()
		c.ShutdownGrace = -1
		assertValidateError(t, c, "SHUTDOWN_GRACE must be greater than 0")
	})

	t.Run("negative watchdog interval", func(t *testing.T) {
		c := validCLI()
		c.WatchdogInterval = -1
		assertValidateError(t, c, "WATCHDOG_INTERVAL must be greater than 0")
	})

	t.Run("zero watchdog max failures", func(t *testing.T) {
		c := validCLI()
		c.WatchdogMaxFailures = 0
		assertValidateError(t, c, "WATCHDOG_MAX_FAILURES must be at least 1")
	})

	t.Run("empty advertise tags", func(t *testing.T) {
		c := validCLI()
		c.AdvertiseTags = []string{}
		assertValidateError(t, c, "TAILSCALE_ADVERTISE_TAGS must contain at least one tag")
	})

	t.Run("whitespace-only advertise tags", func(t *testing.T) {
		c := validCLI()
		c.AdvertiseTags = []string{"  ", ""}
		assertValidateError(t, c, "TAILSCALE_ADVERTISE_TAGS must contain at least one tag")
	})

	t.Run("service name without svc prefix", func(t *testing.T) {
		c := validCLI()
		c.ServiceName = "my-service"
		assertValidateError(t, c, "TAILSCALE_SERVICE_NAME must start with")
	})

	t.Run("normalizes app command whitespace", func(t *testing.T) {
		c := validCLI()
		c.AppCommand = "  bun   run   index.tsx  "
		if err := c.validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if c.AppCommand != "bun run index.tsx" {
			t.Errorf("AppCommand = %q, want %q", c.AppCommand, "bun run index.tsx")
		}
	})

	t.Run("trims advertise tags", func(t *testing.T) {
		c := validCLI()
		c.AdvertiseTags = []string{" tag:a ", "", " tag:b "}
		if err := c.validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(c.AdvertiseTags) != 2 || c.AdvertiseTags[0] != "tag:a" || c.AdvertiseTags[1] != "tag:b" {
			t.Errorf("AdvertiseTags = %v, want [tag:a tag:b]", c.AdvertiseTags)
		}
	})
}

func TestResolveHostname(t *testing.T) {
	t.Run("explicit hostname", func(t *testing.T) {
		c := cli{Hostname: "my-host"}
		got, err := c.resolveHostname()
		assertNoError(t, err)
		assertEqual(t, got, "my-host")
	})

	t.Run("trims whitespace from explicit hostname", func(t *testing.T) {
		c := cli{Hostname: "  my-host  "}
		got, err := c.resolveHostname()
		assertNoError(t, err)
		assertEqual(t, got, "my-host")
	})

	t.Run("falls back to FLY_APP_NAME", func(t *testing.T) {
		c := cli{FlyAppName: "my-app"}
		got, err := c.resolveHostname()
		assertNoError(t, err)
		assertEqual(t, got, "my-app")
	})

	t.Run("combines app name and machine ID", func(t *testing.T) {
		c := cli{FlyAppName: "my-app", FlyMachineID: "abc123"}
		got, err := c.resolveHostname()
		assertNoError(t, err)
		assertEqual(t, got, "my-app-abc123")
	})

	t.Run("errors when no hostname and no FLY_APP_NAME", func(t *testing.T) {
		c := cli{}
		_, err := c.resolveHostname()
		assertErrorContains(t, err, "TAILSCALE_HOSTNAME must be set")
	})

	t.Run("whitespace-only FLY_APP_NAME treated as empty", func(t *testing.T) {
		c := cli{FlyAppName: "   "}
		_, err := c.resolveHostname()
		assertErrorContains(t, err, "TAILSCALE_HOSTNAME must be set")
	})

	t.Run("whitespace-only machine ID ignored", func(t *testing.T) {
		c := cli{FlyAppName: "my-app", FlyMachineID: "   "}
		got, err := c.resolveHostname()
		assertNoError(t, err)
		assertEqual(t, got, "my-app")
	})
}

func TestAppendPersistentWorkloadFlags(t *testing.T) {
	t.Run("simple client ID", func(t *testing.T) {
		got := appendPersistentWorkloadFlags("my-client-id")
		assertEqual(t, got, "my-client-id?ephemeral=false&preauthorized=true")
	})

	t.Run("client ID with existing query params", func(t *testing.T) {
		got := appendPersistentWorkloadFlags("my-client-id?foo=bar")
		assertEqual(t, got, "my-client-id?foo=bar&ephemeral=false&preauthorized=true")
	})
}

func TestNormalizeTags(t *testing.T) {
	t.Run("trims whitespace", func(t *testing.T) {
		got := normalizeTags([]string{" tag:a ", " tag:b "})
		assertSliceEqual(t, got, []string{"tag:a", "tag:b"})
	})

	t.Run("removes empty strings", func(t *testing.T) {
		got := normalizeTags([]string{"tag:a", "", "tag:b", "  "})
		assertSliceEqual(t, got, []string{"tag:a", "tag:b"})
	})

	t.Run("nil input", func(t *testing.T) {
		got := normalizeTags(nil)
		if len(got) != 0 {
			t.Errorf("got %v, want empty", got)
		}
	})
}

func TestAppURL(t *testing.T) {
	c := cli{Port: 3000}
	assertEqual(t, c.appURL(), "http://127.0.0.1:3000")
}

func TestAppCommandArgs(t *testing.T) {
	t.Run("splits on whitespace", func(t *testing.T) {
		c := cli{AppCommand: "bun run index.tsx"}
		assertSliceEqual(t, c.appCommandArgs(), []string{"bun", "run", "index.tsx"})
	})

	t.Run("collapses multiple spaces", func(t *testing.T) {
		c := cli{AppCommand: "  bun   run   index.tsx  "}
		assertSliceEqual(t, c.appCommandArgs(), []string{"bun", "run", "index.tsx"})
	})

	t.Run("empty command", func(t *testing.T) {
		c := cli{AppCommand: ""}
		if len(c.appCommandArgs()) != 0 {
			t.Errorf("expected empty args for empty command")
		}
	})
}

func TestTailscaleClientID(t *testing.T) {
	c := cli{OIDCClientID: "my-id"}
	assertEqual(t, c.tailscaleClientID(), "my-id?ephemeral=false&preauthorized=true")
}

func TestProxyPreservesForwardedHeaders(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Got-Forwarded-For", r.Header.Get("X-Forwarded-For"))
		w.Header().Set("X-Got-Forwarded-Proto", r.Header.Get("X-Forwarded-Proto"))
		w.Header().Set("X-Got-Host", r.Host)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(backendURL)
			pr.Out.Host = pr.In.Host

			for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Proto"} {
				if v := pr.In.Header.Get(h); v != "" {
					pr.Out.Header.Set(h, v)
				}
			}
		},
	}

	t.Run("preserves X-Forwarded-For from upstream", func(t *testing.T) {
		req := httptest.NewRequest("GET", "http://my-service.ts.net/", nil)
		req.Header.Set("X-Forwarded-For", "100.64.1.1")
		req.Header.Set("X-Forwarded-Proto", "https")

		rr := httptest.NewRecorder()
		proxy.ServeHTTP(rr, req)

		assertEqual(t, rr.Header().Get("X-Got-Forwarded-For"), "100.64.1.1")
		assertEqual(t, rr.Header().Get("X-Got-Forwarded-Proto"), "https")
	})

	t.Run("preserves original host", func(t *testing.T) {
		req := httptest.NewRequest("GET", "http://my-service.ts.net/", nil)
		req.Header.Set("X-Forwarded-For", "100.64.1.1")

		rr := httptest.NewRecorder()
		proxy.ServeHTTP(rr, req)

		assertEqual(t, rr.Header().Get("X-Got-Host"), "my-service.ts.net")
	})

	t.Run("no forwarded headers when upstream omits them", func(t *testing.T) {
		req := httptest.NewRequest("GET", "http://my-service.ts.net/", nil)

		rr := httptest.NewRecorder()
		proxy.ServeHTTP(rr, req)

		assertEqual(t, rr.Header().Get("X-Got-Forwarded-For"), "")
	})
}

func TestExitCode(t *testing.T) {
	t.Run("returns 1 for non-ExitError", func(t *testing.T) {
		if got := exitCode(io.EOF); got != 1 {
			t.Errorf("exitCode(io.EOF) = %d, want 1", got)
		}
	})
}

func TestExitCodeError(t *testing.T) {
	t.Run("Error returns message", func(t *testing.T) {
		e := &exitCodeError{code: 42, err: io.EOF}
		assertEqual(t, e.Error(), "EOF")
	})

	t.Run("Unwrap returns inner error", func(t *testing.T) {
		inner := io.EOF
		e := &exitCodeError{code: 42, err: inner}
		if e.Unwrap() != inner {
			t.Errorf("Unwrap() = %v, want %v", e.Unwrap(), inner)
		}
	})
}

func TestProcessHandleWaitNil(t *testing.T) {
	var p *processHandle
	if err := p.Wait(); err != nil {
		t.Errorf("Wait on nil handle should return nil, got %v", err)
	}
}

func TestProcessHandleTerminateNil(t *testing.T) {
	var p *processHandle
	if err := p.Terminate(time.Second); err != nil {
		t.Errorf("Terminate on nil handle should return nil, got %v", err)
	}
}

func TestWaitForAppHealthFailures(t *testing.T) {
	t.Run("app exits early", func(t *testing.T) {
		c := validCLI()
		c.StartupTimeout = time.Second
		s := newService(&c)
		done := make(chan error, 1)
		done <- io.EOF
		err := s.waitForAppHealth(context.Background(), done)
		assertErrorContains(t, err, "app process exited before becoming reachable")
	})

	t.Run("health deadline", func(t *testing.T) {
		c := validCLI()
		c.StartupTimeout = time.Millisecond
		s := newService(&c)
		err := s.waitForAppHealth(context.Background(), make(chan error))
		assertErrorContains(t, err, "app did not become reachable")
	})
}

// test helpers

func assertNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func assertEqual(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func assertSliceEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v (len %d), want %v (len %d)", got, len(got), want, len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("index %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func assertErrorContains(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", substr)
	}
	if got := err.Error(); !contains(got, substr) {
		t.Errorf("error %q does not contain %q", got, substr)
	}
}

func assertValidateError(t *testing.T, c cli, substr string) {
	t.Helper()
	err := c.validate()
	if err == nil {
		t.Fatalf("expected validation error containing %q, got nil", substr)
	}
	if got := err.Error(); !contains(got, substr) {
		t.Errorf("error %q does not contain %q", got, substr)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

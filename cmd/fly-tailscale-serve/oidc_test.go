package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func testJWT(expires time.Time, marker string) string {
	payload, _ := json.Marshal(map[string]any{"exp": expires.Unix(), "marker": marker})
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestOIDCTokenManagerInitializesRefreshesAndCleansUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "token")
	var calls atomic.Int32
	m := newOIDCTokenManager(path, 1500*time.Millisecond, func(context.Context) (string, error) {
		n := calls.Add(1)
		return testJWT(time.Now().Add(2*time.Second), fmt.Sprint(n)), nil
	})
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token mode = %o, want 600", info.Mode().Perm())
	}
	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if calls.Load() < 2 {
		t.Fatal("token was not refreshed")
	}
	m.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("token file remains after close: %v", err)
	}
}

func TestOIDCTokenManagerFailures(t *testing.T) {
	t.Run("initial acquisition", func(t *testing.T) {
		m := newOIDCTokenManager(filepath.Join(t.TempDir(), "token"), time.Second,
			func(context.Context) (string, error) { return "", fmt.Errorf("provider unavailable") })
		if err := m.Start(context.Background()); err == nil {
			t.Fatal("expected acquisition failure")
		}
		m.Close()
	})

	t.Run("malformed token", func(t *testing.T) {
		m := newOIDCTokenManager(filepath.Join(t.TempDir(), "token"), time.Second,
			func(context.Context) (string, error) { return "not-a-jwt", nil })
		if err := m.Start(context.Background()); err == nil {
			t.Fatal("expected malformed JWT failure")
		}
		m.Close()
	})

	t.Run("refresh failure becomes fatal at expiry", func(t *testing.T) {
		var calls atomic.Int32
		m := newOIDCTokenManager(filepath.Join(t.TempDir(), "token"), 1500*time.Millisecond,
			func(context.Context) (string, error) {
				if calls.Add(1) == 1 {
					return testJWT(time.Now().Add(2*time.Second), "initial"), nil
				}
				return "", fmt.Errorf("provider unavailable")
			})
		if err := m.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		defer m.Close()
		select {
		case err := <-m.Errors():
			if err == nil {
				t.Fatal("expected refresh error")
			}
		case <-time.After(4 * time.Second):
			t.Fatal("refresh failure did not become fatal")
		}
	})
}

func TestJWTExpiry(t *testing.T) {
	want := time.Now().Add(time.Hour).Truncate(time.Second)
	got, err := jwtExpiry(testJWT(want, "test"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		t.Fatalf("expiry = %v, want %v", got, want)
	}
}

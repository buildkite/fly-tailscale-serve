package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func litestreamCLI(t *testing.T) cli {
	t.Helper()
	c := validCLI()
	dir := t.TempDir()
	c.LitestreamConfig = filepath.Join(dir, "litestream.yml")
	c.LitestreamDB = filepath.Join(dir, "app.db")
	c.LitestreamSocket = filepath.Join(dir, "litestream.sock")
	c.LitestreamStartupTimeout = 2 * time.Second
	c.LitestreamShutdownTimeout = 2 * time.Second
	return c
}

func TestValidateLitestream(t *testing.T) {
	t.Run("disabled compatibility", func(t *testing.T) {
		c := validCLI()
		if err := c.validate(); err != nil {
			t.Fatal(err)
		}
		if c.litestreamEnabled() {
			t.Fatal("Litestream unexpectedly enabled")
		}
	})
	t.Run("requires group", func(t *testing.T) {
		c := validCLI()
		c.LitestreamConfig = "/etc/litestream.yml"
		assertValidateError(t, c, "must be set together")
	})
	t.Run("requires absolute paths", func(t *testing.T) {
		c := validCLI()
		c.LitestreamConfig, c.LitestreamDB, c.LitestreamSocket = "config", "/data/db", "/run/socket"
		assertValidateError(t, c, "LITESTREAM_CONFIG must be an absolute path")
	})
	t.Run("OIDC requires Litestream", func(t *testing.T) {
		c := validCLI()
		c.LitestreamAWSRoleARN = "arn:aws:iam::123:role/test"
		assertValidateError(t, c, "require LITESTREAM_CONFIG")
	})
}

func TestLitestreamStartupWithFakeProcess(t *testing.T) {
	t.Run("fresh replica waits for app-era sync", func(t *testing.T) {
		c := litestreamCLI(t)
		logPath := filepath.Join(t.TempDir(), "calls")
		c.LitestreamBinary = writeFakeLitestream(t, logPath)
		s := newService(&c)
		if err := s.startLitestream(context.Background()); err != nil {
			t.Fatal(err)
		}
		defer s.shutdown()
		if !s.litestreamFreshDB {
			t.Fatal("expected empty replica to remain a fresh DB")
		}
		calls := readCalls(t, logPath)
		if strings.Contains(calls, "sync ") {
			t.Fatalf("synced before fresh DB creation:\n%s", calls)
		}
		if err := s.syncLitestream(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(readCalls(t, logPath), "sync -socket") {
			t.Fatal("missing post-creation sync")
		}
	})

	t.Run("existing database syncs before return", func(t *testing.T) {
		c := litestreamCLI(t)
		if err := os.WriteFile(c.LitestreamDB, []byte("sqlite"), 0o600); err != nil {
			t.Fatal(err)
		}
		logPath := filepath.Join(t.TempDir(), "calls")
		c.LitestreamBinary = writeFakeLitestream(t, logPath)
		s := newService(&c)
		if err := s.startLitestream(context.Background()); err != nil {
			t.Fatal(err)
		}
		defer s.shutdown()
		if s.litestreamFreshDB {
			t.Fatal("existing DB marked fresh")
		}
		if !strings.Contains(readCalls(t, logPath), "sync -socket") {
			t.Fatal("existing DB was not synced before startup")
		}
	})

	t.Run("socket path never removes a database", func(t *testing.T) {
		c := litestreamCLI(t)
		c.LitestreamSocket = c.LitestreamDB
		if err := os.WriteFile(c.LitestreamDB, []byte("sqlite"), 0o600); err != nil {
			t.Fatal(err)
		}
		c.LitestreamBinary = writeFakeLitestream(t, filepath.Join(t.TempDir(), "calls"))
		s := newService(&c)
		if err := s.startLitestream(context.Background()); err != nil {
			t.Fatal(err)
		}
		defer s.shutdown()
		got, err := os.ReadFile(c.LitestreamDB)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "sqlite" {
			t.Fatalf("database contents = %q, want sqlite", got)
		}
	})

	t.Run("restore failure", func(t *testing.T) {
		c := litestreamCLI(t)
		c.LitestreamBinary = writeScript(t, "exit 23")
		s := newService(&c)
		if err := s.startLitestream(context.Background()); err == nil || !strings.Contains(err.Error(), "restore") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("restore timeout", func(t *testing.T) {
		c := litestreamCLI(t)
		c.LitestreamStartupTimeout = 100 * time.Millisecond
		c.LitestreamBinary = writeScript(t, "sleep 5")
		s := newService(&c)
		started := time.Now()
		if err := s.startLitestream(context.Background()); err == nil || !strings.Contains(err.Error(), "restore") {
			t.Fatalf("error = %v", err)
		}
		if time.Since(started) > time.Second {
			t.Fatal("restore exceeded startup timeout")
		}
	})

	t.Run("signal during restore", func(t *testing.T) {
		c := litestreamCLI(t)
		c.LitestreamBinary = writeScript(t, "exec sleep 5")
		s := newService(&c)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(50*time.Millisecond, cancel)
		started := time.Now()
		err := s.startLitestream(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context cancellation", err)
		}
		if time.Since(started) > time.Second {
			t.Fatal("restore cancellation was not prompt")
		}
	})

	t.Run("initial sync failure", func(t *testing.T) {
		c := litestreamCLI(t)
		if err := os.WriteFile(c.LitestreamDB, []byte("sqlite"), 0o600); err != nil {
			t.Fatal(err)
		}
		c.LitestreamBinary = writeScript(t, `case "$1" in restore|info) exit 0;; sync) exit 12;; replicate) trap 'exit 0' TERM; while :; do sleep 1; done;; esac`)
		s := newService(&c)
		defer s.shutdown()
		if err := s.startLitestream(context.Background()); err == nil || !strings.Contains(err.Error(), "initial Litestream sync") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("readiness failure", func(t *testing.T) {
		c := litestreamCLI(t)
		c.LitestreamStartupTimeout = 200 * time.Millisecond
		c.LitestreamBinary = writeScript(t, `case "$1" in restore) exit 0;; replicate) trap 'exit 0' TERM; while :; do sleep 1; done;; info) exit 1;; esac`)
		s := newService(&c)
		defer s.shutdown()
		if err := s.startLitestream(context.Background()); err == nil || !strings.Contains(err.Error(), "control socket") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("replicator exits during readiness", func(t *testing.T) {
		c := litestreamCLI(t)
		c.LitestreamBinary = writeScript(t, `case "$1" in restore) exit 0;; replicate) exit 9;; info) exit 1;; esac`)
		s := newService(&c)
		defer s.shutdown()
		if err := s.startLitestream(context.Background()); err == nil || !strings.Contains(err.Error(), "exited before") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestShutdownOrdersAppSyncAndLitestream(t *testing.T) {
	c := litestreamCLI(t)
	logPath := filepath.Join(t.TempDir(), "calls")
	c.LitestreamBinary = writeFakeLitestream(t, logPath)
	s := newService(&c)
	ls, err := startProcess(s.litestreamCommand("replicate"))
	if err != nil {
		t.Fatal(err)
	}
	s.litestream = ls
	appScript := writeScript(t, `trap 'echo app-term >> "$EVENT_LOG"; exit 0' TERM; while :; do sleep 1; done`)
	appCmd := exec.Command(appScript)
	appCmd.Env = append(os.Environ(), "EVENT_LOG="+logPath)
	s.app, err = startProcess(appCmd)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(25 * time.Millisecond)
	s.shutdown()
	calls := readCalls(t, logPath)
	appAt, syncAt, lsAt := strings.Index(calls, "app-term"), strings.Index(calls, "sync "), strings.Index(calls, "replicate-term")
	if appAt < 0 || syncAt <= appAt || lsAt <= syncAt {
		t.Fatalf("wrong shutdown order:\n%s", calls)
	}
}

func TestProcessTerminateRacesWaitAndEscalates(t *testing.T) {
	cmd := exec.Command(writeScript(t, `trap '' TERM; while :; do sleep 1; done`))
	p, err := startProcess(cmd)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.Wait() }()
	started := time.Now()
	_ = p.Terminate(100 * time.Millisecond)
	if time.Since(started) > time.Second {
		t.Fatal("termination exceeded timeout")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("concurrent Wait did not return")
	}
}

func TestWaitForTermination(t *testing.T) {
	t.Run("app status is preserved", func(t *testing.T) {
		appDone := make(chan error, 1)
		appDone <- &exec.ExitError{ProcessState: exitedProcessState(t, 42)}
		err := waitForTermination(context.Background(), nil, nil, appDone, nil, nil)
		var exitErr *exitCodeError
		if !errors.As(err, &exitErr) || exitErr.code != 42 {
			t.Fatalf("error = %#v, want exit 42", err)
		}
	})

	t.Run("Litestream exit is fatal", func(t *testing.T) {
		done := make(chan error, 1)
		done <- errors.New("replica failed")
		err := waitForTermination(context.Background(), nil, nil, make(chan error), done, nil)
		var exitErr *exitCodeError
		if !errors.As(err, &exitErr) || exitErr.code != 1 || !strings.Contains(err.Error(), "replica failed") {
			t.Fatalf("error = %#v, want fatal Litestream exit", err)
		}
	})

	t.Run("signal wins cleanly", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := waitForTermination(ctx, nil, nil, make(chan error), nil, nil); err != nil {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestContextCommandError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := contextCommandError(ctx, errors.New("signal: killed")); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	want := errors.New("command failed")
	if got := contextCommandError(context.Background(), want); got != want {
		t.Fatalf("error = %v, want %v", got, want)
	}
}

func exitedProcessState(t *testing.T, code int) *os.ProcessState {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit "+strconv.Itoa(code))
	_ = cmd.Run()
	return cmd.ProcessState
}

func writeFakeLitestream(t *testing.T, logPath string) string {
	t.Helper()
	return writeScript(t, `echo "$*" >> "`+logPath+`"
case "$1" in
  restore|info|sync) exit 0 ;;
  replicate) trap 'echo replicate-term >> "`+logPath+`"; exit 0' TERM; while :; do sleep 1; done ;;
esac`)
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func readCalls(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

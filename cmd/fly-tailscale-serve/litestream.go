package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (c *cli) litestreamEnabled() bool { return strings.TrimSpace(c.LitestreamConfig) != "" }

func (c *cli) validateLitestream() error {
	configured := []bool{c.LitestreamConfig != "", c.LitestreamDB != "", c.LitestreamSocket != ""}
	if configured[0] != configured[1] || configured[1] != configured[2] {
		return errors.New("LITESTREAM_CONFIG, LITESTREAM_DB, and LITESTREAM_SOCKET must be set together")
	}
	if !c.litestreamEnabled() {
		if c.LitestreamAWSRoleARN != "" || c.LitestreamAWSSessionName != "" {
			return errors.New("litestream AWS OIDC settings require LITESTREAM_CONFIG")
		}
		return nil
	}
	if c.LitestreamAWSRoleARN == "" && c.LitestreamAWSSessionName != "" {
		return errors.New("LITESTREAM_AWS_ROLE_SESSION_NAME requires LITESTREAM_AWS_ROLE_ARN")
	}
	for name, value := range map[string]string{
		"LITESTREAM_CONFIG": c.LitestreamConfig,
		"LITESTREAM_DB":     c.LitestreamDB,
		"LITESTREAM_SOCKET": c.LitestreamSocket,
	} {
		if !filepath.IsAbs(value) {
			return fmt.Errorf("%s must be an absolute path", name)
		}
	}
	if c.LitestreamStartupTimeout <= 0 {
		return errors.New("LITESTREAM_STARTUP_TIMEOUT must be greater than 0")
	}
	if c.LitestreamShutdownTimeout <= 0 {
		return errors.New("LITESTREAM_SHUTDOWN_TIMEOUT must be greater than 0")
	}
	if strings.TrimSpace(c.LitestreamBinary) == "" {
		return errors.New("LITESTREAM_BINARY must not be empty")
	}
	if c.LitestreamAWSRoleARN != "" {
		if c.LitestreamAWSAudience == "" {
			return errors.New("LITESTREAM_AWS_OIDC_AUDIENCE must not be empty")
		}
		if !filepath.IsAbs(c.LitestreamAWSTokenFile) {
			return errors.New("LITESTREAM_AWS_WEB_IDENTITY_TOKEN_FILE must be an absolute path")
		}
		if c.LitestreamAWSRefresh <= 0 {
			return errors.New("LITESTREAM_AWS_OIDC_REFRESH_BEFORE must be greater than 0")
		}
	}
	return nil
}

func (s *service) startLitestream(ctx context.Context) error {
	startupCtx, cancel := context.WithTimeout(ctx, s.cli.LitestreamStartupTimeout)
	defer cancel()

	if _, err := exec.LookPath(s.cli.LitestreamBinary); err != nil {
		return fmt.Errorf("find Litestream binary: %w", err)
	}
	if s.cli.LitestreamAWSRoleARN != "" {
		s.oidc = newOIDCTokenManager(s.cli.LitestreamAWSTokenFile, s.cli.LitestreamAWSRefresh,
			func(ctx context.Context) (string, error) {
				return s.requestIdentityTokenForAudience(ctx, s.cli.LitestreamAWSAudience)
			})
		if err := s.oidc.Start(ctx); err != nil {
			return fmt.Errorf("initialize Litestream AWS identity: %w", err)
		}
		s.litestreamEnv = append(s.litestreamEnv,
			"AWS_ROLE_ARN="+s.cli.LitestreamAWSRoleARN,
			"AWS_WEB_IDENTITY_TOKEN_FILE="+s.cli.LitestreamAWSTokenFile)
		if s.cli.LitestreamAWSSessionName != "" {
			s.litestreamEnv = append(s.litestreamEnv, "AWS_ROLE_SESSION_NAME="+s.cli.LitestreamAWSSessionName)
		}
	}

	log.Printf("Restoring %s from Litestream if needed...", s.cli.LitestreamDB)
	if err := s.runLitestreamCommand(startupCtx, "restore", "-config", s.cli.LitestreamConfig,
		"-if-db-not-exists", "-if-replica-exists", s.cli.LitestreamDB); err != nil {
		return fmt.Errorf("litestream restore: %w", err)
	}
	if _, err := os.Stat(s.cli.LitestreamDB); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("stat Litestream database: %w", err)
		}
		s.litestreamFreshDB = true
	}

	cmd := s.litestreamCommand("replicate", "-config", s.cli.LitestreamConfig)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	h, err := startProcess(cmd)
	if err != nil {
		return fmt.Errorf("start Litestream replication: %w", err)
	}
	s.litestream = h
	s.litestreamDone = make(chan error, 1)
	go func() { s.litestreamDone <- h.Wait() }()

	for {
		checkCtx, checkCancel := context.WithTimeout(startupCtx, time.Second)
		err := s.runLitestreamCommand(checkCtx, "info", "-socket", s.cli.LitestreamSocket, "-timeout", "1", "-json")
		checkCancel()
		if err == nil {
			if !s.litestreamFreshDB {
				if err := s.syncLitestream(startupCtx); err != nil {
					return fmt.Errorf("initial Litestream sync: %w", err)
				}
			}
			return nil
		}
		select {
		case err := <-s.litestreamDone:
			if err == nil {
				err = errors.New("process exited successfully")
			}
			return fmt.Errorf("litestream exited before control socket readiness: %w", err)
		case err := <-s.oidcErrors():
			return fmt.Errorf("litestream AWS identity refresh failed: %w", err)
		case <-startupCtx.Done():
			return fmt.Errorf("wait for Litestream control socket: %w", startupCtx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (s *service) syncLitestream(ctx context.Context) error {
	if s.litestream == nil {
		return nil
	}
	seconds := int(time.Until(time.Now().Add(s.cli.LitestreamStartupTimeout)).Seconds())
	if deadline, ok := ctx.Deadline(); ok {
		seconds = int(time.Until(deadline).Seconds())
	}
	if seconds < 1 {
		seconds = 1
	}
	return s.runLitestreamCommand(ctx, "sync", "-socket", s.cli.LitestreamSocket, "-wait", "-timeout", strconv.Itoa(seconds), "-json", s.cli.LitestreamDB)
}

func (s *service) litestreamCommand(args ...string) *exec.Cmd {
	cmd := exec.Command(s.cli.LitestreamBinary, args...)
	cmd.Env = append(os.Environ(), s.litestreamEnv...)
	return cmd
}

func (s *service) runLitestreamCommand(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, s.cli.LitestreamBinary, args...)
	cmd.Env = append(os.Environ(), s.litestreamEnv...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func (s *service) oidcErrors() <-chan error {
	if s.oidc == nil {
		return nil
	}
	return s.oidc.Errors()
}

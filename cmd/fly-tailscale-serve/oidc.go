package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type oidcTokenManager struct {
	path          string
	refreshBefore time.Duration
	fetch         func(context.Context) (string, error)
	cancel        context.CancelFunc
	done          chan struct{}
	errors        chan error
	closeOnce     sync.Once
}

func newOIDCTokenManager(path string, refreshBefore time.Duration, fetch func(context.Context) (string, error)) *oidcTokenManager {
	return &oidcTokenManager{path: path, refreshBefore: refreshBefore, fetch: fetch, done: make(chan struct{}), errors: make(chan error, 1)}
}

func (m *oidcTokenManager) Start(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	m.cancel = cancel
	expires, err := m.refresh(ctx)
	if err != nil {
		cancel()
		close(m.done)
		return err
	}
	go m.loop(ctx, expires)
	return nil
}

func (m *oidcTokenManager) loop(ctx context.Context, expires time.Time) {
	defer close(m.done)
	for {
		delay := time.Until(expires.Add(-m.refreshBefore))
		if lifetime := time.Until(expires); delay <= 0 {
			delay = lifetime / 2
		}
		if delay < time.Second {
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		for {
			newExpires, err := m.refresh(ctx)
			if err == nil {
				expires = newExpires
				break
			}
			if time.Until(expires) <= 0 {
				select {
				case m.errors <- err:
				default:
				}
				return
			}
			retry := minDuration(30*time.Second, time.Until(expires)/2)
			if retry < time.Second {
				retry = time.Second
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(retry):
			}
		}
	}
}

func (m *oidcTokenManager) refresh(ctx context.Context) (time.Time, error) {
	token, err := m.fetch(ctx)
	if err != nil {
		return time.Time{}, err
	}
	expires, err := jwtExpiry(token)
	if err != nil {
		return time.Time{}, err
	}
	if !expires.After(time.Now()) {
		return time.Time{}, errors.New("fly OIDC token is already expired")
	}
	if err := writeTokenAtomically(m.path, token); err != nil {
		return time.Time{}, err
	}
	return expires, nil
}

func (m *oidcTokenManager) Errors() <-chan error { return m.errors }

func (m *oidcTokenManager) Close() {
	m.closeOnce.Do(func() {
		if m.cancel != nil {
			m.cancel()
			<-m.done
		}
		_ = os.Remove(m.path)
	})
}

func jwtExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, errors.New("fly OIDC response is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("decode Fly OIDC token claims: %w", err)
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, fmt.Errorf("decode Fly OIDC token claims: %w", err)
	}
	if claims.Exp == 0 {
		return time.Time{}, errors.New("fly OIDC token has no exp claim")
	}
	return time.Unix(claims.Exp, 0), nil
}

func writeTokenAtomically(path, token string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create OIDC token directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".oidc-token-*")
	if err != nil {
		return fmt.Errorf("create OIDC token file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(token); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace OIDC token file: %w", err)
	}
	return nil
}

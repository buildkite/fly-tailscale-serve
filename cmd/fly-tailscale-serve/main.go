package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
	"tailscale.com/client/local"
	"tailscale.com/tsnet"
)

var version = "dev"

const (
	appHost            = "127.0.0.1"
	flyAPISocket       = "/.fly/api"
	processStopTimeout = 10 * time.Second
)

type cli struct {
	Version kong.VersionFlag `help:"Print version and exit."`

	// App
	AppCommand string `env:"APP_COMMAND" help:"Space-separated command used to start the local application process." required:"" group:"App"`
	Port       int    `env:"PORT" help:"Local application port on 127.0.0.1." required:"" group:"App"`

	// Runtime
	StartupTimeout      time.Duration `env:"STARTUP_TIMEOUT" help:"How long to wait for tsnet and the application to become healthy." default:"30s" group:"Runtime"`
	ShutdownGrace       time.Duration `env:"SHUTDOWN_GRACE" help:"How long to wait after stopping the proxy before stopping the app." default:"3s" group:"Runtime"`
	WatchdogInterval    time.Duration `env:"WATCHDOG_INTERVAL" help:"How often the watchdog should check Tailscale health." default:"1m" group:"Runtime"`
	WatchdogMaxFailures int           `env:"WATCHDOG_MAX_FAILURES" help:"Consecutive watchdog failures before exiting." default:"3" group:"Runtime"`

	// Tailscale
	AdvertiseTags []string `env:"TAILSCALE_ADVERTISE_TAGS" help:"Comma-separated Tailscale tags to advertise." required:"" group:"Tailscale"`
	ServiceName   string   `env:"TAILSCALE_SERVICE_NAME" help:"Named Tailscale service to publish (e.g. svc:my-service)." required:"" group:"Tailscale"`
	StateDir      string   `env:"TAILSCALE_STATE_DIR" type:"path" help:"tsnet state directory." default:"/data/tailscale" group:"Tailscale"`
	Hostname      string   `env:"TAILSCALE_HOSTNAME" help:"Tailscale hostname. Defaults to FLY_APP_NAME or FLY_APP_NAME-FLY_MACHINE_ID when running on Fly." group:"Tailscale"`
	OIDCClientID  string   `env:"TAILSCALE_OIDC_CLIENT_ID" help:"Tailscale federated identity client ID." required:"" group:"Tailscale"`
	OIDCAudience  string   `env:"TAILSCALE_OIDC_AUDIENCE" help:"Audience to request from Fly's OIDC token endpoint." required:"" group:"Tailscale"`
	AcceptAppCaps []string `env:"TAILSCALE_ACCEPT_APP_CAPS" help:"Comma-separated app capabilities to forward via Tailscale-App-Capabilities header." group:"Tailscale"`

	// Fly
	FlyAppName   string `env:"FLY_APP_NAME" hidden:""`
	FlyMachineID string `env:"FLY_MACHINE_ID" hidden:""`
}

type processHandle struct {
	cmd      *exec.Cmd
	result   chan error
	waitOnce sync.Once
	waitErr  error
}

type service struct {
	cli           *cli
	flyHTTPClient *http.Client
	appHTTPClient *http.Client

	app *processHandle

	ts           *tsnet.Server
	localClient  *local.Client
	loopbackAddr string
	localAPICred string

	listener    net.Listener
	proxyServer *http.Server
	proxyDone   chan error
}

type exitCodeError struct {
	code int
	err  error
}

func (e *exitCodeError) Error() string {
	return e.err.Error()
}

func (e *exitCodeError) Unwrap() error {
	return e.err
}

func main() {
	log.SetFlags(log.LstdFlags)

	var cli cli
	kong.Parse(
		&cli,
		kong.Name("fly-tailscale-serve"),
		kong.Description("Bootstrap a Tailscale service for a localhost application running on a Fly Machine."),
		kong.UsageOnError(),
		kong.Vars{"version": version},
	)

	if err := cli.validate(); err != nil {
		log.Print(err)
		os.Exit(1)
	}

	if err := run(&cli); err != nil {
		var exitErr *exitCodeError
		if errors.As(err, &exitErr) {
			log.Print(exitErr.err)
			os.Exit(exitErr.code)
		}

		log.Print(err)
		os.Exit(1)
	}
}

func run(cli *cli) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	if err := ensureFlySocket(); err != nil {
		return err
	}

	if err := os.MkdirAll(cli.StateDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", cli.StateDir, err)
	}

	svc := newService(cli)
	defer svc.shutdown()
	defer stop()

	if err := svc.startTailscale(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			log.Println("Received shutdown signal, shutting down...")
			return nil
		}

		return err
	}

	if err := svc.startApp(); err != nil {
		return err
	}

	appDone := make(chan error, 1)
	go func() {
		appDone <- svc.app.Wait()
	}()

	if err := svc.waitForAppHealth(ctx, appDone); err != nil {
		if errors.Is(err, context.Canceled) {
			log.Println("Received shutdown signal, shutting down...")
			return nil
		}

		return err
	}

	if err := svc.startProxy(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			log.Println("Received shutdown signal, shutting down...")
			return nil
		}

		return err
	}

	watchdogFatal := make(chan error, 1)
	go svc.watchdog(ctx, watchdogFatal)

	select {
	case <-ctx.Done():
		log.Println("Received shutdown signal, shutting down...")
		return nil
	case err := <-watchdogFatal:
		return &exitCodeError{code: 1, err: err}
	case err := <-svc.proxyDone:
		return &exitCodeError{code: 1, err: fmt.Errorf("proxy server exited unexpectedly: %w", err)}
	case err := <-appDone:
		if err == nil {
			return nil
		}

		return &exitCodeError{code: exitCode(err), err: fmt.Errorf("app process exited: %w", err)}
	}
}

func newService(cli *cli) *service {
	flyTransport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", flyAPISocket)
		},
	}

	return &service{
		cli: cli,
		flyHTTPClient: &http.Client{
			Transport: flyTransport,
			Timeout:   10 * time.Second,
		},
		appHTTPClient: &http.Client{Timeout: 2 * time.Second},
	}
}

func (c *cli) validate() error {
	appCommand := c.appCommandArgs()
	if len(appCommand) == 0 {
		return errors.New("APP_COMMAND must contain at least one argument")
	}
	c.AppCommand = strings.Join(appCommand, " ")

	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("PORT must be between 1 and 65535")
	}

	if c.StartupTimeout <= 0 {
		return fmt.Errorf("STARTUP_TIMEOUT must be greater than 0")
	}

	if c.ShutdownGrace <= 0 {
		return fmt.Errorf("SHUTDOWN_GRACE must be greater than 0")
	}

	if c.WatchdogInterval <= 0 {
		return fmt.Errorf("WATCHDOG_INTERVAL must be greater than 0")
	}

	if c.WatchdogMaxFailures < 1 {
		return fmt.Errorf("WATCHDOG_MAX_FAILURES must be at least 1")
	}

	c.AdvertiseTags = normalizeTags(c.AdvertiseTags)
	if len(c.AdvertiseTags) == 0 {
		return errors.New("TAILSCALE_ADVERTISE_TAGS must contain at least one tag")
	}

	if !strings.HasPrefix(c.ServiceName, "svc:") {
		return errors.New("TAILSCALE_SERVICE_NAME must start with \"svc:\"")
	}

	hostname, err := c.resolveHostname()
	if err != nil {
		return err
	}
	c.Hostname = hostname

	return nil
}

func (c *cli) appURL() string {
	return fmt.Sprintf("http://%s:%d", appHost, c.Port)
}

func (c *cli) appCommandArgs() []string {
	return strings.Fields(c.AppCommand)
}

func (c *cli) tailscaleClientID() string {
	return appendPersistentWorkloadFlags(c.OIDCClientID)
}

func (c *cli) resolveHostname() (string, error) {
	if trimmed := strings.TrimSpace(c.Hostname); trimmed != "" {
		return trimmed, nil
	}

	appName := strings.TrimSpace(c.FlyAppName)
	if appName == "" {
		return "", errors.New("TAILSCALE_HOSTNAME must be set when FLY_APP_NAME is unavailable")
	}

	machineID := strings.TrimSpace(c.FlyMachineID)
	if machineID == "" {
		return appName, nil
	}

	return appName + "-" + machineID, nil
}

func appendPersistentWorkloadFlags(clientID string) string {
	separator := "?"
	if strings.Contains(clientID, "?") {
		separator = "&"
	}

	return clientID + separator + "ephemeral=false&preauthorized=true"
}

func normalizeTags(tags []string) []string {
	result := make([]string, 0, len(tags))
	for _, tag := range tags {
		trimmed := strings.TrimSpace(tag)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}

	return result
}

func ensureFlySocket() error {
	info, err := os.Stat(flyAPISocket)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("expected Fly Machines API socket at %s, but it was not found", flyAPISocket)
		}
		return fmt.Errorf("stat %s: %w", flyAPISocket, err)
	}

	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("expected %s to be a unix socket", flyAPISocket)
	}

	return nil
}

func (s *service) startTailscale(ctx context.Context) error {
	log.Printf("Requesting Fly OIDC token for audience %s...", s.cli.OIDCAudience)
	identityToken, err := s.requestIdentityToken(ctx)
	if err != nil {
		return err
	}

	log.Println("Starting tsnet server...")

	s.ts = &tsnet.Server{
		Dir:           s.cli.StateDir,
		Hostname:      s.cli.Hostname,
		AdvertiseTags: s.cli.AdvertiseTags,
		ClientID:      s.cli.tailscaleClientID(),
		IDToken:       identityToken,
		UserLogf:      log.Printf,
	}

	upCtx, cancel := context.WithTimeout(ctx, s.cli.StartupTimeout)
	defer cancel()

	st, err := s.ts.Up(upCtx)
	if err != nil {
		return fmt.Errorf("start tsnet: %w", err)
	}

	lc, err := s.ts.LocalClient()
	if err != nil {
		return fmt.Errorf("create tsnet local client: %w", err)
	}

	s.localClient = lc

	loopbackAddr, _, localAPICred, err := s.ts.Loopback()
	if err != nil {
		return fmt.Errorf("start tsnet loopback: %w", err)
	}
	s.loopbackAddr = loopbackAddr
	s.localAPICred = localAPICred

	log.Printf("Tailscale up as %s, IPs=%v", st.Self.DNSName, st.TailscaleIPs)
	log.Printf("LocalAPI available at %s", loopbackAddr)
	return nil
}

func (s *service) requestIdentityToken(ctx context.Context) (string, error) {
	payload := map[string]string{"aud": s.cli.OIDCAudience}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal Fly OIDC request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://fly.local/v1/tokens/oidc", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build Fly OIDC request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.flyHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request Fly OIDC token: %w", err)
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read Fly OIDC response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("Fly OIDC request failed with %s: %s", resp.Status, strings.TrimSpace(string(responseBody)))
	}

	return strings.TrimSpace(string(responseBody)), nil
}

func (s *service) startApp() error {
	log.Printf("Starting app command %q on %s...", s.cli.AppCommand, s.cli.appURL())

	args := s.cli.appCommandArgs()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		"TS_LOCALAPI_ADDR="+s.loopbackAddr,
		"TS_LOCALAPI_CRED="+s.localAPICred,
	)

	handle, err := startProcess(cmd)
	if err != nil {
		return fmt.Errorf("start app command: %w", err)
	}

	s.app = handle
	return nil
}

func (s *service) waitForAppHealth(ctx context.Context, appDone <-chan error) error {
	deadline := time.Now().Add(s.cli.StartupTimeout)
	attempt := 1

	for !time.Now().After(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cli.appURL(), nil)
		if err != nil {
			return fmt.Errorf("build app health request: %w", err)
		}

		resp, err := s.appHTTPClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				return nil
			}
		}

		log.Printf("Waiting for app to start... (%d/%d)", attempt, int(s.cli.StartupTimeout/time.Second))
		attempt++

		select {
		case err := <-appDone:
			if err == nil {
				return fmt.Errorf("app process exited before becoming reachable at %s", s.cli.appURL())
			}

			return fmt.Errorf("app process exited before becoming reachable at %s: %w", s.cli.appURL(), err)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}

	return fmt.Errorf("app did not become reachable at %s", s.cli.appURL())
}

func (s *service) startProxy(ctx context.Context) error {
	mode := tsnet.ServiceModeHTTP{
		HTTPS: true,
		Port:  443,
	}
	if len(s.cli.AcceptAppCaps) > 0 {
		mode.AcceptAppCaps = map[string][]string{
			"/": s.cli.AcceptAppCaps,
		}
	}

	log.Printf("Listening on Tailscale service %s...", s.cli.ServiceName)
	ln, err := s.ts.ListenService(s.cli.ServiceName, mode)
	if err != nil {
		return fmt.Errorf("listen on Tailscale service %s: %w", s.cli.ServiceName, err)
	}
	s.listener = ln

	target := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(appHost, strconv.Itoa(s.cli.Port)),
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("proxy error: %v", err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},
	}

	s.proxyServer = &http.Server{
		Handler:           proxy,
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.proxyDone = make(chan error, 1)

	go func() {
		err := s.proxyServer.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.proxyDone <- err
		}
		close(s.proxyDone)
	}()

	log.Printf("Reverse proxy configured for %s → %s", s.cli.ServiceName, s.cli.appURL())
	return nil
}

func (s *service) watchdog(ctx context.Context, fatal chan<- error) {
	ticker := time.NewTicker(s.cli.WatchdogInterval)
	defer ticker.Stop()

	failures := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if err := s.tailscaleHealthy(ctx); err != nil {
			failures++
			log.Printf("watchdog: tailscale health check failed (%d/%d): %v", failures, s.cli.WatchdogMaxFailures, err)

			if failures >= s.cli.WatchdogMaxFailures {
				fatalErr := fmt.Errorf("watchdog: tailscale became unhealthy after %d consecutive failures: %v", s.cli.WatchdogMaxFailures, err)
				select {
				case fatal <- fatalErr:
				case <-ctx.Done():
				}
				return
			}

			continue
		}

		failures = 0
	}
}

func (s *service) tailscaleHealthy(ctx context.Context) error {
	st, err := s.localClient.StatusWithoutPeers(ctx)
	if err != nil {
		return err
	}
	if st == nil || st.Self == nil {
		return errors.New("missing tsnet self status")
	}
	if st.BackendState != "Running" {
		return fmt.Errorf("backend state = %s", st.BackendState)
	}
	if len(st.TailscaleIPs) == 0 {
		return errors.New("no tailscale IPs assigned")
	}
	return nil
}

func (s *service) shutdown() {
	if s.proxyServer != nil {
		log.Println("Shutting down proxy server...")
		ctx, cancel := context.WithTimeout(context.Background(), s.cli.ShutdownGrace)
		if err := s.proxyServer.Shutdown(ctx); err != nil {
			log.Printf("proxy shutdown error: %v", err)
		}
		cancel()
	}

	if s.app != nil {
		if err := s.app.Terminate(processStopTimeout); err != nil && !isExpectedShutdownError(err) {
			log.Printf("app shutdown error: %v", err)
		}
		s.app = nil
	}

	if s.listener != nil {
		_ = s.listener.Close()
		s.listener = nil
	}

	if s.ts != nil {
		log.Println("Closing tsnet server...")
		if err := s.ts.Close(); err != nil {
			log.Printf("tsnet close error: %v", err)
		}
		s.ts = nil
	}
}

func startProcess(cmd *exec.Cmd) (*processHandle, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	handle := &processHandle{
		cmd:    cmd,
		result: make(chan error, 1),
	}
	go func() {
		handle.result <- cmd.Wait()
	}()

	return handle, nil
}

func (p *processHandle) Wait() error {
	if p == nil {
		return nil
	}

	p.waitOnce.Do(func() {
		p.waitErr = <-p.result
	})

	return p.waitErr
}

func (p *processHandle) Terminate(timeout time.Duration) error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}

	err := p.cmd.Process.Signal(syscall.SIGTERM)
	if err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
		return err
	}

	done := make(chan error, 1)
	go func() {
		done <- p.Wait()
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		_ = p.cmd.Process.Kill()
		return <-done
	}
}

func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}

	return 1
}

func isExpectedShutdownError(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}

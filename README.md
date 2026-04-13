# fly-tailscale-serve

Bootstrap [Tailscale Serve](https://tailscale.com/kb/1312/serve) for a localhost application running on a [Fly Machine](https://fly.io/docs/machines/).

`fly-tailscale-serve` is a process supervisor that handles the full lifecycle of publishing a Fly Machines app into a Tailscale tailnet as a named service, with no public internet exposure.

## What it does

1. Starts `tailscaled` in userspace mode inside a Fly Machine.
2. Requests a Fly OIDC token from the local Fly API socket.
3. Authenticates to Tailscale using workload identity federation.
4. Starts your application on `127.0.0.1`.
5. Publishes the app as a named Tailscale service via `tailscale serve`.
6. Runs a watchdog that exits the process after repeated Tailscale health failures, letting Fly restart the machine cleanly.
7. Shuts down gracefully: drains the named service, stops the app, resets Serve, disconnects from Tailscale, and stops `tailscaled`.

## Install

### mise (recommended)

```toml
[tools]
"github:buildkite/fly-tailscale-serve" = "latest"
```

Since this is an internal repository, `MISE_GITHUB_TOKEN` (or `GITHUB_TOKEN`) must be set to a token with access to the repo. See [mise docs](https://mise.jdx.dev/getting-started.html#github-api-rate-limiting) for details.

For local development:

```sh
export MISE_GITHUB_TOKEN=$(gh auth token)
mise install
```

### Go

```sh
GOPRIVATE=github.com/buildkite go install github.com/buildkite/fly-tailscale-serve/cmd/fly-tailscale-serve@latest
```

### GitHub Releases

Download a binary from the [releases page](https://github.com/buildkite/fly-tailscale-serve/releases).

## Usage

`fly-tailscale-serve` is designed to run as the entrypoint of a Fly Machine. It reads all configuration from environment variables.

### Fly Deployment

Set the binary as the entrypoint in your `Dockerfile`:

```dockerfile
ENTRYPOINT ["fly-tailscale-serve"]
```

Configure your `fly.toml` with no public `[http_service]`:

```toml
[deploy]
  strategy = "immediate"

[[mounts]]
  source = "tailscale_state"
  destination = "/data"

[env]
  APP_COMMAND = "node server.js"
  PORT = "8080"
  TAILSCALE_ADVERTISE_TAGS = "tag:my-service"
  TAILSCALE_SERVICE_NAME = "svc:my-service"
  TAILSCALE_OIDC_CLIENT_ID = "<federated-identity-id>"
  TAILSCALE_OIDC_AUDIENCE = "tailscale.com"
```

### Verification

After deploying:

```sh
fly logs
tailscale status
tailscale debug resolve my-service.<your-tailnet>.ts.net
curl https://my-service.<your-tailnet>.ts.net/
```

What to look for:

- Fly logs should show `Starting tailscaled...`, successful Tailscale auth, app startup, and `Tailscale serve configured successfully`.
- `tailscale debug resolve` should return service VIPs for the named service.
- `curl` from a tailnet client should return your app's response with forwarded identity headers.

## Environment Variables

### Required

| Variable | Description |
|---|---|
| `APP_COMMAND` | Space-separated command to start the local application (e.g. `bun run index.tsx`) |
| `PORT` | Local application port on `127.0.0.1` |
| `TAILSCALE_ADVERTISE_TAGS` | Comma-separated Tailscale tags to advertise |
| `TAILSCALE_SERVICE_NAME` | Named Tailscale service to publish (e.g. `svc:my-service`) |
| `TAILSCALE_OIDC_CLIENT_ID` | Tailscale federated identity client ID |
| `TAILSCALE_OIDC_AUDIENCE` | Audience to request from Fly's OIDC token endpoint |

### Optional

| Variable | Default | Description |
|---|---|---|
| `TAILSCALE_ACCEPT_APP_CAPS` | | Comma-separated app capabilities to forward via `Tailscale-App-Capabilities` header |
| `TAILSCALE_HOSTNAME` | `FLY_APP_NAME` or `FLY_APP_NAME-FLY_MACHINE_ID` | Tailscale hostname for the node |
| `TAILSCALE_STATE_DIR` | `/data/tailscale` | `tailscaled` state directory |
| `TAILSCALE_SOCKET` | `/var/run/tailscale/tailscaled.sock` | `tailscaled` local API socket path |
| `TAILSCALED_PID_FILE` | `/var/run/tailscale/tailscaled.pid` | `tailscaled` PID file path |
| `STARTUP_TIMEOUT` | `30s` | How long to wait for `tailscaled` and the app to become healthy |
| `SHUTDOWN_GRACE` | `3s` | How long to wait after draining the service before stopping the app |
| `WATCHDOG_INTERVAL` | `1m` | How often the watchdog checks Tailscale health |
| `WATCHDOG_MAX_FAILURES` | `3` | Consecutive watchdog failures before the process exits |

## Startup Flow

1. Start `tailscaled` in userspace mode.
2. Wait for the daemon to be ready.
3. Request a Fly OIDC token from `/.fly/api`.
4. Run `tailscale up` with the federated identity client ID, the Fly token, the machine hostname, and the advertised tag.
5. Wait for Tailscale to reach Running state.
6. Start the configured app command on `http://127.0.0.1:${PORT}` and verify it responds.
7. Publish `TAILSCALE_SERVICE_NAME` with Tailscale Serve.
8. Run a watchdog so repeated Tailscale health failures stop the process and let Fly restart the machine cleanly.

## Tailnet Prerequisites

### Federated Identity

The Fly app needs a Tailscale federated identity that trusts Fly's workload identity issuer:

```hcl
resource "tailscale_federated_identity" "my_service" {
  description = "my-service on Fly"
  issuer      = "https://oidc.fly.io/<fly-org>"
  audience    = "tailscale.com"
  subject     = "<fly-org>:<fly-app-name>:*"
  scopes      = ["auth_keys"]
  tags        = ["tag:my-service"]
}
```

Export the identity ID to use as `TAILSCALE_OIDC_CLIENT_ID`:

```hcl
output "my_service_federated_identity_client_id" {
  value = tailscale_federated_identity.my_service.id
}
```

### Tailnet Policy

The tailnet policy needs:

1. A `tagOwners` entry for the service tag.
2. An `autoApprovers.services` entry allowing the tag to advertise the service.
3. A grant allowing clients to reach the named service.

```jsonc
{
  "grants": [
    {
      "src": ["autogroup:member"],
      "dst": ["svc:my-service"],
      "ip":  ["*"]
    }
  ],
  "tagOwners": {
    "tag:my-service": ["autogroup:admin"]
  },
  "autoApprovers": {
    "services": {
      "svc:my-service": ["tag:my-service"]
    }
  }
}
```

### App Capabilities

To forward `Tailscale-App-Capabilities` headers, three things are needed:

1. Set `TAILSCALE_ACCEPT_APP_CAPS` to the capabilities you want forwarded.
2. A `svc:` grant for network reachability.
3. An `app` grant targeting the node's tag (not the service name):

```jsonc
{
  "src": ["autogroup:member"],
  "dst": ["tag:my-service"],
  "app": {
    "example.com/cap/role": [{"role": "viewer"}]
  }
}
```

### Named Service

The named Tailscale service must be created in the [Tailscale admin UI](https://login.tailscale.com/admin/services) before deploying. `tailscale serve --service=...` requires the service object to exist.

### Hostname vs Service Name

Keep the Tailscale node hostname distinct from the named service. The hostname comes from `TAILSCALE_HOSTNAME` (or falls back to `FLY_APP_NAME` / `FLY_APP_NAME-FLY_MACHINE_ID`), while the service name is set via `TAILSCALE_SERVICE_NAME`.

## Releasing

Releases are managed by [GoReleaser](https://goreleaser.com). Tag, push, and the Buildkite pipeline handles the rest:

```sh
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

Or release locally:

```sh
GITHUB_TOKEN=$(gh auth token) goreleaser release --clean
```

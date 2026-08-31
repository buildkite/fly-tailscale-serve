# fly-tailscale-serve

Publish a localhost application running on a [Fly Machine](https://fly.io/docs/machines/) into a Tailscale tailnet as a named [Tailscale Service](https://tailscale.com/kb/1552/tailscale-services), with no public internet exposure.

`fly-tailscale-serve` is a process supervisor that handles the full lifecycle of publishing a Fly Machines app into a tailnet using [tsnet](https://tailscale.com/docs/features/tsnet) and an in-process reverse proxy.

## What it does

1. Requests a Fly OIDC token from the local Fly API socket.
2. Starts an embedded Tailscale node using `tsnet` with workload identity federation.
3. Exposes the Tailscale LocalAPI via `tsnet.Loopback()` for downstream app use.
4. Starts your application on `127.0.0.1`, passing `TS_LOCALAPI_ADDR` and `TS_LOCALAPI_CRED` env vars.
5. Publishes the app as a named Tailscale service via `tsnet.ListenService`.
6. Runs an in-process HTTPS reverse proxy that forwards traffic from the tailnet to your app.
7. Runs a watchdog that exits the process after repeated Tailscale health failures, letting Fly restart the machine cleanly.
8. Shuts down gracefully: drains the proxy, stops the app, and closes the tsnet server.

Optionally, PID 1 can also supervise [Litestream](https://litestream.io/) as a sibling of the app. This provides ordered restore, replication readiness, fail-closed operation, and final sync without importing Litestream's unstable Go API.

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

Set the binary as the entrypoint in your `Dockerfile`, passing the app command as a CLI argument:

```dockerfile
ENTRYPOINT ["fly-tailscale-serve", "--app-command", "node server.js"]
```

Configure your `fly.toml` with no public `[http_service]`:

```toml
[deploy]
  strategy = "immediate"

[[mounts]]
  source = "tailscale_state"
  destination = "/data"

[env]
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

- Fly logs should show tsnet startup, successful Tailscale auth, app startup, and `Reverse proxy configured`.
- `tailscale debug resolve` should return service VIPs for the named service.
- `curl` from a tailnet client should return your app's response.

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
| `TAILSCALE_STATE_DIR` | `/data/tailscale` | tsnet state directory |
| `STARTUP_TIMEOUT` | `30s` | How long to wait for tsnet and the app to become healthy |
| `SHUTDOWN_GRACE` | `3s` | How long to wait after stopping the proxy before stopping the app |
| `WATCHDOG_INTERVAL` | `1m` | How often the watchdog checks Tailscale health |
| `WATCHDOG_MAX_FAILURES` | `3` | Consecutive watchdog failures before the process exits |

### Injected into the app process

These env vars are set automatically on the app process and do not need to be configured:

| Variable | Description |
|---|---|
| `TS_LOCALAPI_ADDR` | Loopback address (`host:port`) of the tsnet LocalAPI listener |
| `TS_LOCALAPI_CRED` | Credential for authenticating to the LocalAPI (use as basic auth password with username `tsnet` and `Sec-Tailscale: localapi` header) |

## Litestream supervision

Litestream support is disabled unless `LITESTREAM_CONFIG`, `LITESTREAM_DB`, and `LITESTREAM_SOCKET` are all set. When disabled, startup and shutdown behavior is unchanged.

### Lifecycle and failure behavior

With supervision enabled, PID 1 owns both child processes and does the following:

1. Runs `litestream restore -if-db-not-exists -if-replica-exists` before starting the app. A valid empty replica is a successful first boot and leaves the DB absent for the app to create.
2. Starts `litestream replicate`, waits for the configured control socket to answer `litestream info`, and forces `sync -wait` before app startup when a DB already exists or was restored.
3. Starts the app. On a fresh database, it waits for app health and then forces `sync -wait` before exposing the Tailscale proxy.
4. Stops the app if Litestream exits or AWS workload identity can no longer be refreshed. This prevents unprotected writes.
5. If the app exits first, preserves its exit status while stopping Litestream.
6. On a signal, drains the proxy, stops and waits for the app so SQLite closes, forces a final sync, and then stops Litestream. All phases share `LITESTREAM_SHUTDOWN_TIMEOUT`; after proxy drain, each phase receives at most half the remaining time so an uncooperative app or sync cannot consume the next phase's budget. Expired phases are killed cleanly.

Control-socket commands are used instead of log parsing. Litestream configuration remains the source of truth for replicas, retention, encryption, and provider settings.

### Configuration

| Variable | Default | Description |
|---|---|---|
| `LITESTREAM_CONFIG` | disabled | Absolute Litestream YAML path; setting it enables supervision |
| `LITESTREAM_DB` | disabled | Exact absolute SQLite path present in the YAML |
| `LITESTREAM_SOCKET` | disabled | Exact absolute Unix socket path present in the YAML |
| `LITESTREAM_BINARY` | `litestream` | Binary name or path |
| `LITESTREAM_STARTUP_TIMEOUT` | `30s` | Shared budget for restore, socket readiness, and initial sync |
| `LITESTREAM_SHUTDOWN_TIMEOUT` | `15s` | Total budget for proxy drain, app shutdown, final sync, and Litestream shutdown |
| `LITESTREAM_AWS_ROLE_ARN` | disabled | IAM role passed to Litestream as `AWS_ROLE_ARN`; enables managed Fly OIDC refresh |
| `LITESTREAM_AWS_ROLE_SESSION_NAME` | generated by AWS SDK | Optional `AWS_ROLE_SESSION_NAME` for CloudTrail |
| `LITESTREAM_AWS_OIDC_AUDIENCE` | `sts.amazonaws.com` | Fly OIDC token audience |
| `LITESTREAM_AWS_WEB_IDENTITY_TOKEN_FILE` | `/run/fly-tailscale-serve/aws-web-identity-token` | Private token file passed as `AWS_WEB_IDENTITY_TOKEN_FILE` |
| `LITESTREAM_AWS_OIDC_REFRESH_BEFORE` | `2m` | Refresh margin before token expiry |

Paths are explicit and must be absolute. The three main Litestream settings must be provided together. The YAML must enable the same control socket:

```yaml
socket:
  enabled: true
  path: /run/litestream.sock
  permissions: 0600

shutdown-sync-timeout: 4s
shutdown-sync-interval: 500ms

dbs:
  - path: /data/slopcannon.db
    busy-timeout: 30s
    replica:
      type: s3
      bucket: ${LITESTREAM_BUCKET}
      path: slopcannon/sqlite
      region: ${AWS_REGION}
      sync-interval: 1s
```

Keep bucket names and non-secret settings in configuration. Put static AWS keys, if required, in Fly secrets; Litestream still receives its normal environment and uses the AWS credential chain. Workload OIDC is recommended because it avoids long-lived keys.

### Install and pin Litestream

Install Litestream in the application image; the supervisor never downloads executables at runtime. This example pins official v0.5.16 and verifies the release checksum:

```dockerfile
ARG TARGETARCH
ARG LITESTREAM_VERSION=0.5.16
RUN set -eux; \
    case "$TARGETARCH" in \
      amd64) archive_arch=x86_64; sha=9e29112380a942e4a62ee07773684396cb8b308dc4d67e130bef41f75e937f0a ;; \
      arm64) archive_arch=arm64; sha=678022e4103145302598e35d37f8718392d42e153feeb1e2d4a64dd0cd3aaf10 ;; \
      *) exit 1 ;; \
    esac; \
    archive="litestream-${LITESTREAM_VERSION}-linux-${archive_arch}.tar.gz"; \
    curl -fsSLO "https://github.com/benbjohnson/litestream/releases/download/v${LITESTREAM_VERSION}/${archive}"; \
    echo "${sha}  ${archive}" | sha256sum -c -; \
    tar -xzf "${archive}" -C /usr/local/bin litestream; \
    rm "${archive}"; \
    litestream version
```

### Fly and Slopcannon example

For Slopcannon, keep the app command direct. Do not use Litestream's `exec` setting or nested shell quoting:

```toml
[env]
  APP_COMMAND = "./slopcannon serve --host=127.0.0.1 --db=/data/slopcannon.db --data-dir=/data/sites --owners=owners.yaml --base-url=https://slopcannon.example.ts.net/"
  PORT = "8080"
  LITESTREAM_CONFIG = "/etc/litestream.yml"
  LITESTREAM_DB = "/data/slopcannon.db"
  LITESTREAM_SOCKET = "/run/litestream.sock"
  LITESTREAM_STARTUP_TIMEOUT = "30s"
  SHUTDOWN_GRACE = "3s"
  LITESTREAM_SHUTDOWN_TIMEOUT = "15s"
  LITESTREAM_BUCKET = "slopcannon-backups"
  AWS_REGION = "ap-southeast-2"
  LITESTREAM_AWS_ROLE_ARN = "arn:aws:iam::123456789012:role/slopcannon-litestream"
  LITESTREAM_AWS_ROLE_SESSION_NAME = "slopcannon"
```

Set Fly `kill_signal = "SIGTERM"` and a `kill_timeout` greater than `LITESTREAM_SHUTDOWN_TIMEOUT` (for example, 20 seconds).

**Litestream protects only `/data/slopcannon.db`. It does not protect `/data/sites`, `.versions`, uploads, or any adjacent file storage.** Back up those files independently and test a combined recovery.

### AWS workload identity

The supervisor follows [Fly's workload OIDC API](https://fly.io/docs/security/openid-connect/) and [AWS's standard web-identity credential contract](https://docs.aws.amazon.com/sdkref/latest/guide/feature-assume-role-credentials.html). It requests a JWT from `POST /v1/tokens/oidc` on `/.fly/api` with audience `sts.amazonaws.com`. It writes the token with mode `0600` under a mode `0700` directory, replaces it atomically before expiry, never logs its contents, and removes it after Litestream stops. Litestream's AWS SDK reads the standard `AWS_ROLE_ARN`, `AWS_WEB_IDENTITY_TOKEN_FILE`, and optional `AWS_ROLE_SESSION_NAME` variables and calls STS `AssumeRoleWithWebIdentity`.

Create an AWS IAM OIDC provider with:

```text
Provider URL: https://oidc.fly.io/<fly-org-slug>
Audience: sts.amazonaws.com
```

Restrict the role trust to the audience and the exact Fly app. Fly subjects have the form `<org>:<app>:<machine>`:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {"Federated": "arn:aws:iam::123456789012:oidc-provider/oidc.fly.io/my-org"},
    "Action": "sts:AssumeRoleWithWebIdentity",
    "Condition": {
      "StringEquals": {"oidc.fly.io/my-org:aud": "sts.amazonaws.com"},
      "StringLike": {"oidc.fly.io/my-org:sub": "my-org:slopcannon:*"}
    }
  }]
}
```

Attach a least-privilege policy for one bucket and prefix. Include delete only if Litestream retention needs it:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {"Effect":"Allow","Action":["s3:ListBucket","s3:GetBucketLocation"],"Resource":"arn:aws:s3:::slopcannon-backups","Condition":{"StringLike":{"s3:prefix":["slopcannon/sqlite/*"]}}},
    {"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:DeleteObject"],"Resource":"arn:aws:s3:::slopcannon-backups/slopcannon/sqlite/*"}
  ]
}
```

`LITESTREAM_AWS_ROLE_ARN` is not a secret and belongs in `fly.toml`. Do not set access-key secrets when using this flow. The Fly API issues tokens valid for about 15 minutes; the supervisor refreshes them before expiry. Temporary STS credentials are refreshed independently by the AWS SDK. If Fly token refresh repeatedly fails until the current JWT expires, PID 1 fails closed and stops the app.

Troubleshooting:

- `AccessDenied` from STS: verify the provider URL, role ARN, `aud` condition, and org/app spelling in `sub`.
- `InvalidIdentityToken`: verify Fly's issuer exists in IAM and the machine can access `/.fly/api`; check clock health.
- S3 authorization errors: verify both the bucket-level list policy and object-prefix policy.
- Socket readiness timeout: ensure YAML has `socket.enabled: true` and its path exactly matches `LITESTREAM_SOCKET`.
- Repeated restarts after an OIDC outage are intentional fail-closed behavior. Static credentials remain available through Litestream's normal AWS environment chain by leaving `LITESTREAM_AWS_ROLE_ARN` unset.

Fly's init can create an initial AWS web-identity token when a process-wide `AWS_ROLE_ARN` is set. This feature deliberately uses a Litestream-specific role setting and manages rotation itself, so credentials are scoped to the Litestream children and remain valid for long-running Machines.

## Startup Flow

1. Request a Fly OIDC token from `/.fly/api`.
2. Start an embedded Tailscale node using `tsnet` with the federated identity client ID, the Fly token, the machine hostname, and the advertised tags.
3. Wait for the tsnet server to reach Running state.
4. Start the LocalAPI loopback listener via `tsnet.Loopback()`.
5. Start the configured app command on `http://127.0.0.1:${PORT}` with `TS_LOCALAPI_ADDR` and `TS_LOCALAPI_CRED` env vars, and verify it responds.
6. Listen on the named Tailscale service via `tsnet.ListenService` with HTTPS on port 443.
7. Run an in-process reverse proxy forwarding requests to the local app.
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

The named Tailscale service must be created in the [Tailscale admin UI](https://login.tailscale.com/admin/services) before deploying. `tsnet.ListenService` requires the service object to exist.

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

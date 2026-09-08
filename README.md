# pgMux - PostgreSQL routing proxy

pgMux is a lightweight PostgreSQL routing proxy. It routes client connections to
different backends based on username, rewrites the connection's identity
parameters, and relays the authentication exchange, with full PostgreSQL
protocol support and pluggable routing logic.

It is small and deliberately narrow. Read [What is and is not
hardened](#what-is-and-is-not-hardened) before exposing it beyond a trusted
network — some of it holds up on a public listener and some of it does not.

Originally created to provide routing for [SQL Labs](https://labs.boringsql.com/)

## Features

- Username-based routing to different PostgreSQL backends
- Rewrites the backend `user` and, optionally, `database`
- Optional fallback backend so every username routes somewhere
- Startup-parameter rewrite hook for per-connection values
- Full PostgreSQL protocol support
- SSL/TLS support for secure client connections, opt-in TLS to the backend
- Optional refusal of plaintext connections, with certificate reload on renewal
- Graceful drain, connection stats, and per-IP connection limits
- `CancelRequest` forwarding, with a per-session key the client cannot reuse
- Pluggable routing via Router interface
- `cmd/pgmuxd`: a ready-made, environment-configured binary for open endpoints

## Quick Start

```go
package main

import (
    "context"
    "github.com/boringsql/pgmux"
)

func main() {
    // Create static mappings
    mappings := map[string]*pgmux.BackendConfig{
        "app_user": {
            Host: "10.0.1.50",
            Port: 5432,
            User: "postgres",
        },
    }

    router := pgmux.NewStaticRouter(mappings)
    proxy := pgmux.NewProxyServer(":5434", router)

    ctx := context.Background()
    proxy.Start(ctx)
}
```

Connect with: `psql -h localhost -p 5434 -U app_user -d mydb`

## TLS Configuration

Enable SSL/TLS for secure client connections:

```go
proxy := pgmux.NewProxyServer(":5434", router)
proxy.WithTLS(&pgmux.TLSConfig{
    Enabled:  true,
    CertFile: "server.crt",
    KeyFile:  "server.key",
})
proxy.Start(ctx)
```

Connect with TLS: `psql 'host=localhost port=5434 user=app_user dbname=mydb sslmode=require'`

For advanced TLS settings:

```go
proxy.WithTLS(&pgmux.TLSConfig{
    Enabled: true,
    Config: &tls.Config{
        MinVersion: tls.VersionTLS13,
        // Custom certificates, cipher suites, etc.
    },
})
```

## Open endpoints: one destination for every visitor

The default is per-user routing: a username with no mapping is refused. An open
endpoint wants the opposite — anyone who types `psql -h your.host` should land
somewhere useful, whatever their local account happens to be called.

Two things make that work. `WithFallback` routes unmapped usernames, and
`BackendConfig.Database` rewrites the database, because libpq defaults **both**
`user` and `dbname` to the visitor's OS username when `-U` and `-d` are omitted.
Rewriting only the user leaves the visitor asking for a database named after
themselves, which does not exist.

```go
router := pgmux.NewStaticRouter(nil).WithFallback(&pgmux.BackendConfig{
    Host:     "10.0.1.50",
    Port:     5432,
    User:     "guest",     // every visitor arrives as guest
    Database: "showcase",  // ...in showcase, whatever they asked for
})

proxy := pgmux.NewProxyServer(":5432", router)
proxy.Start(context.Background())
```

Now all of these reach `guest@showcase`:

```console
$ psql -h your.host                              # user and dbname both default to $USER
$ psql -h your.host -U guest
$ psql -h your.host -U anything -d anything
```

```
 current_user | current_database
--------------+------------------
 guest        | showcase
```

Explicit mappings still win over the fallback, so the two styles mix:

```go
router := pgmux.NewStaticRouter(map[string]*pgmux.BackendConfig{
    "staff": {Host: "10.0.1.60", Port: 5432, User: "staff_role"},
}).WithFallback(publicBackend)
```

**A note on the prompt.** psql reports the connection it *requested*, not the one
the server gave it, so a visitor who omits `-U` sees their own username in the
prompt and in `\conninfo`:

```console
$ whoami
alice
$ psql -h your.host
alice=> SELECT current_user, current_database();
 current_user | current_database
--------------+------------------
 guest        | showcase
```

This is inherent to rewriting startup parameters and pgMux cannot change it. If
the mismatch matters, tell visitors to pass `-U guest` explicitly.

### Database pass-through (the default)

Leave `Database` empty and the client's own database is forwarded untouched —
the behaviour before this feature existed, and what per-user setups want:

```go
// -d mydb reaches the backend as mydb; only the user is rewritten.
"app_user": {Host: "10.0.1.50", Port: 5432, User: "postgres"}
```

## Limits

```go
proxy.WithLimits(&pgmux.Limits{
    MaxConnections:      100,           // global ceiling; default 1024
    MaxConnectionsPerIP: 10,            // per source address; default 0 (off)
    MaxMessageSize:      1 << 20,       // client-facing reads; default 16 MiB
    ClientIdleTimeout:   5 * time.Minute, // default 0 (disabled)
})
```

`MaxConnections` alone is a ceiling, not fairness: one host can fill it with
sockets that never send a startup message and hold the endpoint down in
`startupTimeout` windows. `MaxConnectionsPerIP` gives each source its own share.
Both refusals send a `53300` FATAL and are counted by `Stats()`
(`Rejected`, and `RejectedPerIP` for the per-address subset).

Two things to get right before enabling the per-IP cap:

- **It needs the real client address.** Behind an L4 balancer, HAProxy in
  `mode tcp`, or nginx `stream` without PROXY protocol — which pgMux does not
  support — every connection carries the balancer's address, they all share one
  key, and the cap becomes a global limit that locks everyone out.
- **Shared egress is one client.** A university or corporate NAT, or a CGNAT
  range, counts as a single source. IPv6 sources are keyed by `/64` rather than
  by address, for the same reason in reverse: a `/64` is the smallest normal
  end-site allocation, so keying on the full address would give anyone with a
  VPS 2^64 keys and make the limit decorative. Pick a value with room for a
  shared source — 5–10 for a showcase endpoint, not 1–2.

Rejections are logged at debug level only: past a cap, an unauthenticated
client would otherwise choose your log volume by reconnecting. Poll `Stats()`
if you want a saturation signal.

## Per-connection values: WithStartupRewrite

A `Router` only sees the username, so it cannot supply anything that varies per
connection. `WithStartupRewrite` runs after routing and after the user/database
rewrites, immediately before the StartupMessage goes to the backend:

```go
proxy.WithStartupRewrite(func(clientAddr net.Addr, params map[string]string) {
    // params["user"] and params["database"] are already rewritten here.
    host, _, _ := net.SplitHostPort(clientAddr.String())
    params["application_name"] = "visitor/" + host
})
```

The hook is a mechanism, not a policy: pgMux ships no `application_name`
feature of its own. The example above exists because PostgreSQL has no PROXY
protocol support — the backend sees the proxy's address on every connection, so
if you want the real client IP in `pg_stat_activity`, you have to put it there
yourself.

Nil (the default) is a no-op. A panic inside the hook is contained to that one
connection.

Configure the proxy fully before calling `Start`. The `With…` setters write
fields the accept loop reads without synchronisation, so changing them on a
running proxy is a data race. `BackendConfig` values handed to a router should
likewise be treated as immutable — swap them with `AddMapping` rather than
mutating in place.

## Logging

pgMux logs via `log/slog`, defaulting to `slog.Default()`:

```go
proxy.WithLogger(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
```

At the default level it emits **exactly one record per accepted connection**,
which is what you count for "how many people connected", and the only place the
real client address is recorded — once traffic reaches PostgreSQL, the backend
sees the proxy:

```
time=2026-09-06T16:47:58.825+02:00 level=INFO msg="connection accepted" client=203.0.113.9:61423
```

Startup parameters, protocol chatter and per-connection outcomes are at
`slog.LevelDebug`, because on an open endpoint every one of those values is
supplied by a stranger. Enable them deliberately:

```go
slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
```

Values are escaped by slog's handlers, so a username containing newlines cannot
forge a log record.

## Router Interface

Implement your own routing logic:

```go
type Router interface {
    Route(ctx context.Context, username string) (*BackendConfig, error)
}

type BackendConfig struct {
    Host     string
    Port     int
    User     string      // replaces the client's user
    Database string      // replaces the client's database; "" passes it through
    TLS      *tls.Config // non-nil upgrades the backend hop to TLS
}
```

## Custom Router Example

```go
type RestRouter struct {
    apiURL string
    cache  map[string]*pgmux.BackendConfig
}

func (r *RestRouter) Route(ctx context.Context, username string) (*pgmux.BackendConfig, error) {
    // Check cache first
    if config, ok := r.cache[username]; ok {
        return config, nil
    }

    // Call REST API
    resp, err := http.Get(r.apiURL + "/route/" + username)
    if err != nil {
        return nil, err
    }

    // Parse response, cache result
    config := parseResponse(resp)
    r.cache[username] = config
    return config, nil
}
```

## pgmuxd: the ready-made binary

`cmd/pgmuxd` is pgMux wired up for one job: a single backend, one role and one
database for every visitor, TLS mandatory. It exists so an open endpoint is a
job file rather than a program.

```console
$ go build ./cmd/pgmuxd     # or scripts/build.sh for static release binaries
```

Everything comes from the environment; there is no config file and nothing is
reloaded except the TLS certificate.

| Variable | Required | Default | |
|---|---|---|---|
| `PGMUXD_LISTEN` | yes | | e.g. `0.0.0.0:5432` |
| `PGMUXD_BACKEND_HOST` | yes | | |
| `PGMUXD_BACKEND_PORT` | | `5432` | |
| `PGMUXD_BACKEND_USER` | yes | | the role every visitor becomes |
| `PGMUXD_BACKEND_DATABASE` | yes | | required: libpq defaults `dbname` to the caller's OS username |
| `PGMUXD_TLS_CERT` / `PGMUXD_TLS_KEY` | yes | | absent means it will not start |
| `PGMUXD_MAX_CONNECTIONS` | | `100` | |
| `PGMUXD_MAX_CONNECTIONS_PER_IP` | | `5` | `0` disables |
| `PGMUXD_MAX_MESSAGE_SIZE` | | `1MiB` | |
| `PGMUXD_CLIENT_IDLE_TIMEOUT` | | `5m` | |
| `PGMUXD_STATEMENT_TIMEOUT` | | `30s` | a default for every session, not a limit |
| `PGMUXD_IDLE_TX_TIMEOUT` | | `60s` | a default for every session, not a limit |
| `PGMUXD_BACKEND_TLS` | | off | `1` encrypts the hop to the backend |
| `PGMUXD_HEALTH_ADDR` | | `127.0.0.1:8080` | must be loopback |
| `PGMUXD_LOG_LEVEL` | | `info` | `debug` adds per-connection detail |
| `PGMUXD_DRAIN_TIMEOUT` | | `4s` | see `kill_timeout` below |

Configuration is validated in full before anything binds — including loading the
keypair — and one run reports every problem, not the first:

```console
$ pgmuxd
pgmuxd: invalid configuration
  - PGMUXD_BACKEND_DATABASE is required
  - PGMUXD_HEALTH_ADDR "0.0.0.0:8080": must be a loopback address
```

The injected `statement_timeout` and `idle_in_transaction_session_timeout` are
defaults, not limits — both are `USERSET`, so a visitor can raise them with
`SET`. They stop the accidental runaway query; `PGMUXD_MAX_CONNECTIONS`,
`PGMUXD_CLIENT_IDLE_TIMEOUT` and backend-side supervision are what bound a
determined one.

**It refuses to run without TLS**, and refuses plaintext connections once
running. That is the client hop; the backend hop is cleartext unless
`PGMUXD_BACKEND_TLS=1`, and pgmuxd warns at startup if the backend is not
loopback and TLS is off. That guarantee moves out of `pg_hba.conf` when TLS terminates at the
proxy, and a typo must not put a plaintext PostgreSQL on the public internet.

### /healthz

Loopback only. It opens a real connection to the backend rather than checking
the listener, so a proxy whose database is dead does not stay in service:

```json
{ "status": "ok", "version": "v0.1.0", "commit": "8d0be2a",
  "backend": {"reachable": true, "latency_ms": 3, "max_connections": 100,
              "superuser_reserved_connections": 3, "reserved_connections": 0},
  "connections": {"current": 4, "accepted": 1281, "rejected": 0,
                  "rejected_per_ip": 0, "max": 100} }
```

The backend is probed on a timer, not per request, so a burst of health checks
cannot turn into a burst of backend connections. `200` when healthy; `503` while
starting, while draining, and after three consecutive probe failures. One blip
reports `degraded` and stays in service, because flapping an endpoint out of
Consul is worse than a briefly busy database. A proxy at its connection cap
reports `saturated` and stays in service too: with a single instance,
deregistering denies everyone rather than the few being refused. The backend's
connection ceilings are read at startup and every five minutes, and compared to
`PGMUXD_MAX_CONNECTIONS` in the log, so the number in the job file and the
number in `postgresql.conf` cannot drift apart unnoticed.

### Running it under Nomad

```hcl
task "pgmuxd" {
  driver = "exec"
  config { command = "pgmuxd" }

  artifact {
    source   = "https://your.host/pgmuxd-linux-amd64"
    options { checksum = "sha256:..." }   # from dist/SHA256SUMS
  }

  # Must exceed PGMUXD_DRAIN_TIMEOUT, or the drain is SIGKILLed halfway.
  # Nomad's default is 5s, which is why pgmuxd's drain defaults to 4s.
  kill_timeout = "30s"
  env { PGMUXD_DRAIN_TIMEOUT = "25s" }
}
```

SIGTERM stops accepting, flips `/healthz` to `503` so Consul deregisters first,
then drains open sessions against the deadline. It exits `0` even if the drain
runs out of time — SIGTERM is operator-initiated, and a non-zero exit on every
deploy would feed Nomad's restart and reschedule counters. Non-zero is reserved
for a bad configuration or a listener that will not bind.

The binary is static (`CGO_ENABLED=0`), runs as an unprivileged user, and needs
no capabilities: 5432 is above 1024.

### Tests

```console
$ go test ./...                              # no docker required
$ go test -tags=integration ./cmd/pgmuxd/    # starts a real PostgreSQL
$ PGMUXD_INTEGRATION_STRICT=1 go test -tags=integration ./cmd/pgmuxd/   # CI: fail instead of skip
```

The integration test starts PostgreSQL in a container pinned by digest, runs
`psql` with **no `-U` and no `-d`**, and asserts `current_user` and
`current_database` are the configured ones — plus that `sslmode=disable` is
refused, on both the plain and `gssencmode=prefer` paths. It also SIGTERMs the
proxy mid-query and asserts the query still returns — the drain is the subtlest
thing in the binary, and that is the test guarding it.

## Examples

See `examples/` directory:

- `examples/static/` - Static routing example
- `examples/tls/` - TLS/SSL configuration example

## Protocol Support

- PostgreSQL wire protocol v3.0
- Authentication: SASL-SCRAM-SHA-256, cleartext
- All standard PostgreSQL messages

Please note, MD5 authentication won't work if the username is rewritten by a proxy, because the username is part of the MD5 password hash calculation in PostgreSQL.

## Limitations

- No connection pooling (creates a new backend connection per client)
- No query rewriting or filtering (not planned at this time)
- No rate limiting — `Limits.MaxConnectionsPerIP` caps concurrent connections
  per source address, but nothing throttles connection *attempts*
- No PROXY protocol support, in either direction
- MD5 authentication cannot work through a username rewrite (see Protocol Support)
- TLS to the client is always available; TLS to the backend is opt-in per
  `BackendConfig` (see Security model)

## What is and is not hardened

pgMux began as a development and lab tool. Parts of it have since been hardened
deliberately, because it now runs on a public listener. Rather than a blanket
claim in either direction, here is what has actually been done and what has not.

**Hardened, and covered by tests:**

- **Connection ceiling.** `Limits.MaxConnections` (default 1024) caps concurrent
  connections; over the cap a client gets a `53300` FATAL and a closed socket
  rather than a hang.
- **Message size ceiling.** `Limits.MaxMessageSize` (default 16 MiB) is enforced
  *before* pgMux allocates from an attacker-controlled length prefix.
- **Handshake and idle timeouts.** A client that connects and sends nothing is
  dropped after 10s, before authentication, so it cannot hold a connection slot.
  TLS handshakes are bounded the same way. `Limits.ClientIdleTimeout` bounds an
  established session — it is 0 (disabled) by default; **set it explicitly for a
  public listener.**
- **Connection slots are released on every disconnect path** — clean close,
  reset, idle timeout, or the backend going away — not only on a graceful FIN.
- **No internal detail in pgMux's own client-visible errors.** Routing and dial
  failures send a fixed generic FATAL. The backend host, port and the wrapped Go
  error go to the log, never to the client, and an unroutable username is not
  echoed back — that would confirm which usernames exist.
- **Backend authentication errors are stripped** of PostgreSQL's `Detail`,
  `Hint`, `Where`, `File`, `Line` and `Routine` before being relayed. See the
  caveat below on the message text itself.
- **Panic containment.** Every per-connection goroutine recovers — the handler
  and both relay directions — including around your `WithStartupRewrite` hook.
  One malformed connection cannot take the process down.
- **TLS material is validated at `Start`, and reloaded when it changes.** A bad
  certificate path fails startup instead of every connection. `CertFile` and
  `KeyFile` are re-read when their mtime or size changes, so a renewal takes
  effect without a restart; a version that fails to load is reported once and
  the previous certificate keeps serving. An unauthenticated client cannot
  force a file read and key parse per connection: unchanged files, and a
  broken version already reported, are served from memory.
- **GSSAPI encryption requests are declined cleanly**, so `psql` works for
  visitors holding Kerberos credentials instead of failing with "server closed
  the connection unexpectedly".
- **Startup parameters are not logged at default level**, and log values are
  escaped, so a hostile username cannot forge log records.

**Not hardened — handle these outside pgMux:**

- **No rate limiting or connection throttling.** `Limits.MaxConnectionsPerIP`
  gives `MaxConnections` some fairness — a single source can no longer occupy
  the whole ceiling — but it caps concurrency, not attempt rate, and it is off
  by default. Nothing slows a host that reconnects in a loop, and a distributed
  source defeats it. Use nftables/pf or an equivalent in front of the listener.
- **No authorisation of its own.** pgMux relays authentication; it never decides
  who may connect. Everything about what a visitor can *do* is the backend's
  `pg_hba.conf`, roles and grants. For a public endpoint, that means a
  genuinely read-only role and a database you would not mind being public.
- **No write deadline on the relay path.** A client that issues a large query
  and then stops reading parks a relay goroutine in `Write` indefinitely,
  holding both a connection slot and a real PostgreSQL backend process. Set
  `ClientIdleTimeout`, and bound the backend with `statement_timeout`.
- **The text of a backend authentication error still reaches the client.** The
  structured internal fields are stripped, but the message itself is
  PostgreSQL's. A misconfigured `pg_hba.conf` produces a message naming the
  pgMux host's address as the backend sees it, so get `pg_hba.conf` right before
  going public.
- **`CancelRequest` is forwarded**, so Ctrl-C stops the query. pgMux issues each
  session its own cancel key and keeps the backend's: the key a client holds is
  random, works only through the proxy, and stops working the moment the session
  ends. A cancel for an unknown key is closed silently, exactly as a live one is,
  so it cannot be used to probe for valid keys. A cancel still opens a fresh
  connection and occupies a session slot for as long as the backend dial takes.
  Because the key is synthetic, the PID a client sees is not the backend's:
  `PQbackendPID` and any "find me in `pg_stat_activity` by PID" workflow will
  not match, the same class of caveat as the psql prompt above.
  Set `statement_timeout` on the backend role anyway — it is the backstop for a
  client that vanished instead of cancelling.
- **No protection against slow-read or resource-exhaustion attacks** beyond the
  timeouts and ceilings above.
- **A backend outage holds connection slots.** A failed dial retries three times
  with backoff, occupying a slot for up to ~16s, so an outage plus steady
  traffic can saturate `MaxConnections`.
- **No auditing of query content.** Queries are relayed unparsed.
- **The backend hop is cleartext unless you set `BackendConfig.TLS`.** See
  Security model below — this one matters more than it looks.

Throughput is untuned: pgMux opens a fresh backend connection per client and
relays messages with a goroutine pair. That is fine for lab and showcase
traffic; it is not a pooler and should not be put in front of a busy
application.

## Security model

pgMux terminates client TLS and, unless `BackendConfig.TLS` is set, forwards to the backend over plain TCP. This has consequences a SCRAM-aware operator should be aware of before pointing it at anything trust-sensitive:

- **Backend hop is cleartext by default.** Without `BackendConfig.TLS` set, the pgMux→backend connection carries the full SCRAM-SHA-256 exchange and all subsequent session traffic without encryption. Anyone able to observe that path (host, container network, sidecar, shared LAN) sees credential-equivalent material under SCRAM's threat model. Set `BackendConfig.TLS = &tls.Config{...}` to make pgMux send the PostgreSQL `SSLRequest` to the backend and upgrade the connection; pgMux refuses to proceed if the backend declines SSL rather than silently downgrading.
- **Channel binding is not end-to-end.** Because the backend sees plain TCP, it never advertises `SCRAM-SHA-256-PLUS`. A client that sets `channel_binding=require` will fail to connect; a client that leaves it at libpq's default (`prefer`) silently negotiates `SCRAM-SHA-256` with no binding. The TLS between client and pgMux protects the client↔pgMux hop only — it does not bind the authentication to that session in any way the backend can verify.
- **Trust direction matters.** Do not point pgMux at a backend in a higher trust tier than the host pgMux runs on. In particular, do not use it as a hop in front of managed-database services, production clusters, or any backend whose `pg_authid` you do not want exposed to whoever can read traffic on the pgMux host.

Two shapes fit these constraints. The first is the one pgMux was built for: routing development and lab traffic to ephemeral backends where the auth material is itself disposable. The second is an open, read-only endpoint — every visitor arrives as the same low-privilege role, so there is no per-user secret to protect and the credential exposure above costs nothing. Both work because the auth material is worthless, not because the path is safe. Anything in between — real users, real passwords, a backend you care about — needs `BackendConfig.TLS` set, and even then read the channel-binding note above before relying on it.

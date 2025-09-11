# pgMux - PostgreSQL routing proxy

pgMux is a lightweight PostgreSQL routing proxy designed for development and
testing environments. It routes client connections to different backends based
on username, with full PostgreSQL protocol support and pluggable routing logic.

Originally created to provide routing for [SQL Labs](https://labs.boringsql.com/)

## Features

- Username-based routing to different PostgreSQL backends
- Full PostgreSQL protocol support
- SSL/TLS support for secure client connections
- Pluggable routing via Router interface

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

## Router Interface

Implement your own routing logic:

```go
type Router interface {
    Route(ctx context.Context, username string) (*BackendConfig, error)
}

type BackendConfig struct {
    Host string
    Port int
    User string
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

- **Development use only** - pgMux is not (yet) robust enough for high-throughput production environments
- No connection pooling (creates new backend connection per client)
- No query rewriting or filtering (not planned at this time)
- TLS support is for client connections only (backend connections use plain TCP)

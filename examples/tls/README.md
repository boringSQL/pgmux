# TLS/SSL PostgreSQL Proxy Example

This example demonstrates how to use pgmux with TLS/SSL support for secure client connections.

## Features

- TLS/SSL encryption for incoming client connections
- Support for PostgreSQL's SSL negotiation protocol
- Configurable TLS settings (certificates, minimum TLS version)
- Backward compatibility with non-TLS connections

## Setup

### 1. Generate TLS Certificates

For testing, you can generate self-signed certificates:

```bash
./generate_certs.sh
```

This will create:
- `server.crt` - The server certificate
- `server.key` - The server private key

For production, use certificates from a trusted Certificate Authority.

### 2. Configure Backend PostgreSQL Servers

Update the backend configurations in `main.go` to point to your PostgreSQL servers.

### 3. Run the TLS Proxy

```bash
go run main.go
```

Or with custom certificate paths:

```bash
TLS_CERT=/path/to/cert.pem TLS_KEY=/path/to/key.pem go run main.go
```

To enforce TLS 1.3 minimum:

```bash
TLS_MIN_VERSION=1.3 go run main.go
```

## Connecting to the Proxy

### With SSL/TLS:

```bash
# Require SSL
psql 'host=localhost port=5434 user=app_user dbname=postgres sslmode=require'

# Prefer SSL (use if available)
psql 'host=localhost port=5434 user=app_user dbname=postgres sslmode=prefer'
```

### Without SSL/TLS:

```bash
psql 'host=localhost port=5434 user=app_user dbname=postgres sslmode=disable'
```

## SSL Modes

PostgreSQL clients support various SSL modes:

- `disable` - Never use SSL
- `allow` - Use SSL if server supports it
- `prefer` - Use SSL if server supports it (default)
- `require` - Always use SSL (no certificate verification)
- `verify-ca` - Always use SSL and verify server certificate
- `verify-full` - Always use SSL and verify server certificate matches hostname

## Configuration Options

The TLS configuration supports:

1. **Basic Configuration**: Provide certificate and key file paths
2. **Advanced Configuration**: Use a custom `tls.Config` for fine-grained control

Example with custom TLS config:

```go
tlsConfig := &pgmux.TLSConfig{
    Enabled: true,
    Config: &tls.Config{
        MinVersion: tls.VersionTLS13,
        CipherSuites: []uint16{
            tls.TLS_AES_128_GCM_SHA256,
            tls.TLS_AES_256_GCM_SHA384,
        },
        // Additional TLS settings...
    },
}
```

## Security Considerations

1. Always use valid certificates from a trusted CA in production
2. Keep private keys secure and set appropriate file permissions
3. Consider using TLS 1.2 or higher as minimum version
4. Regularly update certificates before expiration
5. Monitor for SSL/TLS vulnerabilities and update accordingly
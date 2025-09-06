# Static Routing Example

This example demonstrates how to use pgmux with static user-to-backend mappings.

## Configuration

The example creates a `StaticRouter` with hardcoded mappings:

- `app_user` → connects to `postgres@10.0.1.50:5432`
- `readonly_user` → connects to `readonly@10.0.1.51:5432`

## Running

```bash
cd examples/static
go run main.go [listen_address]
```

Default listen address is `:5434`.

## Testing

Connect with psql using one of the mapped usernames:

```bash
# This will connect as 'postgres' user to 10.0.1.50:5432
psql -h localhost -p 5434 -U app_user -d mydb

# This will connect as 'readonly' user to 10.0.1.51:5432  
psql -h localhost -p 5434 -U readonly_user -d mydb
```

The proxy will:
1. Accept the connection from the client
2. Look up the backend configuration for the username
3. Connect to the appropriate backend server with the mapped username
4. Forward authentication and all subsequent traffic

## Customization

To modify the mappings, edit the `createExampleRouter()` function in `main.go`.
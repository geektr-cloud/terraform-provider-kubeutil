# Allocate random local ports on IPv4 loopback only

Every tunnel binds `127.0.0.1:0` and reports the port assigned by the OS. Users cannot select a local port, including through port-forward's `local:remote` spelling. We do not use client-go's `localhost` address expansion because it binds both `127.0.0.1` and `::1` and tolerates partial bind failure; this provider deliberately exposes one unambiguous IPv4 loopback endpoint.

## Consequences

The requested-port grammar accepts `5432`, `postgres`, and `:5432`, but rejects a non-empty local component such as `8888:5432`. This prevents silent disagreement between the port a user requested and the endpoint the provider returned.

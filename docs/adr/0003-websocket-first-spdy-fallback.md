# Prefer WebSocket tunneling with SPDY fallback

The forwarder uses client-go's SPDY-over-WebSocket dialer first and falls back to SPDY on upgrade or HTTPS-proxy failure, matching current kubectl behavior. SPDY-only would be smaller, but HTTP proxies that mishandle SPDY upgrades are a common port-forward failure mode; WebSocket-first avoids a class of environment-specific failures without introducing another module dependency.

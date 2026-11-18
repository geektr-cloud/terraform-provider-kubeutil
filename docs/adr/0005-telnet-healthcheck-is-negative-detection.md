# Make the telnet healthcheck a negative rejection check

The optional `healthcheck { type = "telnet" }` block runs whenever its Terraform instance acquires a tunnel, including a shared tunnel. It connects to the local tunnel without sending application data and watches briefly for forwarding failure. It does not treat a successful local TCP handshake as remote readiness: that handshake only proves the local listener accepted. Since the port-forward protocol has no positive per-connection acknowledgment, the check can report only that no rejection was observed during its bounded window.

## Consequences

With no healthcheck block, acquisition returns as soon as the forwarding stream and listener are ready. A telnet healthcheck can catch immediate remote-port refusal but cannot prove application health, and a delayed rejection can escape its observation window. Other healthcheck types may be added later without changing the no-check default.

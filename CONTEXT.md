# Kubeutil

Kubeutil exposes process-scoped Kubernetes port-forward tunnels to Terraform and OpenTofu configurations. Its public identity is an abstract target port, not the pod currently backing that target.

## Language

**Tunnel**:
A loopback listener and its forwarding path to one abstract target port, owned by a provider process.
_Avoid_: Connection, session, forward

**Target**:
A user-supplied Kubernetes `type/name`, or a bare name meaning `pod/name`, that identifies a source of backing pods.
_Avoid_: Endpoint, pod, resource

**Requested Port**:
The user-supplied remote port spelling: a number, a port name, or either with an empty local prefix such as `:5432`.
_Avoid_: Local port, listening port

**Target Port**:
The numeric container port obtained by resolving a requested port against the target and its current backing pod.
_Avoid_: Requested port, local port

**Resolved Pod**:
The pod currently backing a tunnel. It is private, replaceable connection detail rather than the tunnel's public identity.
_Avoid_: Tunnel identity, endpoint

**Local Port**:
The OS-assigned TCP port on which a tunnel listens on `127.0.0.1`.
_Avoid_: Requested port, remote port

**Host Port**:
The consumer-facing `127.0.0.1:<local-port>` endpoint of a tunnel.
_Avoid_: Remote address, target address

**Tunnel Registry**:
The provider-instance-scoped, reference-counted ownership of tunnels by effective namespace, canonical target, and normalized requested port.
_Avoid_: Cache, global pool

**Keeper**:
The owner of a tunnel's forwarding lifetime, including reconnection and target re-resolution.
_Avoid_: Request context, connection handler

**Default Namespace**:
The namespace fallback supplied by provider configuration, then credential context, then the literal namespace `default`.
_Avoid_: Current namespace, hard-coded namespace

**Pod Drift**:
A change of the resolved pod backing the same target-scoped tunnel, including after reconnection.
_Avoid_: Tunnel replacement, identity change

**Healthcheck**:
An optional check performed whenever a Terraform instance acquires a tunnel, including a shared one. A `telnet` healthcheck is negative rejection detection, not proof that the remote application is healthy.
_Avoid_: Readiness guarantee, liveness probe

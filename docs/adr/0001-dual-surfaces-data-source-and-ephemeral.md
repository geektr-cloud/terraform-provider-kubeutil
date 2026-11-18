# Provide both a data source and an ephemeral resource

A tunnel has a process-scoped random local port, while a data source persists its result in plan and state. We nevertheless provide both `data.kubeutil_tunnel` and `ephemeral.kubeutil_tunnel`: the ephemeral resource has the correct lifecycle and non-persistence semantics, while the data source remains available to consumers that cannot accept ephemeral values.

## Consequences

The data source opens a real tunnel when Terraform reads it, including during planning. Its endpoint is valid only for the command and provider process that read it; a later command, or applying a saved plan in another process, must not reuse it as a live endpoint. Random ports and target re-resolution make persisted data-source results inherently volatile, so the documentation must not imply cross-command stability.

# Keep tunnel identity target-scoped and allow pod drift

A tunnel represents a namespace, target, and requested port—not a particular pod incarnation. The registry key and data-source ID therefore use that stable abstract identity, and no resolved pod or translated target port is exposed in Terraform results. On connection loss the keeper reconnects on the same local port and re-resolves the target, which may select a replacement or different backing pod.

## Consequences

Existing forwarded connections are cut and the listener has downtime while reconnecting. Consumers that require one immutable pod incarnation are outside this abstraction; even an explicit `pod/name` target identifies the pod name, not a permanent pod UID. Pod selection and drift are logged but never become persisted identity.

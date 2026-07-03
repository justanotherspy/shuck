// Package gateway implements the persistent event-delivery service of
// shuck's opt-in self-hosted mode (JUS-88): terminate channel-shim
// WebSockets, authenticate per-user bearer tokens, own PR subscriptions and
// the per-subscriber event buffer, and deliver worker events with
// write-then-push semantics — the buffer row is persisted before any push,
// so a crash never loses an event. The package is pure — the AWS adapters
// live in gateway/awsx and the binary in cmd/shuck-gateway — so the portable
// shuck CLI never links any of it (see docs/V2.md for the compatibility
// contract).
package gateway

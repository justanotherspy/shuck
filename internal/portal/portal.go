// Package portal is the pure core of the shuck v2 token portal (JUS-90): a
// small private web app where a user optionally passes a generic OIDC gate,
// proves control of a GitHub account via the GitHub App's user-authorization
// flow, is validated against the installation (org membership, or account
// ownership for personal installs), and receives a Shuck token shown exactly
// once — only its SHA-256 lands in the gateway token table. All I/O is
// behind interfaces; DynamoDB adapters live in portal/awsx and only
// cmd/shuck-portal links them, never the shuck binary.
package portal

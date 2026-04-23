// Package tls mints a self-signed certificate on first run and loads existing
// cert/key material on subsequent runs. iOS clients pin by SHA-256 fingerprint.
package tls

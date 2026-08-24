//go:build awg_ref_interop

// Package awgref holds a build-tagged (tag: awg_ref_interop) cross-implementation
// compatibility harness. See interop_test.go for the package documentation,
// run instructions and retargeting notes.
//
// The package is deliberately excluded from default builds: it exists only to
// prove wire-format compatibility between nylon's ported AWG 2.0 stack and the
// upstream reference implementation, and must never enter the daemon's import
// graph (go list -deps ./cmd/... stays free of amneziawg-go).
package awgref

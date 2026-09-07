// SPDX-License-Identifier: MPL-2.0

// Package tools pins the documentation generation toolchain. It is a
// separate Go module so that tfplugindocs does not end up in the
// provider's dependency graph.
package tools

//go:generate go tool tfplugindocs generate --provider-dir ..

// SPDX-License-Identifier: AGPL-3.0-only

// Package cmd contains the cobra command definitions for the netbox-provider binary.
package cmd

import "github.com/spf13/cobra"

// BuildInfo carries version metadata injected at build time via -ldflags.
type BuildInfo struct {
	Version      string
	GitCommit    string
	GitTreeState string
	BuildDate    string
}

// NewRootCommand returns the netbox-provider root cobra command.
// It has no RunE — invoking 'netbox-provider' with no subcommand prints help.
func NewRootCommand(info BuildInfo) *cobra.Command {
	root := &cobra.Command{
		Use:   "netbox-provider",
		Short: "Milo control plane controller template",
		// No RunE — 'netbox-provider' with no subcommand prints help.
	}
	root.AddCommand(newOperatorCommand(info))
	return root
}

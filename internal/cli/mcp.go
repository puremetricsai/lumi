package cli

import (
	"context"
	"errors"

	"github.com/puremetricsai/lumi/internal/mcp"
	"github.com/spf13/cobra"
)

// mcpCommand serves the activity index to MCP-capable agents over stdio.
//
// Two things about this command are load-bearing. Cobra must never write usage
// text or an error banner to stdout, because stdout is the JSON-RPC stream —
// hence the explicit silencing, which does not rely on inheriting the root's.
// And an agent launches this process with a bare environment, so path
// resolution has to work from --data-dir or LUMI_HOME alone; both already do,
// through the shared openStore.
func (a *app) mcpCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Serve captured activity to AI agents over MCP (stdio)",
		Long: "Run a Model Context Protocol server on stdin/stdout, exposing search_events,\n" +
			"get_event, and list_apps over the local activity index. Agents launch this\n" +
			"themselves; it is not meant to be run interactively.\n\n" +
			"Only text and metadata are served. No screenshot or audio bytes are ever\n" +
			"returned through this interface.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, paths, err := a.openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			// A cancelled context is how a signal shuts this down; that is a
			// clean exit, not a failure worth printing.
			opts := mcp.Options{Name: "lumi", Version: version, DatabasePath: paths.Database}
			if err := mcp.Serve(cmd.Context(), s, opts); err != nil &&
				!errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		},
	}
}

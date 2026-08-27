package cli

import (
	"context"
	"errors"
	"log/slog"

	"github.com/puremetricsai/lumi/internal/mcp"
	"github.com/puremetricsai/lumi/internal/selfexec"
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
//
// It keeps its own RunE rather than becoming a bare parent like `record`: this
// command *is* the server, and every existing client config invokes it as
// `lumi mcp`. Turning it into a help-printing parent would dump text onto the
// JSON-RPC stream. `setup` is its only subcommand — there is deliberately no
// `mcp start`, no HTTP transport, and no daemon, because the client owns the
// server's process lifecycle.
//
// The client owning the lifecycle is also why this process watches its own
// binary. It is launched once and kept for the whole session, so an upgrade that
// replaces the file on disk changes nothing here — the old image stays mapped
// and keeps answering — and the client will not relaunch it mid-session to fix
// that. Replacing the image in place is what makes an upgrade reach a live
// session without adding the daemon this command refuses to become.
func (a *app) mcpCommand() *cobra.Command {
	cmd := &cobra.Command{
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
			// Diagnostics go to stderr: stdout is the JSON-RPC stream, and the
			// default slog handler would corrupt it.
			logger := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil))
			// A cancelled context is how a signal shuts this down; that is a
			// clean exit, not a failure worth printing.
			opts := mcp.Options{
				Name: "lumi", Version: version,
				DatabasePath: paths.Database,
				Logger:       logger,
			}
			// An agent holds this process for the whole session, so an upgrade
			// would otherwise keep serving the old build until the user restarted
			// the session. Watching the binary lets the process
			// replace itself in place instead. A watcher that cannot be built is
			// not fatal: it costs the upgrade path, not the server.
			if watcher, err := selfexec.NewWatcher(); err != nil {
				logger.Warn("in-place upgrades disabled", "error", err)
			} else {
				opts.BinaryChanged = watcher.Changed
				opts.BinaryExec = func() error { return selfexec.Exec(watcher.Path()) }
			}
			if err := mcp.Serve(cmd.Context(), s, opts); err != nil &&
				!errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		},
	}
	cmd.AddCommand(a.mcpSetupCommand())
	return cmd
}

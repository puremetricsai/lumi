package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/puremetricsai/lumi/internal/config"
	"github.com/puremetricsai/lumi/internal/llamacpp"
)

// llamaCommand groups management of a Lumi-launched llama-server. `lumi ask`
// starts one on demand when the provider is llama.cpp and leaves it running;
// these subcommands report on and stop it.
func (a *app) llamaCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "llama",
		Short: "Manage the local llama-server used by the llama.cpp provider",
	}
	cmd.AddCommand(a.llamaStatusCommand(), a.llamaStopCommand())
	return cmd
}

func (a *app) llamaStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report the local llama-server's reachability and process id",
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := a.paths()
			if err != nil {
				return err
			}
			cfg, err := config.LoadConfig(paths.Config)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			baseURL := cfg.ResolvedLlamaBaseURL()
			if llamacpp.Healthy(cmd.Context(), baseURL) {
				fmt.Fprintf(out, "llama-server: reachable at %s\n", baseURL)
			} else {
				fmt.Fprintf(out, "llama-server: not running at %s\n", baseURL)
			}
			if st, ok := llamacpp.ReadState(paths.LlamaState); ok {
				state := "not running"
				switch {
				case llamacpp.IsServerProcess(st.PID):
					state = "running"
				case processAlive(st.PID):
					// The pid was reused; the recorded server is gone.
					state = "no longer llama-server"
				}
				fmt.Fprintf(out, "launched by lumi: pid %d (%s)\n", st.PID, state)
				// The recorded model is what distinguishes this server from the
				// configured one; `ask` restarts it when they differ.
				if model := strings.TrimSpace(st.Model); model != "" {
					fmt.Fprintf(out, "serving model: %s\n", model)
				} else {
					fmt.Fprintln(out, "serving model: not recorded")
				}
			} else {
				fmt.Fprintln(out, "launched by lumi: none recorded")
			}
			return nil
		},
	}
}

func (a *app) llamaStopCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the llama-server that lumi launched",
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := a.paths()
			if err != nil {
				return err
			}
			st, ok := llamacpp.ReadState(paths.LlamaState)
			if !ok {
				return errors.New("no lumi-launched llama-server recorded")
			}
			// A live pid is not proof of ownership: pids are reused, and the
			// recorded server may have exited long ago.
			if llamacpp.IsServerProcess(st.PID) {
				proc, err := os.FindProcess(st.PID)
				if err != nil {
					return err
				}
				if err := proc.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
					return fmt.Errorf("stop llama-server (pid %d): %w", st.PID, err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "sent SIGTERM to llama-server (pid %d)\n", st.PID)
			} else if processAlive(st.PID) {
				fmt.Fprintf(cmd.OutOrStdout(), "pid %d is no longer llama-server; leaving it alone and clearing the record\n", st.PID)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "llama-server (pid %d) was not running\n", st.PID)
			}
			return llamacpp.RemoveState(paths.LlamaState)
		},
	}
}

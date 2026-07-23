package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
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
			if pid, ok := readLlamaPid(paths.LlamaPid); ok {
				if processAlive(pid) {
					fmt.Fprintf(out, "launched by lumi: pid %d (running)\n", pid)
				} else {
					fmt.Fprintf(out, "launched by lumi: pid %d (not running)\n", pid)
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
			pid, ok := readLlamaPid(paths.LlamaPid)
			if !ok {
				return errors.New("no lumi-launched llama-server recorded")
			}
			if processAlive(pid) {
				proc, err := os.FindProcess(pid)
				if err != nil {
					return err
				}
				if err := proc.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
					return fmt.Errorf("stop llama-server (pid %d): %w", pid, err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "sent SIGTERM to llama-server (pid %d)\n", pid)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "llama-server (pid %d) was not running\n", pid)
			}
			if err := os.Remove(paths.LlamaPid); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return nil
		},
	}
}

// readLlamaPid reads the pid llama-server was launched with, if recorded.
func readLlamaPid(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

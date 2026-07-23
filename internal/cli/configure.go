package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/puremetricsai/lumi/internal/config"
)

func (a *app) configureCommand() *cobra.Command {
	var apiKey, model string
	var show bool
	cmd := &cobra.Command{
		Use:   "configure",
		Short: "Set and persist Lumi configuration (Cerebras API key and model)",
		Long: "Persist configuration to config.json in the data directory so it is\n" +
			"available in future sessions. With no flags, configure prompts\n" +
			"interactively; blank answers keep the current value.",
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

			if show {
				printConfig(out, paths.Config, cfg)
				return nil
			}

			flags := cmd.Flags()
			if flags.Changed("api-key") || flags.Changed("model") {
				if flags.Changed("api-key") {
					cfg.CerebrasAPIKey = strings.TrimSpace(apiKey)
				}
				if flags.Changed("model") {
					cfg.CerebrasModel = strings.TrimSpace(model)
				}
			} else if err := promptConfig(cmd.InOrStdin(), out, &cfg); err != nil {
				return err
			}

			if err := config.SaveConfig(paths.Config, cfg); err != nil {
				return err
			}
			fmt.Fprintf(out, "Saved configuration to %s\n", paths.Config)
			printConfig(out, paths.Config, cfg)
			return nil
		},
	}
	cmd.Flags().StringVar(&apiKey, "api-key", "", "Cerebras API key (skips the interactive prompt)")
	cmd.Flags().StringVar(&model, "model", "", "Cerebras model (skips the interactive prompt)")
	cmd.Flags().BoolVar(&show, "show", false, "print the current configuration and exit")
	return cmd
}

// promptConfig interactively reads values, keeping the current value on a blank
// line.
func promptConfig(in io.Reader, out io.Writer, cfg *config.Config) error {
	reader := bufio.NewReader(in)

	fmt.Fprintf(out, "Cerebras API key [%s] (leave blank to keep): ", maskKey(cfg.CerebrasAPIKey))
	key, err := readKey(in, out, reader)
	if err != nil {
		return err
	}
	if key != "" {
		cfg.CerebrasAPIKey = key
	}

	fmt.Fprintf(out, "Cerebras model [%s] (leave blank to keep): ", cfg.ResolvedModel())
	model, err := readLine(reader)
	if err != nil {
		return err
	}
	if model != "" {
		cfg.CerebrasModel = model
	}
	return nil
}

func readLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// readKey reads the API key without echoing it when the input is a real
// terminal, so the secret is not displayed (and cannot be OCR-indexed by an
// active recorder). Piped input and tests keep the echoed line-reading path.
func readKey(in io.Reader, out io.Writer, reader *bufio.Reader) (string, error) {
	f, ok := in.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return readLine(reader)
	}
	raw, err := term.ReadPassword(int(f.Fd()))
	if err != nil {
		return "", err
	}
	// ReadPassword consumes Enter but does not echo the newline; end the line.
	fmt.Fprintln(out)
	return strings.TrimSpace(string(raw)), nil
}

func printConfig(out io.Writer, path string, cfg config.Config) {
	fmt.Fprintf(out, "Config file: %s\n", path)
	fmt.Fprintf(out, "  Cerebras API key: %s\n", maskKey(cfg.CerebrasAPIKey))
	fmt.Fprintf(out, "  Cerebras model:   %s\n", cfg.ResolvedModel())
}

// maskKey renders an API key without revealing it: whether it is set, plus the
// last four characters as a fingerprint.
func maskKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return "not set"
	}
	if len(key) <= 4 {
		return "set"
	}
	return "set (…" + key[len(key)-4:] + ")"
}

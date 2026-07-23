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
	"github.com/puremetricsai/lumi/internal/llamacpp"
)

func (a *app) configureCommand() *cobra.Command {
	var apiKey, model, provider, llamaModel, llamaBaseURL string
	var show bool
	cmd := &cobra.Command{
		Use:   "configure",
		Short: "Set and persist Lumi configuration (inference provider, keys, and models)",
		Long: "Persist configuration to config.json in the data directory so it is\n" +
			"available in future sessions. With no flags, configure prompts\n" +
			"interactively; blank answers keep the current value. The provider selects\n" +
			"the `ask` inference backend: `cerebras` (hosted, needs an API key) or\n" +
			"`llama.cpp` (local llama-server, must be installed).",
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
			anyFlag := flags.Changed("api-key") || flags.Changed("model") ||
				flags.Changed("provider") || flags.Changed("llama-model") || flags.Changed("llama-base-url")
			if anyFlag {
				if flags.Changed("provider") {
					cfg.Provider = strings.TrimSpace(provider)
				}
				if flags.Changed("api-key") {
					cfg.CerebrasAPIKey = strings.TrimSpace(apiKey)
				}
				if flags.Changed("model") {
					cfg.CerebrasModel = strings.TrimSpace(model)
				}
				if flags.Changed("llama-model") {
					cfg.LlamaModel = strings.TrimSpace(llamaModel)
				}
				if flags.Changed("llama-base-url") {
					cfg.LlamaBaseURL = strings.TrimSpace(llamaBaseURL)
				}
			} else if err := promptConfig(cmd.InOrStdin(), out, &cfg); err != nil {
				return err
			}

			// Reject an unrecognized provider before persisting so a typo
			// surfaces immediately instead of silently behaving as Cerebras.
			if !config.KnownProvider(cfg.Provider) {
				return fmt.Errorf("unknown provider %q; valid providers are %s and %s", strings.TrimSpace(cfg.Provider), config.ProviderCerebras, config.ProviderLlamaCpp)
			}

			// Store the provider in its canonical form so config.json is tidy.
			if strings.TrimSpace(cfg.Provider) != "" {
				cfg.Provider = cfg.ResolvedProvider()
			}
			// Refuse to select llama.cpp unless llama-server is installed; the
			// backend cannot work without it.
			if cfg.ResolvedProvider() == config.ProviderLlamaCpp {
				if _, ok := llamacpp.Installed(); !ok {
					return fmt.Errorf("llama-server not found on PATH; install llama.cpp (brew install llama.cpp) — https://github.com/ggml-org/llama.cpp")
				}
			}

			if err := config.SaveConfig(paths.Config, cfg); err != nil {
				return err
			}
			fmt.Fprintf(out, "Saved configuration to %s\n", paths.Config)
			printConfig(out, paths.Config, cfg)
			return nil
		},
	}
	cmd.Flags().StringVar(&provider, "provider", "", "inference provider: cerebras or llama.cpp")
	cmd.Flags().StringVar(&apiKey, "api-key", "", "Cerebras API key (skips the interactive prompt)")
	cmd.Flags().StringVar(&model, "model", "", "Cerebras model (skips the interactive prompt)")
	cmd.Flags().StringVar(&llamaModel, "llama-model", "", "llama.cpp model: a GGUF file path or a HuggingFace repo id")
	cmd.Flags().StringVar(&llamaBaseURL, "llama-base-url", "", "llama-server base URL (default "+config.DefaultLlamaBaseURL+")")
	cmd.Flags().BoolVar(&show, "show", false, "print the current configuration and exit")
	return cmd
}

// promptConfig interactively reads values, keeping the current value on a blank
// line. It prompts for the provider first, then only that provider's fields.
func promptConfig(in io.Reader, out io.Writer, cfg *config.Config) error {
	reader := bufio.NewReader(in)

	fmt.Fprintf(out, "Inference provider [%s] (cerebras or llama.cpp, blank to keep): ", cfg.ResolvedProvider())
	if entered, err := readLine(reader); err != nil {
		return err
	} else if entered != "" {
		cfg.Provider = entered
	}

	if cfg.ResolvedProvider() == config.ProviderLlamaCpp {
		fmt.Fprintf(out, "llama.cpp model [%s] (GGUF path or HuggingFace repo, blank to keep): ", orNotSet(cfg.ResolvedLlamaModel()))
		if entered, err := readLine(reader); err != nil {
			return err
		} else if entered != "" {
			cfg.LlamaModel = entered
		}

		fmt.Fprintf(out, "llama-server base URL [%s] (blank to keep): ", cfg.ResolvedLlamaBaseURL())
		if entered, err := readLine(reader); err != nil {
			return err
		} else if entered != "" {
			cfg.LlamaBaseURL = entered
		}
		return nil
	}

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
	provider := cfg.ResolvedProvider()
	fmt.Fprintf(out, "Config file: %s\n", path)
	fmt.Fprintf(out, "  Provider: %s\n", provider)
	if provider == config.ProviderLlamaCpp {
		fmt.Fprintf(out, "  llama.cpp model:    %s\n", orNotSet(cfg.ResolvedLlamaModel()))
		fmt.Fprintf(out, "  llama-server URL:   %s\n", cfg.ResolvedLlamaBaseURL())
		return
	}
	fmt.Fprintf(out, "  Cerebras API key: %s\n", maskKey(cfg.CerebrasAPIKey))
	fmt.Fprintf(out, "  Cerebras model:   %s\n", cfg.ResolvedModel())
}

func orNotSet(s string) string {
	if strings.TrimSpace(s) == "" {
		return "not set"
	}
	return s
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

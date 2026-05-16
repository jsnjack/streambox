package cmd

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"streambox/internal/config"

	"github.com/spf13/cobra"
)

func init() {
	configCmd := &cobra.Command{
		Use:   "config",
		Short: "Manage streambox configuration",
	}

	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Write a default config to <user-config-dir>/streambox/config.toml",
		RunE:  runConfigInit,
	}
	initCmd.Flags().BoolP("force", "f", false, "Overwrite an existing config file")

	configCmd.AddCommand(initCmd)
	rootCmd.AddCommand(configCmd)
}

func runConfigInit(cmd *cobra.Command, args []string) error {
	force, ferr := cmd.Flags().GetBool("force")
	if ferr != nil {
		slog.Log(cmd.Context(), LevelTrace, "get flag force", "err", ferr)
	}

	cfgRoot, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("resolving user config dir: %w", err)
	}

	dir := filepath.Join(cfgRoot, "streambox")
	path := filepath.Join(dir, "config.toml")

	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("config already exists at %s\nuse --force to overwrite", path)
		}
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	if err := os.WriteFile(path, []byte(config.DefaultConfig), 0o644); err != nil {
		return fmt.Errorf("writing config file: %w", err)
	}

	fmt.Printf("Config written to %s\nEdit media_dir then run:\n  streambox --config %s\n", path, path)
	return nil
}

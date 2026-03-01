package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"streambox/internal/config"
)

func init() {
	configCmd := &cobra.Command{
		Use:   "config",
		Short: "Manage streambox configuration",
	}

	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Write a default config to ~/.config/streambox/config.toml",
		RunE:  runConfigInit,
	}
	initCmd.Flags().BoolP("force", "f", false, "Overwrite an existing config file")

	configCmd.AddCommand(initCmd)
	rootCmd.AddCommand(configCmd)
}

func runConfigInit(cmd *cobra.Command, args []string) error {
	force, _ := cmd.Flags().GetBool("force")

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}

	dir := filepath.Join(home, ".config", "streambox")
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

	fmt.Printf("Config written to %s\nEdit media_dir then run:\n  streambox serve --config %s\n", path, path)
	return nil
}

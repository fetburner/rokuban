package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/fetburner/rokuban/internal/config"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Configuration utilities",
	}

	cmd.AddCommand(newConfigValidateCmd())

	return cmd
}

func newConfigValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Validate config file",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := cmd.Flags().GetString("config")
			if err != nil {
				return err
			}
			_, err = config.Load(path)
			if err != nil {
				return err
			}
			fmt.Println("config is valid")
			return nil
		},
	}
}

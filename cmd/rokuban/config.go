package main

import (
	"fmt"

	"github.com/spf13/cobra"
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
			_, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			fmt.Println("config is valid")
			return nil
		},
	}
}

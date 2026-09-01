package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "rokuban: %v\n", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "rokuban",
		Short:         "Cloud-native recording server",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().String("config", "config.yml", "path to config file")

	root.AddCommand(newCatalogCmd())
	root.AddCommand(newConfigCmd())
	root.AddCommand(newEnqueueCmd())
	root.AddCommand(newImportCmd())
	root.AddCommand(newMigrateCmd())
	root.AddCommand(newRescueCmd())
	root.AddCommand(newServerCmd())
	root.AddCommand(newShadowDiffCmd())

	return root
}

/*
Copyright © 2022 Morgan Gangwere <morgan.gangwere@gmail.com>
*/
package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "parc",
	Short: "Parc is a reference Ponzu ARChive tool",
	Long: `Parc is a reference implementation of the Ponzu Archive format.

	`,
	DisableAutoGenTag: true,
	// Uncomment the following line if your bare application
	// has an action associated with it:
	// Run: func(cmd *cobra.Command, args []string) { },
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

// this needs to get good.
// There's a lot of stuff that isn't really doable with cobra's current doc platform. I want to see some sort
// of templating available. Somehow.
func GenDocs() {

	docdir := "./docs/content/docs/parc"

	if err := os.Mkdir(docdir, 0775); err != nil && err != os.ErrExist {
		if errors.Is(err, os.ErrExist) {
			fmt.Println("Docs folder already exists, OK.")
		} else {
			fmt.Println("failed to make dir:", err)
			return
		}
	}
	fmt.Println("Generating markdown")
	err := doc.GenMarkdownTree(rootCmd, docdir)
	if err != nil {
		fmt.Println("failed to make docs:", err)
	}

}

func init() {
	rootCmd.PersistentFlags().BoolP("verbose", "v", false, "Write detailed information to the terminal")
}

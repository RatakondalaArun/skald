package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"skald/internal/config"
)

func newInitCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Create the skald data directory and database (safe to repeat)",
		Long: `Creates $SKALD_HOME (default ~/.skald) and the SQLite database, and applies
any pending migrations. Running it again changes nothing.`,
		Args: noArgs,
		RunE: a.do(func(e *env, c *cobra.Command, _ []string) error {
			home, err := config.Home()
			if err != nil {
				return err
			}
			v, err := e.db.SchemaVersion(e.ctx)
			if err != nil {
				return err
			}
			out := struct {
				Home          string `json:"home"`
				Database      string `json:"database"`
				SchemaVersion int    `json:"schema_version"`
			}{home, e.db.Path(), v}
			return e.emit(out, func(w io.Writer) {
				fmt.Fprintf(w, "skald is ready: %s (schema v%d)\n", e.db.Path(), v)
			})
		}),
	}
}

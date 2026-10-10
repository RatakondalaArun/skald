package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"skald/internal/config"
)

func newBackupCmd(a *app) *cobra.Command {
	var out string
	c := &cobra.Command{
		Use:   "backup",
		Short: "Write a consistent copy of the database",
		Long: `Writes a consistent snapshot of the database with SQLite's VACUUM INTO. It is
safe to run while skald mcp or skald serve is using the database. By default
the copy goes to $SKALD_HOME/backups/skald-<timestamp>.db.`,
		Args: noArgs,
		RunE: a.do(func(e *env, c *cobra.Command, _ []string) error {
			dest := out
			if dest == "" {
				home, err := config.Home()
				if err != nil {
					return err
				}
				dir := filepath.Join(home, "backups")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return err
				}
				dest = uniqueBackupPath(dir, time.Now().UTC())
			} else if _, err := os.Stat(dest); err == nil {
				return usagef("%s already exists; choose another --out", dest)
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
				return err
			}
			// VACUUM cannot run inside a transaction; the writer pool is one
			// connection, so this also waits for any write in progress.
			if _, err := e.db.Writer().ExecContext(e.ctx, "VACUUM INTO ?", dest); err != nil {
				return fmt.Errorf("backup failed: %w", err)
			}
			if err := os.Chmod(dest, 0o600); err != nil {
				return err
			}
			st, err := os.Stat(dest)
			if err != nil {
				return err
			}
			res := struct {
				Path  string `json:"path"`
				Bytes int64  `json:"bytes"`
			}{dest, st.Size()}
			return e.emit(res, func(w io.Writer) { fmt.Fprintf(w, "backup written to %s (%d bytes)\n", dest, st.Size()) })
		}),
	}
	c.Flags().StringVar(&out, "out", "", "write the backup to this file instead of the default directory")
	return c
}

// uniqueBackupPath names a backup after the time, adding a counter if two
// backups land in the same second.
func uniqueBackupPath(dir string, now time.Time) string {
	base := "skald-" + now.Format("20060102T150405Z")
	p := filepath.Join(dir, base+".db")
	for i := 1; ; i++ {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			return p
		}
		p = filepath.Join(dir, fmt.Sprintf("%s-%d.db", base, i))
	}
}

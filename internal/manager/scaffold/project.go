// Package scaffold creates the ephemeral directory layout arcmux needs for
// a project's substrate state.
//
// Vault-side scaffolding (role libraries, project subtrees) is now an
// elonco concern. arcmux only knows about ~/data/arcmux/<project>/.
package scaffold

import (
	"fmt"
	"os"

	"github.com/lin-labs/arcmux/internal/manager/paths"
)

// Project scaffolds the per-project ephemeral layout. Existing dirs are
// untouched. The function is idempotent and safe to call repeatedly.
//
// Note: arcmux no longer materializes anything under the vault. Callers
// (elonco, etc.) own vault-side artifacts including mission docs, role
// files, and any per-project documentation tree.
func Project(p paths.Project) error {
	if p.Project == "" {
		return fmt.Errorf("paths.Project not populated")
	}
	dirs := []string{
		p.EphemeralRoot, p.Scratchpads, p.ConsultInbox, p.Heartbeats,
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	return nil
}

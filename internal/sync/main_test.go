package sync

import (
	"os"
	"testing"

	"github.com/wow-look-at-my/github-state-mirror/internal/database"
)

// Every fetch, replay and consistency check in this package's tests runs against a real store.
func TestMain(m *testing.M) { os.Exit(database.RunWithTxGuard(m)) }

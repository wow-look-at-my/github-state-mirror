package notify

import (
	"os"
	"testing"

	"github.com/wow-look-at-my/github-state-mirror/internal/database"
)

// Subscriber deliveries write their outcome to the subscriptions DB.
func TestMain(m *testing.M) { os.Exit(database.RunWithTxGuard(m)) }

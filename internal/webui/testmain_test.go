package webui

import (
	"os"
	"testing"

	"github.com/maniack/tgaia/internal/storage"
)

func TestMain(m *testing.M) {
	storage.CoalescingDelay = 0
	os.Exit(m.Run())
}

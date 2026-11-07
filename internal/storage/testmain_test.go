package storage

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Disable coalescing in storage-package tests so Accept enqueues jobs
	// immediately and Claim can pick them up without waiting 3 s.
	// Tests that specifically verify coalescing behaviour set it themselves.
	CoalescingDelay = 0
	os.Exit(m.Run())
}

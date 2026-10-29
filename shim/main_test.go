package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bpalermo/redoma/common/sockettest"
)

// TestListenBridge: the bridge's socket replaces a stale one and is the
// owner's alone.
func TestListenBridge(t *testing.T) {
	path := filepath.Join(sockettest.Dir(t), "shim.sock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := listenBridge(path)
	sockettest.SkipIfRefused(t, err)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want a socket with 0600", fi.Mode())
	}
}

package main

import (
	"testing"

	"github.com/miwidot/darkbot/internal/config"
)

func TestHideOnClose(t *testing.T) {
	cfg, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &App{cfg: cfg}
	if a.hideOnClose(false) {
		t.Fatal("without a tray icon the X must quit, or darkbot would be unreachable")
	}
	if !a.hideOnClose(true) {
		t.Fatal("default with a tray: keep running")
	}
	if err := cfg.Update(func(c *config.Config) { c.QuitOnClose = true }); err != nil {
		t.Fatal(err)
	}
	if a.hideOnClose(true) {
		t.Fatal("QuitOnClose must quit")
	}
	if err := cfg.Update(func(c *config.Config) { c.QuitOnClose = false }); err != nil {
		t.Fatal(err)
	}
	a.tray.quitting.Store(true)
	if a.hideOnClose(true) {
		t.Fatal("Quit from the tray menu must close")
	}
}

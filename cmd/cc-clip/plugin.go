package main

import (
	"log"
	"os"

	"github.com/shunmei/cc-clip/internal/plugin"
)

// cmdPlugin dispatches `cc-clip plugin run <name>`.
func cmdPlugin() {
	if len(os.Args) < 3 || os.Args[2] != "run" {
		log.Fatal("usage: cc-clip plugin run <name>")
	}
	if len(os.Args) < 4 {
		log.Fatal("usage: cc-clip plugin run <name>")
	}
	name := os.Args[3]
	port := getPort()
	// Everything after the adapter name belongs to the adapter. Codex appends
	// its event JSON there instead of writing it to stdin, so dropping these
	// arguments silently swallowed every Codex notification.
	args := os.Args[4:]
	if err := plugin.RunWithArgs(name, port, args, os.Stdin, os.Stdout); err != nil {
		// Fail-soft: hook contexts must never see a non-zero exit. The known
		// notify adapters already return nil; this guards the unknown-adapter
		// path so a misconfigured hook still exits 0.
		log.Printf("plugin run failed: %v", err)
	}
}

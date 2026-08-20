package cli

import (
	"centra/protocol/codegraph-ultra/internal/store"
	"centra/protocol/codegraph-ultra/internal/tui"
)

func launchTUI(db *store.DB) (runner, error) {
	return &tuiRunner{db: db}, nil
}

type tuiRunner struct {
	db *store.DB
}

func (r *tuiRunner) Run() error {
	return tui.Run(r.db)
}

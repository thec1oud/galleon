package datatest_test

import (
	"encoding/json"
	"testing"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/pg"
	"github.com/thec1oud/galleon/testkit/conformance/datatest"
)

// pgOptions installs package pg as the suite asks.
func pgOptions(t *testing.T, s datatest.Setup) []galleon.Option {
	leaks := "error"
	if s.LogLeaks {
		leaks = "log"
	}
	cfg, err := json.Marshal(map[string]any{"pg": map[string]any{
		"primary":       map[string]any{"dsn": s.DSN, "max_conns": 4},
		"named":         map[string]any{"other": map[string]any{"dsn": s.DSN, "max_conns": 2}},
		"leaks":         leaks,
		"retry_backoff": 1_000_000, // 1ms
	}})
	if err != nil {
		t.Fatal(err)
	}
	return []galleon.Option{galleon.Modules(pg.Module()), galleon.ConfigJSON(cfg)}
}

func TestConformance(t *testing.T) {
	datatest.Run(t, datatest.Factory{Options: pgOptions})
}

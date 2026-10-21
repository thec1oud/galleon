package migratetest_test

import (
	"testing"

	"github.com/thec1oud/galleon/migrate"
	"github.com/thec1oud/galleon/testkit"
	"github.com/thec1oud/galleon/testkit/conformance/migratetest"
)

// fakeBuilds are the suite's builds as fake steps.
func fakeBuilds(b migratetest.Build) []testkit.FakeStep {
	one := testkit.FakeStep{ID: "1", Risk: migrate.Safe, Content: "create a"}
	two := testkit.FakeStep{ID: "2", Risk: migrate.Safe, Content: "create b"}
	switch b {
	case migratetest.Next:
		return []testkit.FakeStep{one, two, {ID: "3", Risk: migrate.Safe, Content: "create c"}}
	case migratetest.Risky:
		return []testkit.FakeStep{one, two, {ID: "3", Risk: migrate.Destructive, Content: "drop a"}}
	case migratetest.Last:
		return []testkit.FakeStep{one, two, {ID: "3", Risk: migrate.Safe, Content: "create c"}, {ID: "4", Risk: migrate.Safe, Content: "create d"}}
	case migratetest.Edited:
		one.Content = "create a differently"
		return []testkit.FakeStep{one, two}
	}
	return []testkit.FakeStep{one, two}
}

func TestTheFakeMigratorConforms(t *testing.T) {
	migratetest.Run(t, migratetest.Factory{
		Open: func(t *testing.T) func(migratetest.Build) migrate.Migrator {
			store := testkit.NewMigrationStore()
			return func(b migratetest.Build) migrate.Migrator {
				m := store.Migrator(migratetest.Name, fakeBuilds(b)...)
				m.Tasks = []testkit.FakeTask{{ID: "backfill", Units: 5}}
				return m
			}
		},
		Ahead: true, Drift: true, Fencing: true,
	})
}

func TestTheFakeMigratorConformsWithTheCoreAlone(t *testing.T) {
	migratetest.Run(t, migratetest.Factory{
		Open: func(t *testing.T) func(migratetest.Build) migrate.Migrator {
			store := testkit.NewMigrationStore()
			return func(b migratetest.Build) migrate.Migrator {
				return testkit.CoreOnly(store.Migrator(migratetest.Name, fakeBuilds(b)...))
			}
		},
		Ahead: true, Drift: true, Fencing: true,
	})
}

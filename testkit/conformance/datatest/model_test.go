package datatest_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/data"
	"github.com/thec1oud/galleon/testkit"
	"github.com/thec1oud/galleon/testkit/conformance/datatest"
)

// modelSchema creates a schema of the package's database holding a table
// "items", committed, and drops it when t ends.
func modelSchema(t *testing.T, dsn string) string {
	t.Helper()
	var b [6]byte
	_, _ = rand.Read(b[:])
	schema := "models_" + hex.EncodeToString(b[:])
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+schema+"; CREATE TABLE "+schema+".items (id int PRIMARY KEY, v text)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("dropping %s: %v", schema, err)
		}
		pool.Close()
	})
	return schema
}

// models is a module that registers models, as an ORM adapter's does.
func models(ms ...data.ModelInfo) galleon.Module {
	return galleon.ModuleFunc("orm", func(r *galleon.Registry) error {
		galleon.Collect(r, ms...)
		return nil
	})
}

// modelSpans runs a statement, a batch and a copy marked with model, and
// one of each unmarked, and returns the spans of each kind, marked first.
func modelSpans(t *testing.T, m data.Manager, tp *datatest.Recorder, schema, model string) map[string][2]*datatest.Span {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	marked := data.ForModel(c, model)
	for i, ctx := range []context.Context{marked, c} {
		err := m.Write(ctx, func(ctx context.Context, w data.WriteTx) error {
			if _, err := w.Exec(ctx, "INSERT INTO "+schema+".items (id, v) VALUES ($1, 'statement')", 10+i); err != nil {
				return err
			}
			b := &data.Batch{}
			b.Queue("SELECT count(*) FROM " + schema + ".items")
			if err := w.SendBatch(ctx, b).Close(); err != nil {
				return err
			}
			if _, err := w.CopyFrom(ctx, data.Identifier{schema, "items"}, []string{"id", "v"}, pgx.CopyFromRows([][]any{{20 + i, "copied"}})); err != nil {
				return err
			}
			// Savepoints released and rolled back.
			for _, end := range []func(data.WriteTx) error{func(sp data.WriteTx) error { return sp.Commit(ctx) }, func(sp data.WriteTx) error { return sp.Rollback(ctx) }} {
				sp, err := w.BeginWrite(ctx)
				if err != nil {
					return err
				}
				if err := end(sp); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	out := map[string][2]*datatest.Span{}
	for _, kind := range []string{"INSERT", "BATCH", "COPY"} {
		found := tp.Find(func(s *datatest.Span) bool { return strings.HasPrefix(s.Name(), kind) })
		if len(found) != 2 {
			t.Fatalf("%d %s spans among %v", len(found), kind, tp.Names())
		}
		out[kind] = [2]*datatest.Span{found[0], found[1]}
	}
	return out
}

func TestStatementSpansNameTheModelAndItsTable(t *testing.T) {
	db := testkit.DB(t)
	schema := modelSchema(t, db.DSN())
	tp := &datatest.Recorder{}
	item := data.ModelInfo{Name: "item", Schema: schema, Table: "items", Key: []string{"id"}}
	h := testkit.New(t, append(pgOptions(t, datatest.Setup{DSN: db.DSN()}), galleon.TracerProvider(tp), galleon.Modules(models(item)))...)
	if got, ok := testkit.Get[*data.Models](h).Lookup("item"); !ok || got.Table != "items" {
		t.Fatalf("the registry has %+v", got)
	}
	spans := modelSpans(t, testkit.Get[data.Manager](h), tp, schema, "item")
	table := data.Identifier{schema, "items"}.Sanitize()
	for kind, s := range spans {
		if got := s[0].Attr("galleon.data.model"); got != "item" {
			t.Errorf("%s: the model is %q", kind, got)
		}
		if got := s[0].Attr("db.collection.name"); got != table {
			t.Errorf("%s: the collection is %q, want %s", kind, got, table)
		}
		if got := s[1].Attr("galleon.data.model"); got != "" {
			t.Errorf("%s without a model: the model is %q", kind, got)
		}
		if got, want := s[1].Attr("db.collection.name"), map[bool]string{true: table}[kind == "COPY"]; got != want {
			t.Errorf("%s without a model: the collection is %q, want %q", kind, got, want)
		}
	}
	for _, name := range []string{"BEGIN", "SAVEPOINT", "RELEASE", "ROLLBACK"} {
		control := tp.Find(func(s *datatest.Span) bool { return s.Name() == name })
		if len(control) == 0 {
			t.Fatalf("no %s span among %v", name, tp.Names())
		}
		for _, s := range control {
			if s.Attr("galleon.data.model") != "" || s.Attr("db.collection.name") != "" {
				t.Errorf("a %s span names the model or its table: %q, %q", name, s.Attr("galleon.data.model"), s.Attr("db.collection.name"))
			}
		}
	}
}

func TestAModelThatIsntRegisteredIsNamedWithoutATable(t *testing.T) {
	db := testkit.DB(t)
	schema := modelSchema(t, db.DSN())
	tp := &datatest.Recorder{}
	h := testkit.New(t, append(pgOptions(t, datatest.Setup{DSN: db.DSN()}), galleon.TracerProvider(tp))...)
	spans := modelSpans(t, testkit.Get[data.Manager](h), tp, schema, "ghost")
	for _, kind := range []string{"INSERT", "BATCH"} {
		if s := spans[kind][0]; s.Attr("galleon.data.model") != "ghost" || s.Attr("db.collection.name") != "" {
			t.Errorf("%s: model %q, collection %q", kind, s.Attr("galleon.data.model"), s.Attr("db.collection.name"))
		}
	}
}

func TestACopyKeepsItsOwnTableWhateverTheModels(t *testing.T) {
	db := testkit.DB(t)
	schema := modelSchema(t, db.DSN())
	tp := &datatest.Recorder{}
	elsewhere := data.ModelInfo{Name: "item", Schema: "archive", Table: "old_items", Key: []string{"id"}}
	h := testkit.New(t, append(pgOptions(t, datatest.Setup{DSN: db.DSN()}), galleon.TracerProvider(tp), galleon.Modules(models(elsewhere)))...)
	spans := modelSpans(t, testkit.Get[data.Manager](h), tp, schema, "item")
	if got := spans["COPY"][0].Attr("db.collection.name"); got != (data.Identifier{schema, "items"}).Sanitize() {
		t.Fatalf("the copy's collection is %q", got)
	}
	if got := spans["INSERT"][0].Attr("db.collection.name"); got != `"archive"."old_items"` {
		t.Fatalf("the statement's collection is %q", got)
	}
}

func TestAManagerGivenToTheModuleNamesTheAppsModels(t *testing.T) {
	db := testkit.DB(t)
	tp := &datatest.Recorder{}
	item := data.ModelInfo{Name: "item", Schema: "shop", Table: "items", Key: []string{"id"}}
	h := testkit.New(t, galleon.TracerProvider(tp), galleon.Modules(db.Module(), models(item)))
	err := testkit.Get[data.Manager](h).Read(data.ForModel(context.Background(), "item"), func(ctx context.Context, r data.ReadTx) error {
		var n int
		return r.QueryRow(ctx, "SELECT 1").Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	found := tp.Find(func(s *datatest.Span) bool { return s.Name() == "SELECT" })
	if len(found) != 1 || found[0].Attr("galleon.data.model") != "item" || found[0].Attr("db.collection.name") != `"shop"."items"` {
		t.Fatalf("spans %v", tp.Names())
	}
}

func TestDuplicateModelsFailStart(t *testing.T) {
	db := testkit.DB(t)
	item := data.ModelInfo{Name: "item", Schema: "shop", Table: "items", Key: []string{"id"}}
	other := item
	other.Name = "other"
	for want, ms := range map[string][]data.ModelInfo{
		`model "item" is registered twice`:                            {item, item},
		`models "item" and "other" are both stored in "shop"."items"`: {item, other},
	} {
		app, err := galleon.New(append(pgOptions(t, datatest.Setup{DSN: db.DSN()}), galleon.Name("models"), galleon.Coordination(local.New()),
			galleon.Deploy(galleon.SingleNode), galleon.Modules(models(ms...)))...)
		if err != nil {
			t.Fatal(err)
		}
		err = app.Start(context.Background())
		if err == nil {
			_ = app.Shutdown(context.Background())
		}
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "*data.Models") {
			t.Errorf("%s: got %v", want, err)
		}
	}
}

func TestAnInvalidModelFailsBootNamingItsModule(t *testing.T) {
	db := testkit.DB(t)
	_, err := galleon.New(append(pgOptions(t, datatest.Setup{DSN: db.DSN()}), galleon.Name("models"), galleon.Coordination(local.New()),
		galleon.Deploy(galleon.SingleNode), galleon.Modules(models(data.ModelInfo{Name: "x", Schema: "s", Table: "t"})))...)
	if err == nil || !strings.Contains(err.Error(), `module "orm" (`) || !strings.Contains(err.Error(), `model "x": it has no primary key columns`) {
		t.Fatalf("got %v", err)
	}
}

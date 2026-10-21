package ormtest

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thec1oud/galleon/data"
)

// ownPool runs every statement on a pool of its own, from the DSN it was
// configured with, ignoring the handles it is given.
type ownPool struct {
	*Reference
	pool *pgxpool.Pool
}

func newOwnPool(t *testing.T, s Setup) *ownPool {
	pool, err := pgxpool.New(context.Background(), s.DSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &ownPool{Reference: NewReference(s), pool: pool}
}

func (a *ownPool) Insert(ctx context.Context, _ data.Querier, w Widget) error {
	return a.Reference.Insert(ctx, poolQuerier{a.pool}, w)
}

func (a *ownPool) Find(ctx context.Context, _ data.Reader, id int64) (Widget, error) {
	return a.Reference.Find(ctx, poolQuerier{a.pool}, id)
}

func (a *ownPool) Update(ctx context.Context, _ data.Querier, w Widget) error {
	return a.Reference.Update(ctx, poolQuerier{a.pool}, w)
}

// readsElsewhere writes through the handle, but reads on a pool of its own.
type readsElsewhere struct{ *ownPool }

func (a readsElsewhere) Insert(ctx context.Context, q data.Querier, w Widget) error {
	return a.Reference.Insert(ctx, q, w)
}

func (a readsElsewhere) Update(ctx context.Context, q data.Querier, w Widget) error {
	return a.Reference.Update(ctx, q, w)
}

// poolQuerier is a pool as a data.Querier.
type poolQuerier struct{ *pgxpool.Pool }

func (p poolQuerier) Exec(ctx context.Context, sql string, args ...any) (data.CommandTag, error) {
	return p.Pool.Exec(ctx, sql, args...)
}

// inline runs the side effect at once, as if the change had committed.
type inline struct{ *Reference }

func (a inline) Insert(ctx context.Context, q data.Querier, w Widget) error {
	if _, err := q.Exec(ctx, "INSERT INTO "+a.table()+" (id, name, count) VALUES ($1, $2, $3)", w.ID, w.Name, w.Count); err != nil {
		return err
	}
	a.s.Saved(ctx, w)
	return nil
}

func (a inline) Update(ctx context.Context, q data.Querier, w Widget) error {
	if _, err := q.Exec(ctx, "UPDATE "+a.table()+" SET name = $2, count = $3 WHERE id = $1", w.ID, w.Name, w.Count); err != nil {
		return err
	}
	a.s.Saved(ctx, w)
	return nil
}

// rollbackToo registers the side effect after a rollback as well as after
// the commit.
type rollbackToo struct{ *Reference }

func (a rollbackToo) Insert(ctx context.Context, q data.Querier, w Widget) error {
	if err := a.Reference.Insert(ctx, q, w); err != nil {
		return err
	}
	q.(data.WriteTx).AfterRollback(func(ctx context.Context) { a.s.Saved(ctx, w) })
	return nil
}

// connectsPerCall reads on a connection of its own, which it closes again.
type connectsPerCall struct{ *Reference }

func (a connectsPerCall) Find(ctx context.Context, _ data.Reader, id int64) (Widget, error) {
	conn, err := pgx.Connect(ctx, a.s.DSN)
	if err != nil {
		return Widget{}, err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	var w Widget
	err = conn.QueryRow(ctx, "SELECT id, name, count FROM "+a.table()+" WHERE id = $1", id).Scan(&w.ID, &w.Name, &w.Count)
	return w, err
}

// models registers the models it is given.
type models struct {
	*Reference
	edit func(ms []data.ModelInfo) []data.ModelInfo
}

func (a models) Models() []data.ModelInfo { return a.edit(a.Reference.Models()) }

// marks marks a statement for a model it doesn't register.
type marks struct{ *Reference }

func (a marks) Insert(ctx context.Context, q data.Querier, w Widget) error {
	if err := a.Reference.Insert(ctx, q, w); err != nil {
		return err
	}
	_, err := q.Exec(data.ForModel(ctx, "gadget"), "SELECT 1")
	return err
}

func TestTheSuiteCatchesBadAdapters(t *testing.T) {
	for _, c := range []struct {
		name  string
		new   func(t *testing.T, s Setup) Adapter
		check func(e *env) error
		want  []string
	}{
		{
			name: "AnAdapterWithAPoolOfItsOwnBypassesTheHandle", check: statementsGoThroughTheHandle,
			new:  func(t *testing.T, s Setup) Adapter { return newOwnPool(t, s) },
			want: []string{"Insert ran no statement through the handle", "the transaction rolled back, yet {ID:1 Name:first Count:1} is committed"},
		},
		{
			name: "AnAdapterWithAPoolOfItsOwnHasConnections", check: noConnectionOfItsOwn,
			new:  func(t *testing.T, s Setup) Adapter { return newOwnPool(t, s) },
			want: []string{"connection(s) of its own"},
		},
		{
			name: "AnAdapterThatReadsElsewhereDoesntSeeItsTransaction", check: statementsGoThroughTheHandle,
			new:  func(t *testing.T, s Setup) Adapter { return readsElsewhere{newOwnPool(t, s)} },
			want: []string{"the widget it inserted in the same transaction, uncommitted: Find", "no rows"},
		},
		{
			name: "AnAdapterThatReadsElsewhereHasConnections", check: noConnectionOfItsOwn,
			new:  func(t *testing.T, s Setup) Adapter { return readsElsewhere{newOwnPool(t, s)} },
			want: []string{"connection(s) of its own"},
		},
		{
			name: "InlineSideEffectsFireBeforeTheCommit", check: sideEffectsWaitForTheCommit,
			new:  func(t *testing.T, s Setup) Adapter { return inline{NewReference(s)} },
			want: []string{"Saved ran after Insert, before any commit", "Saved ran for a transaction that rolled back"},
		},
		{
			name: "SideEffectsOnRollbackFireForRolledBackSaves", check: sideEffectsWaitForTheCommit,
			new:  func(t *testing.T, s Setup) Adapter { return rollbackToo{NewReference(s)} },
			want: []string{"Saved ran for a transaction that rolled back", "saved through a savepoint that rolled back", "a closure that ran 2 times, retried once"},
		},
		{
			name: "AnAdapterThatConnectsPerCallIsCounted", check: noConnectionOfItsOwn,
			new:  func(t *testing.T, s Setup) Adapter { return connectsPerCall{NewReference(s)} },
			want: []string{"connection(s) of its own"},
		},
		{
			name: "AModelWithTheWrongKeyDoesntMatch", check: modelsMatchTheTablesItWrites,
			new: func(t *testing.T, s Setup) Adapter {
				return models{NewReference(s), func(ms []data.ModelInfo) []data.ModelInfo { ms[0].Key = []string{"name"}; return ms }}
			},
			want: []string{`model "widget": its key is [name], but`, "primary key is [id]"},
		},
		{
			name: "AModelWithAColumnTheTableLacksDoesntMatch", check: modelsMatchTheTablesItWrites,
			new: func(t *testing.T, s Setup) Adapter {
				return models{NewReference(s), func(ms []data.ModelInfo) []data.ModelInfo { ms[0].Columns = append(ms[0].Columns, "colour"); return ms }}
			},
			want: []string{`it maps column "colour"`},
		},
		{
			name: "NoModelOfTheTableItWrites", check: modelsMatchTheTablesItWrites,
			new: func(t *testing.T, s Setup) Adapter {
				return models{NewReference(s), func(ms []data.ModelInfo) []data.ModelInfo { ms[0].Table = "gadgets"; return ms }}
			},
			want: []string{"no model it registers is stored in", `its table "` + "ormtest_"},
		},
		{
			name: "InvalidModels", check: modelsMatchTheTablesItWrites,
			new: func(t *testing.T, s Setup) Adapter {
				return models{NewReference(s), func(ms []data.ModelInfo) []data.ModelInfo { return append(ms, ms[0]) }}
			},
			want: []string{"invalid", `model "widget" is registered twice`},
		},
		{
			name: "AStatementMarkedForAModelItDoesntRegister", check: modelsMatchTheTablesItWrites,
			new:  func(t *testing.T, s Setup) Adapter { return marks{NewReference(s)} },
			want: []string{`marked for model "gadget"`},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.check(newEnv(t, Factory{New: c.new}))
			if err == nil {
				t.Fatal("the suite passed a bad adapter")
			}
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the suite's report lacks %q:\n%v", want, err)
				}
			}
		})
	}
}

func TestTheReferencePassesEveryCheck(t *testing.T) {
	for name, check := range map[string]func(*env) error{
		"statements": statementsGoThroughTheHandle, "connections": noConnectionOfItsOwn,
		"side effects": sideEffectsWaitForTheCommit, "models": modelsMatchTheTablesItWrites,
	} {
		if err := check(newEnv(t, Factory{New: func(_ *testing.T, s Setup) Adapter { return NewReference(s) }})); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestAReferenceTakingItsHandleFromContextNeverFallsBackToAPool(t *testing.T) {
	r := NewReference(Setup{Schema: "s", Table: "t", Saved: func(context.Context, Widget) {}})
	r.FromContext = true
	ctx := context.Background()
	if err := r.Insert(ctx, nil, Widget{}); err != errNoHandle {
		t.Errorf("Insert: %v", err)
	}
	if err := r.Update(ctx, nil, Widget{}); err != errNoHandle {
		t.Errorf("Update: %v", err)
	}
	if _, err := r.Find(ctx, nil, 1); err != errNoHandle {
		t.Errorf("Find: %v", err)
	}
}

package data_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/thec1oud/galleon/data"
)

func order() data.ModelInfo {
	return data.ModelInfo{
		Name: "order", Schema: "shop", Table: "orders",
		Key:       []string{"id"},
		Columns:   []string{"id", "customer_id", "total"},
		Relations: []data.Relation{{Name: "customer", Model: "customer", Columns: []string{"customer_id"}, References: []string{"id"}}},
	}
}

func customer() data.ModelInfo {
	return data.ModelInfo{Name: "customer", Schema: "shop", Table: "customers", Key: []string{"id"}}
}

func TestAModelNamesItsSchemaQualifiedTable(t *testing.T) {
	if got := order().Identifier().Sanitize(); got != `"shop"."orders"` {
		t.Fatalf("got %s", got)
	}
}

func TestBadModelsAreRefusedWithTheirReason(t *testing.T) {
	for _, c := range []struct {
		want string
		edit func(m *data.ModelInfo)
	}{
		{`the name "" is empty`, func(m *data.ModelInfo) { m.Name = "" }},
		{`the name " order"`, func(m *data.ModelInfo) { m.Name = " order" }},
		{"schema-qualified", func(m *data.ModelInfo) { m.Schema = "" }},
		{"schema-qualified", func(m *data.ModelInfo) { m.Table = "" }},
		{"no primary key", func(m *data.ModelInfo) { m.Key = nil }},
		{`Key names "id" twice`, func(m *data.ModelInfo) { m.Key = []string{"id", "id"} }},
		{"Columns has an empty", func(m *data.ModelInfo) { m.Columns = append(m.Columns, "") }},
		{`Key names "ref", which isn't in Columns`, func(m *data.ModelInfo) { m.Key = []string{"ref"} }},
		{"a relation has no name", func(m *data.ModelInfo) { m.Relations[0].Name = "" }},
		{"declared twice", func(m *data.ModelInfo) { m.Relations = append(m.Relations, m.Relations[0]) }},
		{"names no model", func(m *data.ModelInfo) { m.Relations[0].Model = "" }},
		{"1 columns, 2 references", func(m *data.ModelInfo) { m.Relations[0].References = []string{"id", "x"} }},
		{"0 columns, 0 references", func(m *data.ModelInfo) { m.Relations[0].Columns, m.Relations[0].References = nil, nil }},
		{`relation "customer" names "buyer_id"`, func(m *data.ModelInfo) { m.Relations[0].Columns = []string{"buyer_id"} }},
	} {
		m := order()
		c.edit(&m)
		err := m.Validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v", c.want, err)
		}
	}
	if err := order().Validate(); err != nil {
		t.Fatal(err)
	}
	unmapped := customer()
	unmapped.Relations = []data.Relation{{Name: "referrer", Model: "customer", Columns: []string{"referrer_id"}, References: []string{"id"}}}
	if err := unmapped.Validate(); err != nil {
		t.Fatalf("a model that lists no columns may relate by any: %v", err)
	}
}

func TestTheRegistryRefusesDuplicatesAndDanglingRelations(t *testing.T) {
	dupTable := customer()
	dupTable.Name = "client"
	for want, models := range map[string][]data.ModelInfo{
		`model "customer" is registered twice`:                          {order(), customer(), customer()},
		`"customer" and "client" are both stored in "shop"."customers"`: {order(), customer(), dupTable},
		`refers to model "customer", which isn't registered`:            {order()},
		`no primary key`: {customer(), {Name: "x", Schema: "s", Table: "t"}},
	} {
		if _, err := data.NewModels(models...); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
	listed := customer()
	listed.Columns = []string{"id", "name"}
	o := order()
	o.Relations[0].References = []string{"name", "uuid"}
	o.Relations[0].Columns = []string{"customer_id", "total"}
	if _, err := data.NewModels(o, listed); err == nil || !strings.Contains(err.Error(), `refers to "uuid", which isn't among model "customer"'s columns`) {
		t.Errorf("a reference outside the other model's columns: %v", err)
	}
}

func TestTheRegistryHandsOutCopiesInNameOrder(t *testing.T) {
	r, err := data.NewModels(order(), customer())
	if err != nil {
		t.Fatal(err)
	}
	all := r.All()
	if len(all) != 2 || all[0].Name != "customer" || all[1].Name != "order" {
		t.Fatalf("got %+v", all)
	}
	all[1].Key[0] = "changed"
	all[1].Relations[0].References[0] = "changed"
	m, ok := r.Lookup("order")
	if !ok || m.Key[0] != "id" || m.Relations[0].References[0] != "id" {
		t.Fatalf("a model handed out changed the registry's: %+v", m)
	}
	m.Columns[0] = "changed"
	if m, _ := r.Lookup("order"); m.Columns[0] != "id" {
		t.Fatal("a looked-up model changed the registry's")
	}
	if _, ok := r.Lookup("invoice"); ok {
		t.Fatal("found a model nobody registered")
	}
	var none *data.Models
	if _, ok := none.Lookup("order"); ok || none.All() != nil {
		t.Fatal("a nil registry has models")
	}
}

func TestTheRegistryIsSafeForConcurrentReads(t *testing.T) {
	r, err := data.NewModels(order(), customer())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				m, _ := r.Lookup("order")
				m.Key[0] = "mine"
				for _, m := range r.All() {
					m.Key[0] = "mine"
				}
			}
		})
	}
	wg.Wait()
	if m, _ := r.Lookup("order"); m.Key[0] != "id" {
		t.Fatal("readers changed the registry")
	}
}

func TestAContextNamesTheModelItsStatementsServe(t *testing.T) {
	ctx := context.Background()
	if _, ok := data.ModelFrom(ctx); ok {
		t.Fatal("an empty ctx names a model")
	}
	if name, ok := data.ModelFrom(data.ForModel(ctx, "order")); !ok || name != "order" {
		t.Fatalf("got %q %v", name, ok)
	}
	if _, ok := data.ModelFrom(data.ForModel(data.ForModel(ctx, "order"), "")); ok {
		t.Fatal("an empty name still marks a model")
	}
}

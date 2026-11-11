// Package protect is the sample of the guide "Protect an operation": a
// notes service where people see and change only what they may.
package protect

import (
	"context"
	"slices"
	"strconv"
	"sync"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
	"github.com/thec1oud/galleon/security"
	"github.com/thec1oud/galleon/security/jwt"
	"github.com/thec1oud/galleon/server"
)

type Note struct {
	ID    string `json:"id"`
	Owner string `json:"owner"`
	Text  string `json:"text"`
}

type CreateIn struct {
	Text string `json:"text" validate:"required"`
}

type NoteIn struct {
	ID string `path:"id" json:"-"`
}

type NoteList struct {
	Notes []Note `json:"notes"`
}

type Stats struct {
	Notes int `json:"notes"`
}

// docs:start operations
var (
	// Health is for load balancers: anyone may call it.
	Health = op.Unary[struct{}, struct{}]("notes.health").
		HTTP("GET", "/health").
		Public()

	// Create needs permission for the action alone.
	Create = op.Unary[CreateIn, Note]("notes.create").
		HTTP("POST", "/notes").
		Authorize("notes:create")

	// Get needs permission for the action, then for the note the path
	// names.
	Get = op.Unary[NoteIn, Note]("notes.get").
		HTTP("GET", "/notes/{id}").
		AuthorizeResource("notes:read", op.Resource("note", "id"))

	List = op.Unary[struct{}, NoteList]("notes.list").
		HTTP("GET", "/notes").
		Authorize("notes:list")

	Delete = op.Unary[NoteIn, struct{}]("notes.delete").
		HTTP("DELETE", "/notes/{id}").
		Authorize("notes:delete")
)

// docs:end operations

// docs:start context
// Stats counts an organization's notes, so it only makes sense inside one.
var OrgStats = op.Unary[struct{}, Stats]("notes.org_stats").
	HTTP("GET", "/org/stats").
	Contexts("org").
	Authorize("notes:stats")

// docs:end context

// docs:start policy
// roles grants actions to roles, from the "roles" claim of a caller's token.
var roles = &security.RoleTable{Grants: map[string][]string{
	"admin":                    {"notes:*"},
	security.RoleAuthenticated: {"notes:create", "notes:read", "notes:list", "notes:delete"},
	"analyst":                  {"notes:stats"},
}}

// policy is the roles, plus one rule that needs the note: only its owner,
// or an admin, may read or delete it.
var policy = security.DeciderFunc(func(ctx context.Context, q security.Query) (security.Decision, error) {
	if q.Resource != nil && q.Resource.Type == "note" {
		owner, known := q.Resource.Attributes.String("owner")
		admin, _ := q.Subject.Claims().Strings("roles")
		if known && owner != q.Subject.Subject() && !slices.Contains(admin, "admin") {
			return security.Deny("not the owner"), nil
		}
	}
	return roles.Check(ctx, q)
})

// docs:end policy

// docs:start security
// Security is the security module. The app passes how callers
// authenticate; a test passes testkit.Security instead.
func Security(authenticate security.Option) galleon.Module {
	return security.Module(
		authenticate,
		security.ResolveContext(security.ClaimResolver{Domain: "org", Claim: "org_id"}),
		security.Decide(policy),
	)
}

func options() []galleon.Option {
	return []galleon.Option{
		galleon.Name("notes"),
		galleon.Coordination(local.New()),
		galleon.Deploy(galleon.SingleNode),
		galleon.ConfigFile("config.json"),
		galleon.Modules(server.Module(), Security(jwt.Bearer()), Notes{}),
	}
}

// docs:end security

// Notes is the module that serves notes.
type Notes struct{}

func (Notes) Name() string { return "notes" }

func (Notes) Register(r *galleon.Registry) error {
	s := &Store{notes: map[string]Note{}}
	galleon.Supply(r, s)
	r.Operation(Health, func(context.Context, *struct{}) (*struct{}, error) { return &struct{}{}, nil })
	r.Operation(Create, s.Create)
	r.Operation(Get, s.Get)
	r.Operation(List, s.List)
	r.Operation(Delete, s.Delete)
	r.Operation(OrgStats, s.OrgStats)
	return nil
}

// Store keeps notes in memory.
type Store struct {
	mu    sync.Mutex
	next  int
	notes map[string]Note
}

// docs:start create
func (s *Store) Create(ctx context.Context, in *CreateIn) (*Note, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	n := Note{ID: strconv.Itoa(s.next), Owner: security.Caller(ctx).Subject(), Text: in.Text}
	s.notes[n.ID] = n
	return &n, nil
}

// docs:end create

// docs:start get
func (s *Store) Get(ctx context.Context, in *NoteIn) (*Note, error) {
	n, err := s.find(in.ID)
	if err != nil {
		return nil, err
	}
	// The pipeline checked "notes:read" on this note by its ID. Only now,
	// with the note loaded, can the policy see its owner.
	if err := security.Require(ctx, "notes:read", resource(n)); err != nil {
		return nil, err
	}
	return n, nil
}

func resource(n *Note) *security.Resource {
	return &security.Resource{Type: "note", ID: n.ID, Attributes: security.NewValues(map[string]any{"owner": n.Owner})}
}

// docs:end get

// docs:start list
// List returns the notes the caller may read, asking about them in one
// batch.
func (s *Store) List(ctx context.Context, _ *struct{}) (*NoteList, error) {
	s.mu.Lock()
	all := make([]Note, 0, len(s.notes))
	for _, n := range s.notes {
		all = append(all, n)
	}
	s.mu.Unlock()
	slices.SortFunc(all, func(a, b Note) int { return cmpID(a.ID, b.ID) })
	rs := make([]security.Resource, len(all))
	for i := range all {
		rs[i] = *resource(&all[i])
	}
	can, err := security.CanEach(ctx, "notes:read", rs)
	if err != nil {
		return nil, err
	}
	out := &NoteList{Notes: []Note{}}
	for i, n := range all {
		if can[i] {
			out.Notes = append(out.Notes, n)
		}
	}
	return out, nil
}

// docs:end list

func (s *Store) Delete(ctx context.Context, in *NoteIn) (*struct{}, error) {
	n, err := s.find(in.ID)
	if err != nil {
		return nil, err
	}
	if err := security.Require(ctx, "notes:delete", resource(n)); err != nil {
		return nil, err
	}
	s.mu.Lock()
	delete(s.notes, n.ID)
	s.mu.Unlock()
	return nil, nil
}

// docs:start stats
func (s *Store) OrgStats(ctx context.Context, _ *struct{}) (*Stats, error) {
	org := security.Context(ctx).ID() // the gate let only org contexts through
	_ = org                           // a real store would count WHERE org_id = org
	s.mu.Lock()
	defer s.mu.Unlock()
	return &Stats{Notes: len(s.notes)}, nil
}

// docs:end stats

func (s *Store) find(id string) (*Note, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.notes[id]
	if !ok {
		return nil, errs.New(errs.NotFound, "notes.not_found", "no such note")
	}
	return &n, nil
}

func cmpID(a, b string) int {
	x, _ := strconv.Atoi(a)
	y, _ := strconv.Atoi(b)
	return x - y
}

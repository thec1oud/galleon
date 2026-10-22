// Package notes is the sample behind the "Read from replicas" guide.
package notes

import (
	"context"
	"errors"
	"strconv"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/data"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
)

// Note is what the service stores and returns.
type Note struct {
	ID   string `json:"id"`
	Body string `json:"body"`
}

// CreateIn is the body of a request to save a note.
type CreateIn struct {
	Body string `json:"body" validate:"required,max=1000"`
}

// GetIn names the note to return, from the request's path.
type GetIn struct {
	ID string `path:"id" json:"-"`
}

// Stats is how many notes there are.
type Stats struct {
	Notes int `json:"notes"`
}

// CodeNotFound is the code of a note that doesn't exist.
const CodeNotFound = "notes.not_found"

// docs:start operations
var (
	Create = op.Unary[CreateIn, Note]("notes.create").
		HTTP("POST", "/notes").
		Public()

	// Get reads at the default consistency, read_your_writes: a replica
	// that has every write the caller made, or else the primary.
	Get = op.Unary[GetIn, Note]("notes.get").
		HTTP("GET", "/notes/{id}").
		Public().
		Errors(CodeNotFound)

	// Count may be a little behind, so any replica in rotation will do.
	Count = op.Unary[struct{}, Stats]("notes.count").
		HTTP("GET", "/notes/count").
		Public().
		Consistency(data.Any)
)

// docs:end operations

// Notes is the module that serves notes.
type Notes struct{}

func (Notes) Name() string { return "notes" }

func (Notes) Register(r *galleon.Registry) error {
	var s *Store
	galleon.Provide[*Store](r, func(db data.Manager) *Store {
		s = &Store{db: db}
		return s
	})
	r.Operation(Create, func(ctx context.Context, in *CreateIn) (*Note, error) { return s.Create(ctx, in.Body) })
	r.Operation(Get, func(ctx context.Context, in *GetIn) (*Note, error) { return s.Get(ctx, in.ID) })
	r.Operation(Count, func(ctx context.Context, _ *struct{}) (*Stats, error) { return s.Count(ctx) })
	return nil
}

// docs:start store
// Store keeps notes in the database. Its code never names a replica: each
// read goes where its operation's consistency allows.
type Store struct{ db data.Manager }

func (s *Store) Create(ctx context.Context, body string) (*Note, error) {
	var n Note
	err := s.db.Write(ctx, func(ctx context.Context, tx data.WriteTx) error {
		return tx.QueryRow(ctx, "INSERT INTO notes (body) VALUES ($1) RETURNING id::text, body", body).Scan(&n.ID, &n.Body)
	})
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func (s *Store) Get(ctx context.Context, id string) (*Note, error) {
	notFound := errs.New(errs.NotFound, CodeNotFound, "no such note")
	key, err := strconv.ParseInt(id, 10, 64)
	if err != nil || strconv.FormatInt(key, 10) != id {
		return nil, notFound
	}
	var n Note
	err = s.db.Read(ctx, func(ctx context.Context, tx data.ReadTx) error {
		return tx.QueryRow(ctx, "SELECT id::text, body FROM notes WHERE id = $1", key).Scan(&n.ID, &n.Body)
	})
	if errors.Is(err, data.ErrNoRows) {
		return nil, notFound
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func (s *Store) Count(ctx context.Context) (*Stats, error) {
	var st Stats
	err := s.db.Read(ctx, func(ctx context.Context, tx data.ReadTx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM notes").Scan(&st.Notes)
	})
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// docs:end store

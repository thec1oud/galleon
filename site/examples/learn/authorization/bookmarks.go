package main

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"strconv"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/data"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/migrate"
	"github.com/thec1oud/galleon/op"
	"github.com/thec1oud/galleon/pg"
	"github.com/thec1oud/galleon/pg/sqlmigrate"
	"github.com/thec1oud/galleon/security"
)

// Bookmark is what the service stores and returns.
type Bookmark struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Title string `json:"title"`
	Owner string `json:"owner"`
}

// CreateIn is the body of a request to save a bookmark.
type CreateIn struct {
	URL   string `json:"url" validate:"required,url"`
	Title string `json:"title" validate:"required,max=200"`
}

// GetIn names the bookmark to return, from the request's path.
type GetIn struct {
	ID string `path:"id" json:"-"`
}

// docs:start operations
// The error codes of the bookmarks operations.
const (
	CodeNotFound = "bookmarks.not_found"
	CodeFull     = "bookmarks.full"
)

var (
	Create = op.Unary[CreateIn, Bookmark]("bookmarks.create").
		HTTP("POST", "/bookmarks").
		Authorize("bookmarks:create").
		Errors(CodeFull)

	Get = op.Unary[GetIn, Bookmark]("bookmarks.get").
		HTTP("GET", "/bookmarks/{id}").
		Authorize("bookmarks:read").
		Errors(CodeNotFound)
)

// docs:end operations

// Limits is the "limits" section of the config.
type Limits struct {
	// MaxBookmarks is how many bookmarks the store keeps.
	MaxBookmarks int `json:"max_bookmarks"`
}

// Default fills in what the config leaves out.
func (l *Limits) Default() { l.MaxBookmarks = 1000 }

// Validate refuses a config the service can't run with, at boot.
func (l *Limits) Validate() error {
	if l.MaxBookmarks < 1 {
		return errors.New("max_bookmarks must be at least 1")
	}
	return nil
}

//go:embed migrations/*.sql
var migrations embed.FS

// Bookmarks is the module that serves bookmarks.
type Bookmarks struct{}

func (Bookmarks) Name() string { return "bookmarks" }

// docs:start module
func (Bookmarks) Register(r *galleon.Registry) error {
	galleon.Migrator(r, "bookmarks", func(m *pg.Manager) (migrate.Migrator, error) {
		pool := m.Pool("")
		if pool == nil { // a Manager made with pg.Within, as testkit.DB's, has no pool
			return nil, errors.New("bookmarks: migrations need a database connection pool")
		}
		files, err := fs.Sub(migrations, "migrations")
		if err != nil {
			return nil, err
		}
		return sqlmigrate.New("bookmarks", files, pool)
	})
	galleon.Config[Limits](r, "limits")
	galleon.Provide[*Keys](r, func(db data.Manager) *Keys { return &Keys{db: db} })
	var s *Store
	galleon.Provide[*Store](r, func(db data.Manager, limits *Limits) *Store {
		s = &Store{db: db, max: limits.MaxBookmarks}
		return s
	})
	r.Operation(Create, func(ctx context.Context, in *CreateIn) (*Bookmark, error) { return s.Create(ctx, in) })
	r.Operation(Get, func(ctx context.Context, in *GetIn) (*Bookmark, error) { return s.Get(ctx, in.ID) })
	return nil
}

// docs:end module

// Store keeps up to max bookmarks in the database.
type Store struct {
	db  data.Manager
	max int
}

// docs:start create
func (s *Store) Create(ctx context.Context, in *CreateIn) (*Bookmark, error) {
	owner := security.Caller(ctx).Subject() // the pipeline let only a signed-in caller through
	var b Bookmark
	err := s.db.Write(ctx, func(ctx context.Context, tx data.WriteTx) error {
		var n int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM bookmarks").Scan(&n); err != nil {
			return err
		}
		if n >= s.max {
			return errs.New(errs.FailedPrecondition, CodeFull, "the store is full")
		}
		return tx.QueryRow(ctx,
			"INSERT INTO bookmarks (url, title, owner) VALUES ($1, $2, $3) RETURNING id::text, url, title, owner",
			in.URL, in.Title, owner).Scan(&b.ID, &b.URL, &b.Title, &b.Owner)
	})
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// docs:end create

// docs:start get
func (s *Store) Get(ctx context.Context, id string) (*Bookmark, error) {
	notFound := errs.New(errs.NotFound, CodeNotFound, "no such bookmark")
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || strconv.FormatInt(n, 10) != id {
		return nil, notFound
	}
	var b Bookmark
	var owner *string // bookmarks saved before this chapter have none
	err = s.db.Read(ctx, func(ctx context.Context, tx data.ReadTx) error {
		return tx.QueryRow(ctx,
			"SELECT id::text, url, title, owner FROM bookmarks WHERE id = $1", n).Scan(&b.ID, &b.URL, &b.Title, &owner)
	})
	if errors.Is(err, data.ErrNoRows) {
		return nil, notFound
	}
	if err != nil {
		return nil, err
	}
	if owner != nil {
		b.Owner = *owner
	}
	// Only now, with the bookmark loaded, is its owner known.
	err = security.Require(ctx, "bookmarks:read", &security.Resource{
		Type: "bookmark", ID: b.ID,
		Attributes: security.NewValues(map[string]any{"owner": b.Owner}),
	})
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// docs:end get

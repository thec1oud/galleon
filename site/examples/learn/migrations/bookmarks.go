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
)

// docs:start types
// Bookmark is what the service stores and returns.
type Bookmark struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Title string `json:"title"`
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

// docs:end types

// docs:start operations
// The error codes of the bookmarks operations.
const (
	CodeNotFound = "bookmarks.not_found"
	CodeFull     = "bookmarks.full"
)

var (
	Create = op.Unary[CreateIn, Bookmark]("bookmarks.create").
		HTTP("POST", "/bookmarks").
		Public().
		Errors(CodeFull)

	Get = op.Unary[GetIn, Bookmark]("bookmarks.get").
		HTTP("GET", "/bookmarks/{id}").
		Public().
		Errors(CodeNotFound)
)

// docs:end operations

// docs:start config
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

// docs:end config

// docs:start module

//go:embed migrations/*.sql
var migrations embed.FS

// Bookmarks is the module that serves bookmarks.
type Bookmarks struct{}

func (Bookmarks) Name() string { return "bookmarks" }

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

// docs:start store
// Store keeps up to max bookmarks in the database.
type Store struct {
	db  data.Manager
	max int
}

func (s *Store) Create(ctx context.Context, in *CreateIn) (*Bookmark, error) {
	var b Bookmark
	err := s.db.Write(ctx, func(ctx context.Context, tx data.WriteTx) error {
		// A soft limit: two requests at once can both pass this count.
		var n int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM bookmarks").Scan(&n); err != nil {
			return err
		}
		if n >= s.max {
			return errs.New(errs.FailedPrecondition, CodeFull, "the store is full")
		}
		return tx.QueryRow(ctx,
			"INSERT INTO bookmarks (url, title) VALUES ($1, $2) RETURNING id::text, url, title",
			in.URL, in.Title).Scan(&b.ID, &b.URL, &b.Title)
	})
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *Store) Get(ctx context.Context, id string) (*Bookmark, error) {
	notFound := errs.New(errs.NotFound, CodeNotFound, "no such bookmark")
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || strconv.FormatInt(n, 10) != id {
		return nil, notFound // IDs are numbers, written one way: "1", not "01" or "+1"
	}
	var b Bookmark
	err = s.db.Read(ctx, func(ctx context.Context, tx data.ReadTx) error {
		return tx.QueryRow(ctx,
			"SELECT id::text, url, title FROM bookmarks WHERE id = $1", n).Scan(&b.ID, &b.URL, &b.Title)
	})
	if errors.Is(err, data.ErrNoRows) {
		return nil, notFound
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// docs:end store

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
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
// Bookmarks is the module that serves bookmarks.
type Bookmarks struct{}

func (Bookmarks) Name() string { return "bookmarks" }

func (Bookmarks) Register(r *galleon.Registry) error {
	galleon.Config[Limits](r, "limits")
	var s *Store
	galleon.Provide[*Store](r, func(limits *Limits, log *slog.Logger) *Store {
		s = NewStore(limits.MaxBookmarks, log)
		return s
	})
	r.Operation(Create, func(ctx context.Context, in *CreateIn) (*Bookmark, error) { return s.Create(ctx, in) })
	r.Operation(Get, func(ctx context.Context, in *GetIn) (*Bookmark, error) { return s.Get(ctx, in.ID) })
	return nil
}

// docs:end module

// docs:start store
// Store keeps up to max bookmarks in memory.
type Store struct {
	max  int
	log  *slog.Logger
	mu   sync.Mutex
	last int
	byID map[string]Bookmark
}

func NewStore(max int, log *slog.Logger) *Store {
	return &Store{max: max, log: log, byID: map[string]Bookmark{}}
}

func (s *Store) Create(ctx context.Context, in *CreateIn) (*Bookmark, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.byID) >= s.max {
		return nil, errs.New(errs.FailedPrecondition, CodeFull, "the store is full")
	}
	s.last++
	b := Bookmark{ID: fmt.Sprint(s.last), URL: in.URL, Title: in.Title}
	s.byID[b.ID] = b
	s.log.InfoContext(ctx, "bookmark saved", "id", b.ID)
	return &b, nil
}

func (s *Store) Get(ctx context.Context, id string) (*Bookmark, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.byID[id]
	if !ok {
		return nil, errs.New(errs.NotFound, CodeNotFound, "no such bookmark")
	}
	return &b, nil
}

// docs:end store

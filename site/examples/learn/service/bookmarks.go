package main

import (
	"context"
	"fmt"
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
// CodeNotFound is the error code of a bookmark that doesn't exist.
const CodeNotFound = "bookmarks.not_found"

var (
	Create = op.Unary[CreateIn, Bookmark]("bookmarks.create").
		HTTP("POST", "/bookmarks").
		Public()

	Get = op.Unary[GetIn, Bookmark]("bookmarks.get").
		HTTP("GET", "/bookmarks/{id}").
		Public().
		Errors(CodeNotFound)
)

// docs:end operations

// docs:start module
// Bookmarks is the module that serves bookmarks.
type Bookmarks struct{}

func (Bookmarks) Name() string { return "bookmarks" }

func (Bookmarks) Register(r *galleon.Registry) error {
	s := &store{byID: map[string]Bookmark{}}
	r.Operation(Create, s.create)
	r.Operation(Get, s.get)
	return nil
}

// docs:end module

// docs:start store
// store keeps bookmarks in memory, so they last until the process exits.
type store struct {
	mu   sync.Mutex
	last int
	byID map[string]Bookmark
}

func (s *store) create(ctx context.Context, in *CreateIn) (*Bookmark, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last++
	b := Bookmark{ID: fmt.Sprint(s.last), URL: in.URL, Title: in.Title}
	s.byID[b.ID] = b
	return &b, nil
}

func (s *store) get(ctx context.Context, in *GetIn) (*Bookmark, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.byID[in.ID]
	if !ok {
		return nil, errs.New(errs.NotFound, CodeNotFound, "no such bookmark")
	}
	return &b, nil
}

// docs:end store

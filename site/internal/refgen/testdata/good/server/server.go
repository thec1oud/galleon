// Package server sends some codes with a status of their own.
package server

import (
	"errors"

	"github.com/thec1oud/galleon/errs"
)

type Server struct{}

func (s *Server) writeError(w, r any, err error, status int) {}

var errFormat = errs.New(errs.InvalidArgument, "server.format", "bad format")

func (s *Server) serve(err error) {
	s.writeError(nil, nil, errs.New(errs.InvalidArgument, "server.method", "no"), 405)
	s.writeError(nil, nil, errs.New(errs.NotFound, "server.route", "no"), 0)
	status := 0
	if errors.Is(err, errFormat) {
		status = 415
	}
	s.writeError(nil, nil, err, status)
}

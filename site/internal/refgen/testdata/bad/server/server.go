package server

import (
	"errors"

	"github.com/thec1oud/galleon/errs"
)

type Server struct{}

func (s *Server) writeError(w, r any, err error, status int) {}

var errFormat = errs.New(errs.InvalidArgument, "server.format", "bad format")

func (s *Server) serve(err error, n int) {
	status := 0
	if errors.Is(err, errFormat) {
		status = 415
	}
	if n > 0 {
		status = 413
	}
	s.writeError(nil, nil, err, status)
}

func (s *Server) elseBranch(err error) {
	status := 0
	if errors.Is(err, errFormat) {
	} else {
		status = 400
	}
	s.writeError(nil, nil, err, status)
}

func (s *Server) param(err error, status int) { s.writeError(nil, nil, err, status) }

func (s *Server) conflict() {
	s.writeError(nil, nil, errFormat, 400)
	s.writeError(nil, nil, errFormat, 415)
}

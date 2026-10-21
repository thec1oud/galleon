package ormtest_test

import (
	"testing"

	"github.com/thec1oud/galleon/testkit/conformance/ormtest"
)

func TestTheReferenceAdapterConforms(t *testing.T) {
	ormtest.Run(t, ormtest.Factory{New: func(t *testing.T, s ormtest.Setup) ormtest.Adapter {
		return ormtest.NewReference(s)
	}})
}

func TestTheReferenceAdapterConformsTakingItsHandleFromContext(t *testing.T) {
	ormtest.Run(t, ormtest.Factory{New: func(t *testing.T, s ormtest.Setup) ormtest.Adapter {
		r := ormtest.NewReference(s)
		r.FromContext = true
		return r
	}})
}

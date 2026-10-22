package coordtest_test

import (
	"testing"

	"github.com/thec1oud/galleon/coord"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/testkit/conformance/coordtest"
)

func TestLocalConforms(t *testing.T) {
	coordtest.Run(t, coordtest.Factory{New: func(*testing.T) coord.Backend { return local.New() }})
}

package process_test

import (
	"context"
	"testing"

	"github.com/thec1oud/galleon/process"
)

func TestListenWithoutSupervisorDoesNotSharePorts(t *testing.T) {
	if process.Supervised() {
		t.Skip()
	}
	a, err := process.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if b, err := process.Listen(context.Background(), "tcp", a.Addr().String()); err == nil {
		b.Close()
		t.Fatal("a second unsupervised listener on the same port should fail")
	}
}

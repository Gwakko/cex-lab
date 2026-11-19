package trace

import (
	"context"
	"testing"
)

func TestEnsureKeepsOrCreates(t *testing.T) {
	ctx, id := Ensure(context.Background())
	if len(id) != 16 || ID(ctx) != id {
		t.Fatalf("new id %q", id)
	}
	ctx2, id2 := Ensure(ctx)
	if id2 != id || ID(ctx2) != id {
		t.Fatal("existing id replaced")
	}
	if ID(context.Background()) != "" {
		t.Fatal("empty context has an id")
	}
	if NewID() == NewID() {
		t.Fatal("ids repeat")
	}
}

package git

import (
	"context"
	"os/exec"
	"reflect"
	"testing"
)

func TestResolveThreadIDsFromMessageHandlesMultipleResolvesTrailers(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ids, err := ResolveThreadIDsFromMessage(context.Background(), `fix: handle review feedback

Body.

Resolves: th-one
Resolves: th-two, th-three
Other: ignored
Resolves: th-one
`)
	if err != nil {
		t.Fatalf("ResolveThreadIDsFromMessage: %v", err)
	}
	want := []string{"th-one", "th-two", "th-three"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids = %#v, want %#v", ids, want)
	}
}

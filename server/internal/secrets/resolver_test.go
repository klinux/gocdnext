package secrets_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/secrets"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestNopResolver_AlwaysEmpty(t *testing.T) {
	got, err := secrets.NopResolver{}.Resolve(context.Background(), uuid.New(), []string{"X", "Y"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got = %+v, want empty map", got)
	}
}

func TestNewDBResolver_RejectsNilDependencies(t *testing.T) {
	if _, err := secrets.NewDBResolver(nil, nil); err == nil {
		t.Fatalf("accepted nil store+cipher")
	}
	if _, err := secrets.NewDBResolver(nil, &crypto.Cipher{}); err == nil {
		t.Fatalf("accepted nil store")
	}
}

func TestDBResolver_DelegatesToStore(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	c, err := crypto.NewCipherFromHex(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	ctx := context.Background()

	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "demo", Name: "Demo"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := s.SetSecret(ctx, c, store.SecretSet{
		ProjectID: applied.ProjectID, Name: "FOO", Value: []byte("bar"),
	}); err != nil {
		t.Fatalf("set secret: %v", err)
	}

	r, err := secrets.NewDBResolver(s, c)
	if err != nil {
		t.Fatalf("NewDBResolver: %v", err)
	}
	got, err := r.Resolve(ctx, applied.ProjectID, []string{"FOO", "MISSING"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got["FOO"] != "bar" {
		t.Fatalf("FOO = %q", got["FOO"])
	}
	if _, present := got["MISSING"]; present {
		t.Fatalf("unknown name appeared in result: %+v", got)
	}
}

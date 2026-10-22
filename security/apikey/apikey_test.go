package apikey_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
	"github.com/thec1oud/galleon/security"
	"github.com/thec1oud/galleon/security/apikey"
)

func TestKeys(t *testing.T) {
	key, digest, err := apikey.Generate("bk")
	if err != nil || !strings.HasPrefix(key, "bk_") || len(key) != len("bk_")+43 || apikey.Hash(key) != digest {
		t.Fatalf("Generate: %q, %v", key, err)
	}
	if d, err := apikey.ParseDigest(digest.String()); err != nil || d != digest {
		t.Fatalf("ParseDigest: %v", err)
	}
	if _, _, err := apikey.Generate("Bad-Prefix"); err == nil {
		t.Fatal("a bad prefix was accepted")
	}
	svc := security.Identity{Subject: "key_1", Kind: security.KindAPIKey}.Principal()
	a := apikey.New(apikey.Keys{digest: svc})
	req := func(h http.Header) *op.Request { return &op.Request{Transport: "http", Header: h} }

	if p, err := a.Authenticate(context.Background(), req(http.Header{"X-Api-Key": {key}})); err != nil || p.Subject() != "key_1" {
		t.Fatalf("a known key: %v, %v", p, err)
	}
	if _, err := a.Authenticate(context.Background(), req(http.Header{})); !errors.Is(err, security.ErrNoCredentials) {
		t.Fatalf("no key: %v", err)
	}
	for name, h := range map[string]http.Header{
		"unknown":  {"X-Api-Key": {"bk_nope"}},
		"empty":    {"X-Api-Key": {""}},
		"two":      {"X-Api-Key": {key, key}},
		"too long": {"X-Api-Key": {strings.Repeat("a", 1000)}},
	} {
		if _, err := a.Authenticate(context.Background(), req(h)); errs.From(err).Code != security.CodeInvalidCredentials {
			t.Errorf("%s: %v", name, err)
		}
	}
	custom := apikey.New(apikey.Keys{digest: svc}, apikey.Header("Authorization-Key"), apikey.Scheme("key"))
	if p, err := custom.Authenticate(context.Background(), req(http.Header{"Authorization-Key": {key}})); err != nil || p.Subject() != "key_1" || custom.Scheme() != "key" {
		t.Fatalf("custom header: %v, %v", p, err)
	}
}

type downLookup struct{}

func (downLookup) Lookup(context.Context, apikey.Digest) (security.Principal, error) {
	return nil, errors.New("the database is down")
}

func TestALookupFailureIsNotInvalidCredentials(t *testing.T) {
	_, err := apikey.New(downLookup{}).Authenticate(context.Background(), &op.Request{Header: http.Header{"X-Api-Key": {"bk_x"}}})
	if err == nil || errs.From(err).Kind == errs.Unauthenticated {
		t.Fatalf("got %v", err)
	}
}

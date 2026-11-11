package main

import (
	"context"
	"errors"

	"github.com/thec1oud/galleon/data"
	"github.com/thec1oud/galleon/security"
	"github.com/thec1oud/galleon/security/apikey"
)

// docs:start keys
// Keys stores API keys by their digest, never the keys themselves.
type Keys struct{ db data.Manager }

// Add makes a key for owner, with roles, and returns it. It is shown once:
// only its digest is kept.
func (k *Keys) Add(ctx context.Context, owner string, roles ...string) (string, error) {
	key, digest, err := apikey.Generate("bk")
	if err != nil {
		return "", err
	}
	if roles == nil {
		roles = []string{}
	}
	err = k.db.Write(ctx, func(ctx context.Context, tx data.WriteTx) error {
		_, err := tx.Exec(ctx, "INSERT INTO api_keys (digest, owner, roles) VALUES ($1, $2, $3)", digest[:], owner, roles)
		return err
	})
	return key, err
}

// Lookup finds the owner of a key, for apikey's authenticator.
func (k *Keys) Lookup(ctx context.Context, digest apikey.Digest) (security.Principal, error) {
	var owner string
	var roles []string
	err := k.db.Read(ctx, func(ctx context.Context, tx data.ReadTx) error {
		return tx.QueryRow(ctx, "SELECT owner, roles FROM api_keys WHERE digest = $1", digest[:]).Scan(&owner, &roles)
	})
	if errors.Is(err, data.ErrNoRows) {
		return nil, apikey.ErrUnknownKey
	}
	if err != nil {
		return nil, err
	}
	return security.Identity{
		Subject: owner,
		Kind:    security.KindAPIKey,
		Claims:  security.NewValues(map[string]any{"roles": roles}),
	}.Principal(), nil
}

// docs:end keys

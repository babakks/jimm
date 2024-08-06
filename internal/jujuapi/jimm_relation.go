// Copyright 2024 Canonical Ltd.

package jujuapi

import (
	"context"

	"github.com/canonical/jimm/v3/internal/openfga"
	apiparams "github.com/canonical/jimm/v3/pkg/api/params"
)

// RelationService defines an interface used to manage relations in the authorization model.
type RelationService interface {
	AddRelation(ctx context.Context, user *openfga.User, tuples []apiparams.RelationshipTuple) error
	RemoveRelation(ctx context.Context, user *openfga.User, tuples []apiparams.RelationshipTuple) error
	CheckRelation(ctx context.Context, user *openfga.User, tuple apiparams.RelationshipTuple, trace bool) (_ bool, err error)
	ListRelationshipTuples(ctx context.Context, user *openfga.User, tuple apiparams.RelationshipTuple, pageSize int32, continuationToken string) ([]openfga.Tuple, string, error)
}
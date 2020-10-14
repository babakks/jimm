// Copyright 2020 Canonical Ltd.

package jimmdb

import "gopkg.in/mgo.v2/bson"

// A Query is a helper for creating document queries.
type Query = bson.D

// And creates a query that checks that all the given queries match.
func And(qs ...Query) Query {
	return Query{{Name: "$and", Value: qs}}
}

// Eq creates a query that checks that the given field has the given value.
func Eq(field string, value interface{}) Query {
	return Query{{Name: field, Value: value}}
}

// Exists returns a query that the given field exists.
func Exists(field string) Query {
	return Query{{Name: field, Value: bson.D{{Name: "$exists", Value: true}}}}
}

// NotExists returns a query that the given field does not exist.
func NotExists(field string) Query {
	return Query{{Name: field, Value: bson.D{{Name: "$exists", Value: false}}}}
}
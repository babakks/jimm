// Copyright 2023 canonical.

package names

func NewTag(id, kind, relation string) *Tag {
	return &Tag{
		id:       id,
		relation: Relation(relation),
		kind:     kind,
	}
}
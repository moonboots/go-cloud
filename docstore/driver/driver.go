// Copyright 2019 The Go Cloud Development Kit Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package driver defines a set of interfaces that the docstore package uses to
// interact with the underlying services.
package driver // import "gocloud.dev/docstore/driver"

import (
	"context"

	"gocloud.dev/internal/gcerr"
)

// A Collection is a set of documents.
type Collection interface {
	// Key returns the document key, or nil if the document doesn't have one, which
	// means it is absent or zero value, such as 0, a nil interface value, and any
	// empty array or string.
	//
	// If the collection is able to generate a key for a Create action, then
	// it should not return an error if the key is missing. If the collection
	// can't generate a missing key, it should return an error.
	//
	// The returned key must be comparable.
	//
	// The returned key should not be encoded with the provider's codec; it should
	// be the user-supplied Go value.
	Key(Document) (interface{}, error)

	// RevisionField returns the name of the field used to hold revisions.
	// If the empty string is returned, docstore.RevisionField will be used.
	RevisionField() string

	// RunActions executes a slice of actions.
	//
	// If unordered is false, it must appear as if the actions were executed in the
	// order they appear in the slice, from the client's point of view. The actions
	// need not happen atomically, nor does eventual consistency in the provider
	// system need to be taken into account. For example, after a write returns
	// successfully, the driver can immediately perform a read on the same document,
	// even though the provider's semantics does not guarantee that the read will see
	// the write. RunActions should return immediately after the first action that fails.
	// The returned slice should have a single element.
	//
	// opts controls the behavior of RunActions and is guaranteed to be non-nil.
	RunActions(ctx context.Context, actions []*Action, opts *RunActionsOptions) ActionListError

	// RunGetQuery executes a Query.
	//
	// Implementations can choose to execute the Query as one single request or
	// multiple ones, depending on their service offerings.
	RunGetQuery(context.Context, *Query) (DocumentIterator, error)

	// RunDeleteQuery deletes every document matched by the query.
	RunDeleteQuery(context.Context, *Query) error

	// RunUpdateQuery updates every document matched by the query.
	RunUpdateQuery(context.Context, *Query, []Mod) error

	// QueryPlan returns the plan for the query.
	QueryPlan(*Query) (string, error)

	// As converts i to provider-specific types.
	// See https://gocloud.dev/concepts/as/ for background information.
	As(i interface{}) bool

	// ErrorAs allows providers to expose provider-specific types for returned
	// errors.
	//
	// See https://gocloud.dev/concepts/as/ for background information.
	ErrorAs(err error, i interface{}) bool

	// ErrorCode should return a code that describes the error, which was returned by
	// one of the other methods in this interface.
	ErrorCode(error) gcerr.ErrorCode

	// Close cleans up any resources used by the Collection. Once Close is called,
	// there will be no method calls to the Collection other than As, ErrorAs, and
	// ErrorCode.
	Close() error
}

// ActionKind describes the type of an action.
type ActionKind int

const (
	Create ActionKind = iota
	Replace
	Put
	Get
	Delete
	Update
)

//go:generate stringer -type=ActionKind

// An Action describes a single operation on a single document.
type Action struct {
	Kind       ActionKind  // the kind of action
	Doc        Document    // the document on which to perform the action
	Key        interface{} // the document key returned by Collection.Key, to avoid recomputing it
	FieldPaths [][]string  // field paths to retrieve, for Get only
	Mods       []Mod       // modifications to make, for Update only
	Index      int         // the index of the action in the original action list
}

// A Mod is a modification to a field path in a document.
// At present, the only modifications supported are:
// - set the value at the field path, or create the field path if it doesn't exist
// - delete the field path (when Value is nil)
type Mod struct {
	FieldPath []string
	Value     interface{}
}

// A value representing an increment modification.
type IncOp struct {
	Amount interface{}
}

// An ActionListError contains all the errors encountered from a call to RunActions,
// and the positions of the corresponding actions.
type ActionListError []struct {
	Index int
	Err   error
}

// NewActionListError creates an ActionListError from a slice of errors.
// If the ith element err of the slice is non-nil, the resulting ActionListError
// will have an item {i, err}.
func NewActionListError(errs []error) ActionListError {
	var alerr ActionListError
	for i, err := range errs {
		if err != nil {
			alerr = append(alerr, struct {
				Index int
				Err   error
			}{i, err})
		}
	}
	return alerr
}

// RunActionsOptions controls the behavior of RunActions.
type RunActionsOptions struct {
	// BeforeDo is a callback that must be called once, sequentially, before each one
	// or group of the underlying provider's actions is executed. asFunc allows
	// providers to expose provider-specific types.
	BeforeDo func(asFunc func(interface{}) bool) error
}

// A Query defines a query operation to find documents within a collection based
// on a set of requirements.
type Query struct {
	// FieldPaths contain a list of field paths the user selects to return in the
	// query results. The returned documents should only have these fields
	// populated.
	FieldPaths [][]string

	// Filters contain a list of filters for the query. If there are more than one
	// filter, they should be combined with AND.
	Filters []Filter

	// Limit sets the maximum number of results returned by running the query. When
	// Limit <= 0, the driver implementation should return all possible results.
	Limit int

	// OrderByField is the field to use for sorting the results.
	OrderByField string

	// OrderAscending specifies the sort direction.
	OrderAscending bool

	// BeforeQuery is a callback that must be called exactly once before the
	// underlying provider's query is executed. asFunc allows providers to expose
	// provider-specific types.
	BeforeQuery func(asFunc func(interface{}) bool) error
}

// A Filter defines a filter expression used to filter the query result.
// If the value is a number type, the filter uses numeric comparison.
// If the value is a string type, the filter uses UTF-8 string comparison.
// TODO(#1762): support comparison of other types.
type Filter struct {
	FieldPath []string    // the field path to filter
	Op        string      // the operation, supports =, >, >=, <, <=
	Value     interface{} // the value to compare using the operation
}

// A DocumentIterator iterates through the results (for Get action).
type DocumentIterator interface {

	// Next tries to get the next item in the query result and decodes into Document
	// with the driver's codec.
	// When there are no more results, it should return io.EOF.
	// Once Next returns a non-nil error, it will never be called again.
	Next(context.Context, Document) error

	// Stop terminates the iterator before Next return io.EOF, allowing any cleanup
	// needed.
	Stop()

	// As converts i to provider-specific types.
	// See https://gocloud.dev/concepts/as/ for background information.
	As(i interface{}) bool
}

// EqualOp is the name of the equality operator.
// It is defined here to avoid confusion between "=" and "==".
const EqualOp = "="

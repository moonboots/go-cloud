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

// Package memdocstore provides an in-memory implementation of the docstore
// API. It is suitable for local development and testing.
//
// Every document in a memdocstore collection has a unique primary key. The primary
// key values need not be strings; they may be any comparable Go value.
//
//
// Action Lists
//
// Action lists are executed concurrently. Each action in an action list is executed
// in a separate goroutine.
//
// memdocstore calls the BeforeDo function of an ActionList once before executing the
// actions. Its as function never returns true.
//
//
// URLs
//
// For docstore.OpenCollection, memdocstore registers for the scheme
// "mem".
// To customize the URL opener, or for more details on the URL format,
// see URLOpener.
// See https://gocloud.dev/concepts/urls/ for background information.
package memdocstore // import "gocloud.dev/docstore/memdocstore"

import (
	"context"
	"sort"
	"strings"
	"sync"

	"gocloud.dev/docstore"
	"gocloud.dev/docstore/driver"
	"gocloud.dev/gcerrors"
	"gocloud.dev/internal/gcerr"
)

// Options are optional arguments to the OpenCollection functions.
type Options struct {
	// The name of the field holding the document revision.
	// Defaults to docstore.RevisionField.
	RevisionField string

	// The maximum number of concurrent goroutines started for a single call to
	// ActionList.Do. If less than 1, there is no limit.
	MaxOutstandingActionRPCs int
}

// TODO(jba): make this package thread-safe.

// OpenCollection creates a *docstore.Collection backed by memory. keyField is the
// document field holding the primary key of the collection.
func OpenCollection(keyField string, opts *Options) (*docstore.Collection, error) {
	c, err := newCollection(keyField, nil, opts)
	if err != nil {
		return nil, err
	}
	return docstore.NewCollection(c), nil
}

// OpenCollectionWithKeyFunc creates a *docstore.Collection backed by memory. keyFunc takes
// a document and returns the document's primary key. It should return nil if the
// document is missing the information to construct a key. This will cause all
// actions, even Create, to fail.
func OpenCollectionWithKeyFunc(keyFunc func(docstore.Document) interface{}, opts *Options) (*docstore.Collection, error) {
	c, err := newCollection("", keyFunc, opts)
	if err != nil {
		return nil, err
	}
	return docstore.NewCollection(c), nil
}

func newCollection(keyField string, keyFunc func(docstore.Document) interface{}, opts *Options) (driver.Collection, error) {
	if keyField == "" && keyFunc == nil {
		return nil, gcerr.Newf(gcerr.InvalidArgument, nil, "must provide either keyField or keyFunc")
	}
	if opts == nil {
		opts = &Options{}
	}
	if opts.RevisionField == "" {
		opts.RevisionField = docstore.DefaultRevisionField
	}
	return &collection{
		keyField:    keyField,
		keyFunc:     keyFunc,
		opts:        opts,
		docs:        map[interface{}]map[string]interface{}{},
		curRevision: 0,
	}, nil
}

type collection struct {
	keyField string
	keyFunc  func(docstore.Document) interface{}
	opts     *Options
	mu       sync.Mutex
	// map from keys to documents. Documents are represented as map[string]interface{},
	// regardless of what their original representation is. Even if the user is using
	// map[string]interface{}, we make our own copy.
	docs        map[interface{}]map[string]interface{}
	curRevision int64 // incremented on each write
}

func (c *collection) Key(doc driver.Document) (interface{}, error) {
	if c.keyField != "" {
		key, _ := doc.GetField(c.keyField) // no error on missing key, and it will be nil
		return key, nil
	}
	key := c.keyFunc(doc.Origin)
	if key == nil {
		return nil, gcerr.Newf(gcerr.InvalidArgument, nil, "missing document key")
	}
	return key, nil
}

func (c *collection) RevisionField() string {
	return c.opts.RevisionField
}

// ErrorCode implements driver.ErrorCode.
func (c *collection) ErrorCode(err error) gcerr.ErrorCode {
	return gcerrors.Code(err)
}

// RunActions implements driver.RunActions.
func (c *collection) RunActions(ctx context.Context, actions []*driver.Action, opts *driver.RunActionsOptions) driver.ActionListError {
	errs := make([]error, len(actions))

	// Run the actions concurrently with each other.
	run := func(as []*driver.Action) {
		t := driver.NewThrottle(c.opts.MaxOutstandingActionRPCs)
		for _, a := range as {
			a := a
			t.Acquire()
			go func() {
				defer t.Release()
				errs[a.Index] = c.runAction(ctx, a)
			}()
		}
		t.Wait()
	}

	if opts.BeforeDo != nil {
		if err := opts.BeforeDo(func(interface{}) bool { return false }); err != nil {
			for i := range errs {
				errs[i] = err
			}
			return driver.NewActionListError(errs)
		}
	}

	beforeGets, gets, writes, afterGets := driver.GroupActions(actions)
	run(beforeGets)
	run(gets)
	run(writes)
	run(afterGets)
	return driver.NewActionListError(errs)
}

// runAction executes a single action.
func (c *collection) runAction(ctx context.Context, a *driver.Action) error {
	// Stop if the context is done.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Get the key from the doc so we can look it up in the map.
	c.mu.Lock()
	defer c.mu.Unlock()
	// If there is a key, get the current document with that key.
	var (
		current map[string]interface{}
		exists  bool
	)
	if a.Key != nil {
		current, exists = c.docs[a.Key]
	}
	// Check for a NotFound error.
	if !exists && (a.Kind == driver.Replace || a.Kind == driver.Update || a.Kind == driver.Get) {
		return gcerr.Newf(gcerr.NotFound, nil, "document with key %v does not exist", a.Key)
	}
	switch a.Kind {
	case driver.Create:
		// It is an error to attempt to create an existing document.
		if exists {
			return gcerr.Newf(gcerr.AlreadyExists, nil, "Create: document with key %v exists", a.Key)
		}
		// If the user didn't supply a value for the key field, create a new one.
		if a.Key == nil {
			a.Key = driver.UniqueString()
			// Set the new key in the document.
			if err := a.Doc.SetField(c.keyField, a.Key); err != nil {
				return gcerr.Newf(gcerr.InvalidArgument, nil, "cannot set key field %q", c.keyField)
			}
		}
		fallthrough

	case driver.Replace, driver.Put:
		if err := c.checkRevision(a.Doc, current); err != nil {
			return err
		}
		doc, err := encodeDoc(a.Doc)
		if err != nil {
			return err
		}
		c.changeRevision(doc)
		// Ignore errors. It's fine if the doc doesn't have a revision field.
		a.Doc.SetField(c.opts.RevisionField, doc[c.opts.RevisionField])
		c.docs[a.Key] = doc

	case driver.Delete:
		if err := c.checkRevision(a.Doc, current); err != nil {
			return err
		}
		delete(c.docs, a.Key)

	case driver.Update:
		if err := c.checkRevision(a.Doc, current); err != nil {
			return err
		}
		if err := c.update(current, a.Mods); err != nil {
			return err
		}
		_ = a.Doc.SetField(c.opts.RevisionField, current[c.opts.RevisionField])

	case driver.Get:
		// We've already retrieved the document into current, above.
		// Now we copy its fields into the user-provided document.
		if err := decodeDoc(current, a.Doc, a.FieldPaths, c.opts.RevisionField); err != nil {
			return err
		}
	default:
		return gcerr.Newf(gcerr.Internal, nil, "unknown kind %v", a.Kind)
	}
	return nil
}

// Must be called with the lock held.
func (c *collection) update(doc map[string]interface{}, mods []driver.Mod) error {
	// Sort mods by first field path element so tests are deterministic.
	sort.Slice(mods, func(i, j int) bool { return mods[i].FieldPath[0] < mods[j].FieldPath[0] })

	// To make update atomic, we first convert the actions into a form that can't
	// fail.
	type guaranteedMod struct {
		parentMap    map[string]interface{} // the map holding the key to be modified
		key          string
		encodedValue interface{} // the value after encoding
	}

	gmods := make([]guaranteedMod, len(mods))
	var err error
	for i, mod := range mods {
		gmod := &gmods[i]
		// Check that the field path is valid. That is, every component of the path
		// but the last refers to a map, and no component along the way is nil.
		if gmod.parentMap, err = getParentMap(doc, mod.FieldPath, false); err != nil {
			return err
		}
		gmod.key = mod.FieldPath[len(mod.FieldPath)-1]
		if inc, ok := mod.Value.(driver.IncOp); ok {
			amt, err := encodeValue(inc.Amount)
			if err != nil {
				return err
			}
			if gmod.encodedValue, err = add(gmod.parentMap[gmod.key], amt); err != nil {
				return err
			}
		} else if mod.Value != nil {
			// Make sure the value encodes successfully.
			if gmod.encodedValue, err = encodeValue(mod.Value); err != nil {
				return err
			}
		}
	}
	// Now execute the guaranteed mods.
	for _, m := range gmods {
		if m.encodedValue == nil {
			delete(m.parentMap, m.key)
		} else {
			m.parentMap[m.key] = m.encodedValue
		}
	}
	c.changeRevision(doc)
	return nil
}

// Add two encoded numbers.
// Since they're encoded, they are either int64 or float64.
// Allow adding a float to an int, producing a float.
// TODO(jba): see how other providers handle that.
func add(x, y interface{}) (interface{}, error) {
	if x == nil {
		return y, nil
	}
	switch x := x.(type) {
	case int64:
		switch y := y.(type) {
		case int64:
			return x + y, nil
		case float64:
			return float64(x) + y, nil
		default:
			// This shouldn't happen because it should be checked by docstore.
			return nil, gcerr.Newf(gcerr.Internal, nil, "bad increment aount type %T", y)
		}
	case float64:
		switch y := y.(type) {
		case int64:
			return x + float64(y), nil
		case float64:
			return x + y, nil
		default:
			// This shouldn't happen because it should be checked by docstore.
			return nil, gcerr.Newf(gcerr.Internal, nil, "bad increment aount type %T", y)
		}
	default:
		return nil, gcerr.Newf(gcerr.InvalidArgument, nil, "value %v being incremented not int64 or float64", x)
	}
}

// Must be called with the lock held.
func (c *collection) changeRevision(doc map[string]interface{}) {
	c.curRevision++
	doc[c.opts.RevisionField] = c.curRevision
}

func (c *collection) checkRevision(arg driver.Document, current map[string]interface{}) error {
	if current == nil {
		return nil // no existing document
	}
	curRev := current[c.opts.RevisionField].(int64)
	r, err := arg.GetField(c.opts.RevisionField)
	if err != nil || r == nil {
		return nil // no incoming revision information: nothing to check
	}
	wantRev, ok := r.(int64)
	if !ok {
		return gcerr.Newf(gcerr.InvalidArgument, nil, "revision field %s is not an int64", c.opts.RevisionField)
	}
	if wantRev != curRev {
		return gcerr.Newf(gcerr.FailedPrecondition, nil, "mismatched revisions: want %d, current %d", wantRev, curRev)
	}
	return nil
}

// getAtFieldPath gets the value of m at fp. It returns an error if fp is invalid
// (see getParentMap).
func getAtFieldPath(m map[string]interface{}, fp []string) (interface{}, error) {
	m2, err := getParentMap(m, fp, false)
	if err != nil {
		return nil, err
	}
	return m2[fp[len(fp)-1]], nil
}

// setAtFieldPath sets m's value at fp to val. It creates intermediate maps as
// needed. It returns an error if a non-final component of fp does not denote a map.
func setAtFieldPath(m map[string]interface{}, fp []string, val interface{}) error {
	m2, err := getParentMap(m, fp, true)
	if err != nil {
		return err
	}
	m2[fp[len(fp)-1]] = val
	return nil
}

// Delete the value from m at the given field path, if it exists.
func deleteAtFieldPath(m map[string]interface{}, fp []string) {
	m2, _ := getParentMap(m, fp, false) // ignore error
	if m2 != nil {
		delete(m2, fp[len(fp)-1])
	}
}

// getParentMap returns the map that directly contains the given field path;
// that is, the value of m at the field path that excludes the last component
// of fp. If a non-map is encountered along the way, an InvalidArgument error is
// returned. If nil is encountered, nil is returned unless create is true, in
// which case a map is added at that point.
func getParentMap(m map[string]interface{}, fp []string, create bool) (map[string]interface{}, error) {
	var ok bool
	for _, k := range fp[:len(fp)-1] {
		if m[k] == nil {
			if !create {
				return nil, nil
			}
			m[k] = map[string]interface{}{}
		}
		m, ok = m[k].(map[string]interface{})
		if !ok {
			return nil, gcerr.Newf(gcerr.InvalidArgument, nil, "invalid field path %q at %q", strings.Join(fp, "."), k)
		}
	}
	return m, nil
}

// As implements driver.As.
func (c *collection) As(i interface{}) bool { return false }

// As implements driver.Collection.ErrorAs.
func (c *collection) ErrorAs(err error, i interface{}) bool { return false }

// Close implements driver.Collection.Close.
func (c *collection) Close() error { return nil }

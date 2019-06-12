// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package encryption

import (
	"github.com/zeebo/errs"
	"storj.io/storj/pkg/paths"
	"storj.io/storj/pkg/storj"
)

// The Store allows one to find the matching most encrypted key and path for
// some unencrypted path. It also reports a mapping of encrypted to unencrypted paths
// at the searched for unencrypted path.
//
// For example, if the Store contains the mappings
//
//    b1, u1/u2/u3    => <e1/e2/e3, k3>
//    b1, u1/u2/u3/u4 => <e1/e2/e3/e4, k4>
//    b1, u1/u5       => <e1/e5, k5>
//    b1, u6          => <e6, k6>
//    b1, u6/u7/u8    => <e6/e7/e8, k8>
//    b2, u1          => <e1', k1'>
//
// Then the following lookups have outputs
//
//    b1, u1          => <{e2:u2, e5:u5}, u1, nil>
//    b1, u1/u2/u3    => <{e4:u4}, u1/u2/u3, <u1/u2/u3, e1/e2/e3, k3>>
//    b1, u1/u2/u3/u6 => <{}, u1/u2/u3/, <u1/u2/u3, e1/e2/e3, k3>>
//    b1, u1/u2/u3/u4 => <{}, u1/u2/u3/u4, <u1/u2/u3/u4, e1/e2/e3/e4, k4>>
//    b1, u6/u7       => <{e8:u8}, u6/, <u6, e6, k6>>
//    b2, u1          => <{}, u1, <u1, e1', k1'>>
type Store struct {
	roots map[string]*node
}

// node is a node in the Store graph. It may contain an encryption key and encrypted path,
// a list of children nodes, and data to ensure a bijection between encrypted and unencrypted
// path entries.
type node struct {
	unenc    map[string]*node  // unenc => node
	unencMap map[string]string // unenc => enc
	enc      map[string]*node  // enc => node
	encMap   map[string]string // enc => unenc
	base     *Base
}

// Base represents a key with which to derive further keys at some encrypted/unencrypted path.
type Base struct {
	Unencrypted paths.Unencrypted
	Encrypted   paths.Encrypted
	Key         storj.Key
}

// clone returns a copy of the Base. The implementation can be simple because the
// types of its fields do not contain any references.
func (b *Base) clone() *Base {
	if b == nil {
		return nil
	}
	bc := *b
	return &bc
}

// NewStore constructs a Store.
func NewStore() *Store {
	return &Store{roots: make(map[string]*node)}
}

// newNode constructs a node.
func newNode() *node {
	return &node{
		unenc:    make(map[string]*node),
		unencMap: make(map[string]string),
		enc:      make(map[string]*node),
		encMap:   make(map[string]string),
	}
}

// Add creates a mapping from the unencrypted path to the encrypted path and key.
func (s *Store) Add(bucket string, unenc paths.Unencrypted, enc paths.Encrypted, key storj.Key) error {
	root, ok := s.roots[bucket]
	if !ok {
		root = newNode()
		s.roots[bucket] = root
	}

	return root.add(unenc.Iterator(), enc.Iterator(), &Base{
		Unencrypted: unenc,
		Encrypted:   enc,
		Key:         key,
	})
}

// add places the paths and base into the node tree structure.
func (n *node) add(unenc, enc paths.Iterator, base *Base) error {
	if unenc.Done() != enc.Done() {
		return errs.New("encrypted and unencrypted paths had different number of components")
	}

	// If we're done walking the paths, this node must have the provided base.
	if unenc.Done() {
		n.base = base
		return nil
	}

	// Walk to the next parts and ensure they're consistent with previous additions.
	unencPart, encPart := unenc.Next(), enc.Next()
	if exUnencPart, ok := n.encMap[encPart]; ok && exUnencPart != unencPart {
		return errs.New("conflicting encrypted parts for unencrypted path")
	}
	if exEncPart, ok := n.unencMap[unencPart]; ok && exEncPart != encPart {
		return errs.New("conflicting encrypted parts for unencrypted path")
	}

	// Look up the child node. Since we're sure the unenc and enc mappings are
	// consistent, we can look it up in one map and unconditionally insert it
	// into both maps if necessary.
	child, ok := n.unenc[unencPart]
	if !ok {
		child = newNode()
		n.unencMap[unencPart] = encPart
		n.encMap[encPart] = unencPart
		n.unenc[unencPart] = child
		n.enc[encPart] = child
	}

	// Recurse to the next node in the tree.
	return child.add(unenc, enc, base)
}

// LookupUnencrypted finds the matching most unencrypted path added to the Store, reports how
// much of the path matched, any known unencrypted paths at the requested path, and if a key
// and encrypted path exists for some prefix of the unencrypted path.
func (s *Store) LookupUnencrypted(bucket string, path paths.Unencrypted) (
	revealed map[string]string, consumed paths.Unencrypted, base *Base) {

	root, ok := s.roots[bucket]
	if !ok {
		return nil, paths.Unencrypted{}, nil
	}

	revealed, rawConsumed, base := root.lookup(path.Iterator(), "", nil, true)
	return revealed, paths.NewUnencrypted(rawConsumed), base.clone()
}

// LookupEncrypted finds the matching most encrypted path added to the Store, reports how
// much of the path matched, any known encrypted paths at the requested path, and if a key
// an encrypted path exists for some prefix of the encrypted path.
func (s *Store) LookupEncrypted(bucket string, path paths.Encrypted) (
	revealed map[string]string, consumed paths.Encrypted, base *Base) {

	root, ok := s.roots[bucket]
	if !ok {
		return nil, paths.Encrypted{}, nil
	}

	revealed, rawConsumed, base := root.lookup(path.Iterator(), "", nil, false)
	return revealed, paths.NewEncrypted(rawConsumed), base.clone()
}

// lookup searches for the path in the node tree structure.
func (n *node) lookup(path paths.Iterator, bestConsumed string, bestBase *Base, unenc bool) (
	map[string]string, string, *Base) {

	// Keep track of the best match so far.
	if n.base != nil || bestBase == nil {
		bestConsumed, bestBase = path.Consumed(), n.base
	}

	// Pick the tree we're walking down based on the unenc bool.
	revealed, children := n.unencMap, n.enc
	if unenc {
		revealed, children = n.encMap, n.unenc
	}

	// If we're done walking the path, then return our best match along with the
	// revealed paths at this node.
	if path.Done() {
		return revealed, bestConsumed, bestBase
	}

	// Walk to the next node in the tree. If there is no node, then report our best match.
	child, ok := children[path.Next()]
	if !ok {
		return nil, bestConsumed, bestBase
	}

	// Recurse to the next node in the tree.
	return child.lookup(path, bestConsumed, bestBase, unenc)
}
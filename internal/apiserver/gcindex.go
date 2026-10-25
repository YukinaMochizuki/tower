package apiserver

import (
	"cmp"
	"slices"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/YukinaMochizuki/tower/internal/apimachinery/object"
	"github.com/YukinaMochizuki/tower/internal/store"
)

// ownerIndex maps an owner's uid to the objects whose owner references name
// it, as the store events the garbage collector has followed left them. It
// is a hint: whoever acts on a key it gives reads the object and checks it
// first. The event loop writes it and the periodic sweep reads it.
type ownerIndex struct {
	mu sync.Mutex
	// ready is false until a listing of the whole store has built it.
	ready bool
	// rev is the revision through which every event has been applied.
	rev int64
	m   ownerMap
}

// ownerMap is the index's content.
type ownerMap struct {
	dependents map[string]map[store.Key]struct{}
	// owners holds the uids each object named when last seen, which is
	// what a later change to it, or its delete, takes back.
	owners map[store.Key][]string
}

func newOwnerMap() ownerMap {
	return ownerMap{dependents: map[string]map[store.Key]struct{}{}, owners: map[store.Key][]string{}}
}

// set records that the object at key names the uids, in place of what it
// named before, and returns whether it names one it did not.
func (m ownerMap) set(key store.Key, uids []string) (added bool) {
	old := m.owners[key]
	for _, uid := range old {
		if !slices.Contains(uids, uid) {
			m.drop(uid, key)
		}
	}
	for _, uid := range uids {
		if slices.Contains(old, uid) {
			continue
		}
		added = true
		deps := m.dependents[uid]
		if deps == nil {
			deps = map[store.Key]struct{}{}
			m.dependents[uid] = deps
		}
		deps[key] = struct{}{}
	}
	if len(uids) == 0 {
		delete(m.owners, key)
	} else {
		m.owners[key] = uids
	}
	return added
}

func (m ownerMap) remove(key store.Key) {
	for _, uid := range m.owners[key] {
		m.drop(uid, key)
	}
	delete(m.owners, key)
}

func (m ownerMap) drop(uid string, key store.Key) {
	deps := m.dependents[uid]
	delete(deps, key)
	if len(deps) == 0 {
		delete(m.dependents, uid)
	}
}

// install replaces the index with m, built from a listing that started at
// rev: the events after rev then bring it up to date.
func (x *ownerIndex) install(rev int64, m ownerMap) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.ready, x.rev, x.m = true, rev, m
}

// invalidate marks the index as missing events, until install.
func (x *ownerIndex) invalidate() {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.ready = false
}

// apply records one followed event of an object, which names the uids
// unless it was deleted, and returns whether it now names an owner it did
// not. Applying an event the listing already reflected changes nothing.
func (x *ownerIndex) apply(ev store.Event, uids []string) (added bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if !x.ready {
		return false
	}
	x.rev = max(x.rev, ev.Revision)
	if ev.Type == store.EventDeleted {
		x.m.remove(ev.Object.Key)
		return false
	}
	return x.m.set(ev.Object.Key, uids)
}

// lookup returns the keys of the objects that name uid, in key order, and
// the revision they are as of; ok is false if the index is not built.
func (x *ownerIndex) lookup(uid string) (keys []store.Key, rev int64, ok bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if !x.ready {
		return nil, 0, false
	}
	for k := range x.m.dependents[uid] {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, compareKeys)
	return keys, x.rev, true
}

func compareKeys(a, b store.Key) int {
	return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Project, b.Project), cmp.Compare(a.Name, b.Name))
}

// ownerUIDs returns the uids obj's owner references name.
func ownerUIDs(obj proto.Message) []string {
	refs := object.Meta(obj).GetOwnerReferences()
	if len(refs) == 0 {
		return nil
	}
	uids := make([]string, 0, len(refs))
	for _, r := range refs {
		if !slices.Contains(uids, r.GetUid()) {
			uids = append(uids, r.GetUid())
		}
	}
	return uids
}

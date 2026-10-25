// Package keylock gives a lock per key, such as an image digest, kept only
// while someone holds or waits for it.
package keylock

import "sync"

// Map is a set of locks by key. The zero value is ready to use.
type Map struct {
	mu    sync.Mutex
	locks map[string]*entry
}

type entry struct {
	sync.Mutex
	users int
}

// Lock takes the lock of key and returns its unlock.
func (m *Map) Lock(key string) (unlock func()) {
	m.mu.Lock()
	if m.locks == nil {
		m.locks = map[string]*entry{}
	}
	e := m.locks[key]
	if e == nil {
		e = &entry{}
		m.locks[key] = e
	}
	e.users++
	m.mu.Unlock()
	e.Lock()
	var once sync.Once
	return func() {
		once.Do(func() {
			e.Unlock()
			m.mu.Lock()
			defer m.mu.Unlock()
			if e.users--; e.users == 0 {
				delete(m.locks, key)
			}
		})
	}
}

// TryLock takes the lock of key if no one holds it, and returns its unlock;
// ok is false, and nothing taken, if someone does.
func (m *Map) TryLock(key string) (unlock func(), ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locks[key] != nil {
		return nil, false
	}
	if m.locks == nil {
		m.locks = map[string]*entry{}
	}
	e := &entry{users: 1}
	e.Lock()
	m.locks[key] = e
	var once sync.Once
	return func() {
		once.Do(func() {
			e.Unlock()
			m.mu.Lock()
			defer m.mu.Unlock()
			if e.users--; e.users == 0 {
				delete(m.locks, key)
			}
		})
	}, true
}

// Held says someone holds or waits for the lock of key.
func (m *Map) Held(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.locks[key] != nil
}

// Len returns how many keys have a lock held or waited for.
func (m *Map) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.locks)
}

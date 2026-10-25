package apiserver

import "sync"

// keyLocks is a mutex per key, made when first wanted and dropped once no
// one holds or waits for it, so writers of different objects never wait
// on each other. The zero value is ready to use.
type keyLocks struct {
	mu    sync.Mutex
	locks map[string]*keyLock
}

type keyLock struct {
	sync.Mutex
	// refs counts who holds or waits for the lock, under keyLocks.mu.
	refs int
}

// lock takes the key's lock and returns what releases it.
func (k *keyLocks) lock(key string) (unlock func()) {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyLock{}
	}
	l := k.locks[key]
	if l == nil {
		l = &keyLock{}
		k.locks[key] = l
	}
	l.refs++
	k.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		k.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}

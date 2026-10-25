// Package secrets seals a Secret's data for the store and opens it again,
// and reads the Docker config format registry-auth secrets hold. Data is
// encrypted under a key of its own, a data key, which is encrypted under
// the platform's key-encryption key (KEK), a file outside the database: a
// copy of the database alone holds no value. See docs/decisions/secrets.md.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"google.golang.org/protobuf/proto"

	secretsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/secrets/v1alpha1"
)

// KEK is a key-encryption key: an id, which the data keys it wraps record,
// and 32 bytes.
type KEK struct {
	ID  string
	key []byte
}

// kekIDRE is what an id may be: it is written into every sealed secret.
var kekIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ParseKEK reads a KEK file: one line, its id, a space, and its 32 bytes in
// standard base64. What fails to parse is named, never quoted.
func ParseKEK(b []byte) (KEK, error) {
	fields := strings.Fields(string(b))
	if len(fields) != 2 {
		return KEK{}, errors.New("a key-encryption key file is one line: an id, a space, and 32 bytes in base64")
	}
	if !kekIDRE.MatchString(fields[0]) {
		return KEK{}, errors.New("the key-encryption key's id must be letters, digits, '.', '_' or '-', at most 63")
	}
	key, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil || len(key) != 32 {
		return KEK{}, errors.New("the key-encryption key must be 32 bytes in standard base64")
	}
	return KEK{ID: fields[0], key: key}, nil
}

// LoadKEK reads a KEK file, which only its owner may read.
func LoadKEK(path string) (KEK, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return KEK{}, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return KEK{}, fmt.Errorf("%s is mode %#o: a key-encryption key is its owner's alone (0600)", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return KEK{}, err
	}
	k, err := ParseKEK(b)
	if err != nil {
		return KEK{}, fmt.Errorf("%s: %w", path, err)
	}
	return k, nil
}

// NewKEK makes a random KEK, for tests and tools.
func NewKEK(id string) KEK {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	return KEK{ID: id, key: key}
}

// String is the KEK file's content.
func (k KEK) String() string { return k.ID + " " + base64.StdEncoding.EncodeToString(k.key) + "\n" }

// Keyring seals with its current KEK and opens what any of its KEKs sealed,
// so a KEK can be rotated: the old one opens what it sealed until Rewrap
// has sealed it again under the new.
type Keyring struct {
	current  KEK
	previous map[string]KEK
	hmacKey  []byte
}

// Errors opening a sealed secret.
var (
	// ErrUnknownKEK means the secret was sealed under a KEK the keyring
	// does not hold.
	ErrUnknownKEK = errors.New("sealed under a key-encryption key this apiserver does not hold")
	// ErrUndecryptable means the secret's data did not decrypt: the KEK is
	// not the one it was sealed under, or the stored data was changed.
	ErrUndecryptable = errors.New("does not decrypt with the key-encryption key it names")
)

// NewKeyring returns a keyring sealing under current, which opens what
// previous sealed too.
func NewKeyring(current KEK, previous ...KEK) (*Keyring, error) {
	r := &Keyring{current: current, previous: map[string]KEK{}}
	for _, k := range previous {
		if k.ID == current.ID {
			return nil, fmt.Errorf("key-encryption key id %q is both the current one and a previous one", k.ID)
		}
		if _, dup := r.previous[k.ID]; dup {
			return nil, fmt.Errorf("key-encryption key id %q is given twice", k.ID)
		}
		r.previous[k.ID] = k
	}
	h, err := hkdf.Key(sha256.New, current.key, nil, "tower secret content hmac v1", 32)
	if err != nil {
		return nil, err
	}
	r.hmacKey = h
	return r, nil
}

// CurrentID names the KEK the keyring seals under.
func (r *Keyring) CurrentID() string { return r.current.ID }

// Object names what a sealed secret belongs to; sealed data opens only for
// the object it was sealed for, so stored data moved to another secret
// does not open.
func Object(project, name, uid string) string { return project + "/" + name + "/" + uid }

// plaintext is the deterministic encoding of data that is sealed and hashed.
func plaintext(data map[string][]byte) ([]byte, error) {
	return proto.MarshalOptions{Deterministic: true}.Marshal(&secretsv1alpha1.SecretSpec{Data: data})
}

// HMAC is the content HMAC of data under the current KEK.
func (r *Keyring) HMAC(data map[string][]byte) (string, error) {
	p, err := plaintext(data)
	if err != nil {
		return "", err
	}
	m := hmac.New(sha256.New, r.hmacKey)
	m.Write(p)
	return "hmac-sha256:" + hex.EncodeToString(m.Sum(nil)), nil
}

// Seal encrypts data for the object, under a new data key wrapped by the
// current KEK.
func (r *Keyring) Seal(object string, data map[string][]byte) (*secretsv1alpha1.SealedData, error) {
	p, err := plaintext(data)
	if err != nil {
		return nil, err
	}
	sum, err := r.HMAC(data)
	if err != nil {
		return nil, err
	}
	dataKey := make([]byte, 32)
	if _, err := rand.Read(dataKey); err != nil {
		return nil, err
	}
	wrapped, err := r.wrap(r.current, object, dataKey)
	if err != nil {
		return nil, err
	}
	nonce, ciphertext, err := encrypt(dataKey, p, dataAAD(object))
	if err != nil {
		return nil, err
	}
	return &secretsv1alpha1.SealedData{KekId: r.current.ID, WrappedKey: wrapped, Nonce: nonce, Ciphertext: ciphertext, ContentHmac: sum}, nil
}

// Open decrypts what Seal sealed for the object.
func (r *Keyring) Open(object string, s *secretsv1alpha1.SealedData) (map[string][]byte, error) {
	dataKey, err := r.unwrap(object, s)
	if err != nil {
		return nil, err
	}
	aead, err := newGCM(dataKey)
	if err != nil {
		return nil, err
	}
	p, err := aead.Open(nil, s.GetNonce(), s.GetCiphertext(), dataAAD(object))
	if err != nil {
		return nil, ErrUndecryptable
	}
	var spec secretsv1alpha1.SecretSpec
	if err := proto.Unmarshal(p, &spec); err != nil {
		return nil, ErrUndecryptable
	}
	return spec.GetData(), nil
}

// Rewrap seals s again under the current KEK, with its HMAC under that KEK,
// if another KEK sealed it; ok says whether it did.
func (r *Keyring) Rewrap(object string, s *secretsv1alpha1.SealedData) (out *secretsv1alpha1.SealedData, ok bool, err error) {
	if s.GetKekId() == r.current.ID {
		return s, false, nil
	}
	data, err := r.Open(object, s)
	if err != nil {
		return nil, false, err
	}
	out, err = r.Seal(object, data)
	return out, err == nil, err
}

func (r *Keyring) kek(id string) (KEK, bool) {
	if id == r.current.ID {
		return r.current, true
	}
	k, ok := r.previous[id]
	return k, ok
}

func (r *Keyring) wrap(k KEK, object string, dataKey []byte) ([]byte, error) {
	nonce, ct, err := encrypt(k.key, dataKey, keyAAD(k.ID, object))
	if err != nil {
		return nil, err
	}
	return append(nonce, ct...), nil
}

func (r *Keyring) unwrap(object string, s *secretsv1alpha1.SealedData) ([]byte, error) {
	k, ok := r.kek(s.GetKekId())
	if !ok {
		return nil, ErrUnknownKEK
	}
	aead, err := newGCM(k.key)
	if err != nil {
		return nil, err
	}
	w := s.GetWrappedKey()
	if len(w) < aead.NonceSize() {
		return nil, ErrUndecryptable
	}
	dataKey, err := aead.Open(nil, w[:aead.NonceSize()], w[aead.NonceSize():], keyAAD(k.ID, object))
	if err != nil {
		return nil, ErrUndecryptable
	}
	return dataKey, nil
}

func keyAAD(kekID, object string) []byte {
	return []byte("tower secret data key v1\x00" + kekID + "\x00" + object)
}

func dataAAD(object string) []byte { return []byte("tower secret data v1\x00" + object) }

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func encrypt(key, plain, aad []byte) (nonce, ciphertext []byte, err error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return nonce, aead.Seal(nil, nonce, plain, aad), nil
}

// SameContent reports whether two content HMACs are the same and set, in
// constant time.
func SameContent(a, b string) bool { return a != "" && hmac.Equal([]byte(a), []byte(b)) }

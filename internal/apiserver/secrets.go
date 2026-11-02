package apiserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	secretsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/secrets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/object"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/authz"
	"github.com/YukinaMochizuki/tower/internal/secrets"
	"github.com/YukinaMochizuki/tower/internal/store"
)

// SecretReady says whether the apiserver can open the secret's data, which
// it can for every secret it stored: one it cannot stops it at start.
const SecretReady = "Ready"

// errNoKEK refuses writing secrets on an apiserver without a KEK.
var errNoKEK = errors.New("secrets are not set up on this apiserver: it has no key-encryption key (--secrets-kek)")

func secretObject(s *secretsv1alpha1.Secret) string {
	m := s.GetMetadata()
	return secrets.Object(m.GetProject(), m.GetName(), m.GetUid())
}

// validateSecret checks what a caller writes: the data, every time, never
// the sealed form. Its errors never quote a value.
func validateSecret(_ context.Context, s *Server, _ authz.Caller, _, obj proto.Message) error {
	if s.cfg.Secrets == nil {
		return connect.NewError(connect.CodeFailedPrecondition, errNoKEK)
	}
	spec := obj.(*secretsv1alpha1.Secret).GetSpec()
	if spec.GetSealed() != nil || len(spec.GetSuperseded()) > 0 || spec.GetHistorySince() != nil {
		return invalid("spec.sealed, spec.superseded and spec.history_since are the apiserver's; leave them out")
	}
	if len(spec.GetData()) == 0 {
		return invalid("spec.data is write-only; supply it again: every apply carries it")
	}
	if err := secrets.Validate(spec.GetType(), spec.GetData()); err != nil {
		return invalid("spec: %v", err)
	}
	return nil
}

// sealSecret replaces the data with its sealed form, and says in the status
// what can be said without the values: the keys, the content HMAC and a
// registry-auth secret's registries, with Ready, at the generation the
// write is stored at. Data equal to what is stored, by its HMAC under the
// current KEK, keeps the stored form, so applying the same values again
// changes nothing.
func sealSecret(s *Server, old, obj proto.Message) error {
	sec := obj.(*secretsv1alpha1.Secret)
	spec := sec.GetSpec()
	data := spec.GetData()
	spec.Data = nil
	sum, err := s.cfg.Secrets.HMAC(data)
	if err != nil {
		return err
	}
	var stored *secretsv1alpha1.SealedData
	if old != nil {
		oldSpec := old.(*secretsv1alpha1.Secret).GetSpec()
		stored = oldSpec.GetSealed()
		spec.Superseded, spec.HistorySince = slices.Clone(oldSpec.GetSuperseded()), oldSpec.GetHistorySince()
	}
	same := stored.GetKekId() == s.cfg.Secrets.CurrentID() && secrets.SameContent(stored.GetContentHmac(), sum)
	if same {
		spec.Sealed = stored
	} else if spec.Sealed, err = s.cfg.Secrets.Seal(secretObject(sec), data); err != nil {
		return err
	}
	if !same && stored != nil {
		// Under an earlier KEK the HMACs differ whatever the data: whether it
		// changed is what opening it says.
		if was, err := s.cfg.Secrets.Open(secretObject(sec), stored); err != nil || !maps.EqualFunc(was, data, bytes.Equal) {
			spec.Superseded = append([]*secretsv1alpha1.SupersededData{{Sealed: stored, SupersededTime: timestamppb.New(s.clock.Now())}}, spec.Superseded...)
			// Pruned only with a new value, which bumps the generation
			// anyway; an apiserver's start prunes the rest.
			spec.Superseded, spec.HistorySince = pruneHistory(spec.GetSuperseded(), spec.GetHistorySince(), s.clock.Now())
		}
	}
	// The generation the write is stored at: the backend bumps it after
	// sealing exactly when the sealed spec differs from the stored one.
	gen := sec.GetMetadata().GetGeneration()
	if old != nil && !object.SpecEqual(old, sec) {
		gen++
	}
	status := &secretsv1alpha1.SecretStatus{
		ObservedGeneration: gen,
		Keys:               slices.Sorted(maps.Keys(data)),
		ContentHmac:        spec.GetSealed().GetContentHmac(),
	}
	if spec.GetType() == secretsv1alpha1.SecretType_SECRET_TYPE_REGISTRY_AUTH {
		status.Registries = secrets.Registries(data)
	}
	ready := &metav1.Condition{
		Type: SecretReady, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Sealed", ObservedGeneration: gen,
	}
	status.Conditions = conditions.Set(slices.Clone(sec.GetStatus().GetConditions()), ready, s.clock.Now())
	sec.Status = status
	return nil
}

// redactSecret clears a secret's values and their sealed form.
func redactSecret(obj proto.Message) {
	if spec := obj.(*secretsv1alpha1.Secret).GetSpec(); spec != nil {
		spec.Data, spec.Sealed, spec.Superseded, spec.HistorySince = nil, nil, nil, nil
	}
}

// What a secret keeps of the data it held before: the 16 newest values,
// for 90 days after each was replaced.
const (
	maxSuperseded       = 16
	supersededRetention = 90 * 24 * time.Hour
)

// pruneHistory drops from superseded, newest first, what is past its
// bounds, and moves history_since past what it dropped: a value replaced
// at T was held until T, so every value held at or after T is still kept.
func pruneHistory(superseded []*secretsv1alpha1.SupersededData, since *timestamppb.Timestamp, now time.Time) ([]*secretsv1alpha1.SupersededData, *timestamppb.Timestamp) {
	kept := superseded[:0:0]
	for i, d := range superseded {
		if i < maxSuperseded && now.Sub(d.GetSupersededTime().AsTime()) <= supersededRetention {
			kept = append(kept, d)
			continue
		}
		if since == nil || d.GetSupersededTime().AsTime().After(since.AsTime()) {
			since = d.GetSupersededTime()
		}
	}
	return kept, since
}

// prepareHistory brings a stored secret's superseded data to what this
// apiserver keeps, and reports whether it changed it: each sealed again
// under the current KEK, one it cannot open or past its bounds dropped.
// A secret stored before superseded data was kept, and written more than
// once, may have held values no one kept: its history starts now.
func (s *Server) prepareHistory(sec *secretsv1alpha1.Secret) bool {
	spec := sec.GetSpec()
	changed := false
	if len(spec.GetSuperseded()) == 0 && spec.GetHistorySince() == nil && sec.GetMetadata().GetGeneration() > 1 {
		spec.HistorySince, changed = timestamppb.New(s.clock.Now()), true
	}
	var kept []*secretsv1alpha1.SupersededData
	for _, d := range spec.GetSuperseded() {
		re, ok, err := s.cfg.Secrets.Rewrap(secretObject(sec), d.GetSealed())
		switch {
		case err != nil:
			// Lost: the values it held until then are no longer kept.
			s.log.Warn("dropping a superseded value of a secret that no key-encryption key opens",
				"project", sec.GetMetadata().GetProject(), "secret", sec.GetMetadata().GetName())
			if spec.HistorySince == nil || d.GetSupersededTime().AsTime().After(spec.GetHistorySince().AsTime()) {
				spec.HistorySince = d.GetSupersededTime()
			}
			changed = true
			continue
		case ok:
			d = &secretsv1alpha1.SupersededData{Sealed: re, SupersededTime: d.GetSupersededTime()}
			changed = true
		}
		kept = append(kept, d)
	}
	pruned, since := pruneHistory(kept, spec.GetHistorySince(), s.clock.Now())
	changed = changed || len(pruned) != len(kept)
	spec.Superseded, spec.HistorySince = pruned, since
	return changed
}

// OpenSecretHistory returns every value a secret held at or after since,
// the current one first, all read from one stored object, for the
// apiserver's own use alone, as OpenSecret. complete is false when one of
// them is no longer kept: one it dropped, or one a secret of the same name
// deleted since held, which this secret, made after since, never had.
func (s *Server) OpenSecretHistory(ctx context.Context, key resource.Key, since time.Time) (values []map[string][]byte, complete bool, err error) {
	sec, current, err := s.openSecret(ctx, key)
	if err != nil {
		return nil, false, err
	}
	values = append(values, current)
	for _, d := range sec.GetSpec().GetSuperseded() {
		if d.GetSupersededTime().AsTime().Before(since) {
			continue
		}
		data, err := s.cfg.Secrets.Open(secretObject(sec), d.GetSealed())
		if err != nil {
			return nil, false, fmt.Errorf("secret %s in project %s, as it was until %s: %w", key.Name, key.Project, d.GetSupersededTime().AsTime().Format(time.RFC3339), err)
		}
		values = append(values, data)
	}
	hist := sec.GetSpec().GetHistorySince()
	complete = (hist == nil || !since.Before(hist.AsTime())) && !sec.GetMetadata().GetCreateTime().AsTime().After(since)
	return values, complete, nil
}

// OpenSecret returns the values of a secret, for the apiserver's own use
// alone: it is never served over the API, and refuses any caller but the
// system. A secret that is gone or being deleted is NotFound.
func (s *Server) OpenSecret(ctx context.Context, key resource.Key) (map[string][]byte, error) {
	_, data, err := s.openSecret(ctx, key)
	return data, err
}

// openSecret is OpenSecret, with the stored secret it opened.
func (s *Server) openSecret(ctx context.Context, key resource.Key) (*secretsv1alpha1.Secret, map[string][]byte, error) {
	if p, ok := auth.FromContext(ctx); !ok || p.Type != auth.System {
		return nil, nil, denied("only the apiserver itself opens a secret's values")
	}
	if s.cfg.Secrets == nil {
		return nil, nil, connect.NewError(connect.CodeFailedPrecondition, errNoKEK)
	}
	obj, err := s.get(ctx, registry.Secret, key)
	if err != nil {
		return nil, nil, apiError(err)
	}
	if obj == nil || obj.(*secretsv1alpha1.Secret).GetMetadata().GetDeleteTime() != nil {
		return nil, nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("secret %q in project %q not found", key.Name, key.Project))
	}
	sec := obj.(*secretsv1alpha1.Secret)
	data, err := s.cfg.Secrets.Open(secretObject(sec), sec.GetSpec().GetSealed())
	if err != nil {
		return nil, nil, fmt.Errorf("secret %s in project %s: %w", key.Name, key.Project, err)
	}
	return sec, data, nil
}

// PrepareSecrets checks, before the apiserver serves, that it can open
// every stored secret, and seals again under the current KEK those a
// previous one sealed. It fails when there are secrets and no KEK, or a
// secret the KEKs do not open: the KEK is wrong or missing. Nothing is
// written until every secret has opened.
func (s *Server) PrepareSecrets(ctx context.Context) error {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Secret.Kind})
	if err != nil {
		return err
	}
	if len(objs) > 0 && s.cfg.Secrets == nil {
		return fmt.Errorf("the store holds %d secrets, and this apiserver has no key-encryption key to open them (--secrets-kek)", len(objs))
	}
	b := s.backend(registry.Secret)
	stored := make([]*secretsv1alpha1.Secret, len(objs))
	for i, o := range objs {
		obj, err := b.decode(o)
		if err != nil {
			return err
		}
		sec := obj.(*secretsv1alpha1.Secret)
		// Opening is the check: a key of the right id may still be the
		// wrong key.
		if _, err := s.cfg.Secrets.Open(secretObject(sec), sec.GetSpec().GetSealed()); err != nil {
			return fmt.Errorf("secret %s in project %s: %w: the key-encryption key is wrong, or a previous one is missing (--secrets-kek-previous)", o.Name, o.Project, err)
		}
		stored[i] = sec
	}
	rewrapped := 0
	for i, sec := range stored {
		o := objs[i]
		re, ok, err := s.cfg.Secrets.Rewrap(secretObject(sec), sec.GetSpec().GetSealed())
		if err != nil {
			return fmt.Errorf("sealing secret %s in project %s again: %w", o.Name, o.Project, err)
		}
		history := s.prepareHistory(sec)
		if !ok && !history {
			continue
		}
		if ok {
			// The data is the same, so the spec's generation stays; its HMAC
			// is under the current KEK now.
			sec.Spec.Sealed = re
			if sec.Status != nil {
				sec.Status.ContentHmac = re.GetContentHmac()
			}
		}
		data, err := encode(sec)
		if err != nil {
			return err
		}
		if _, err := s.store.Update(ctx, o.Key, data, o.Revision); err != nil {
			return fmt.Errorf("sealing secret %s in project %s again: %w", o.Name, o.Project, err)
		}
		rewrapped++
	}
	if rewrapped > 0 {
		s.log.Info("sealed secrets again under the current key-encryption key", "secrets", rewrapped, "kek", s.cfg.Secrets.CurrentID())
	}
	return nil
}

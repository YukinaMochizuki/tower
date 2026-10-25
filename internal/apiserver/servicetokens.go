package apiserver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	iamv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/object"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/auth/session"
	"github.com/YukinaMochizuki/tower/internal/authz"
)

// A service token's secret lasts DefaultTokenDuration unless its creator
// asks otherwise, and between MinTokenDuration and MaxTokenDuration.
const (
	DefaultTokenDuration = 365 * 24 * time.Hour
	MinTokenDuration     = time.Hour
	MaxTokenDuration     = 3 * DefaultTokenDuration

	// tokenUseEvery is how stale a token's status.last_use_time may get.
	tokenUseEvery = time.Hour
	// tokenSecretPrefixLen is how much of a secret status.secret_prefix
	// shows: twr_ and 8 of its 43 random characters.
	tokenSecretPrefixLen = 12
	// maxTokenDescription bounds spec.description.
	maxTokenDescription = 256
)

// tokenPrincipal is who a service token authenticates as:
// serviceToken:<name>.
func tokenPrincipal(key resource.Key) auth.Principal {
	return auth.Principal{Type: auth.ServiceToken, ID: key.Name}
}

func validateServiceToken(_ context.Context, _ *Server, _ authz.Caller, _, obj proto.Message) error {
	spec := obj.(*iamv1alpha1.ServiceToken).GetSpec()
	if d := spec.GetDescription(); len(d) > maxTokenDescription {
		return invalid("spec.description is %d bytes; at most %d", len(d), maxTokenDescription)
	}
	switch role := spec.GetRole(); role {
	case iamv1alpha1.Role_ROLE_UNSPECIFIED:
		return invalid("spec.role is required: ROLE_ADMIN, ROLE_WORKSTREAM or ROLE_HOSTD")
	case iamv1alpha1.Role_ROLE_HOSTD:
		if spec.GetTarget() == "" {
			return invalid("spec.target is required for ROLE_HOSTD: the Target the hostd agent acts for")
		}
		if err := ValidName(spec.GetTarget()); err != nil {
			return invalid("spec.target: %v", err)
		}
	case iamv1alpha1.Role_ROLE_ADMIN, iamv1alpha1.Role_ROLE_WORKSTREAM:
		if spec.GetTarget() != "" {
			return invalid("spec.target is for ROLE_HOSTD alone; leave it out for %s", role)
		}
	default:
		return invalid("spec.role %d is not a known role", role)
	}
	return nil
}

// roleOf is the role the service token of the name holds now, and its
// target: none if it is gone or being deleted.
func (s *Server) roleOf(ctx context.Context, name string) (iamv1alpha1.Role, string, error) {
	o, err := s.get(ctx, registry.ServiceToken, resource.Key{Name: name})
	if err != nil || o == nil {
		return iamv1alpha1.Role_ROLE_UNSPECIFIED, "", err
	}
	tok := o.(*iamv1alpha1.ServiceToken)
	if tok.GetMetadata().GetDeleteTime() != nil {
		return iamv1alpha1.Role_ROLE_UNSPECIFIED, "", nil
	}
	return tok.GetSpec().GetRole(), tok.GetSpec().GetTarget(), nil
}

// revokeServiceToken revokes a deleted token's secret, at once. It runs
// after the delete is stored, so an issue racing it either sees the token
// being deleted, and revokes what it issued itself (issueTokenSecret), or
// issued before, and is revoked here.
func revokeServiceToken(ctx context.Context, s *Server, key resource.Key) error {
	if s.sessions == nil {
		return nil
	}
	n, err := s.sessions.RevokePrincipal(ctx, tokenPrincipal(key))
	if err != nil {
		return fmt.Errorf("revoking the secret of service token %s: %w", key.Name, err)
	}
	if n > 0 {
		s.log.Info("revoked a deleted service token's secret", "name", key.Name)
	}
	return nil
}

// serviceTokenLive says whether the service token of the uid stands: it
// exists under the principal's name, with that uid, and is not being
// deleted. A secret is refused otherwise, even if a revoke of it failed, so
// it never outlives its token nor passes to a later one of the same name.
func (s *Server) serviceTokenLive(ctx context.Context, p auth.Principal, uid string) (bool, error) {
	if p.Type != auth.ServiceToken || !validName(p.ID) {
		return false, nil
	}
	o, err := s.get(ctx, registry.ServiceToken, resource.Key{Name: p.ID})
	if err != nil || o == nil {
		return false, err
	}
	meta := object.Meta(o)
	return meta.GetUid() == uid && meta.GetDeleteTime() == nil, nil
}

// tokenDuration reads how long a secret should last, a year if unset.
func tokenDuration(d *durationpb.Duration) (time.Duration, error) {
	if d == nil {
		return DefaultTokenDuration, nil
	}
	if !d.IsValid() {
		return 0, invalid("duration is not a valid duration")
	}
	ttl := d.AsDuration()
	if ttl < MinTokenDuration || ttl > MaxTokenDuration {
		return 0, invalid("duration %s is out of range: at least %s, at most %s (three years)", ttl, MinTokenDuration, MaxTokenDuration)
	}
	return ttl, nil
}

// tokenIssuer serves ServiceTokenIssuerService.
type tokenIssuer struct{ s *Server }

// mayIssue refuses whoever may not issue secrets, naming only what the
// request named.
func (h tokenIssuer) mayIssue(ctx context.Context, key resource.Key) (authz.Caller, error) {
	b := h.s.backend(registry.ServiceToken)
	p, err := b.begin(ctx)
	if err != nil {
		return p, err
	}
	if !authz.MayIssueTokens(p) {
		return p, denied("%s may not issue the secret of %s", p, b.asked(key))
	}
	return p, b.checkKey(key)
}

func (h tokenIssuer) CreateServiceToken(ctx context.Context, req *connect.Request[iamv1alpha1.CreateServiceTokenRequest]) (*connect.Response[iamv1alpha1.CreateServiceTokenResponse], error) {
	s := h.s
	msg := req.Msg
	key := resource.Key{Name: msg.GetName()}
	p, err := h.mayIssue(ctx, key)
	if err != nil {
		return nil, err
	}
	ttl, err := tokenDuration(msg.GetDuration())
	if err != nil {
		return nil, err
	}
	if s.sessions == nil {
		return nil, failedPrecondition("this apiserver keeps no credentials, so it issues no service tokens")
	}
	defer s.tokenSecrets.lock(key.Name)()

	// Created as the caller, checked as any apply of a token is.
	res, err := s.backend(registry.ServiceToken).Create(ctx, &iamv1alpha1.ServiceToken{
		Metadata: &metav1.ObjectMeta{Name: key.Name},
		Spec:     &iamv1alpha1.ServiceTokenSpec{Description: msg.GetDescription(), Role: msg.GetRole(), Target: msg.GetTarget()},
	})
	if err != nil {
		return nil, err
	}
	uid := res.Object.(*iamv1alpha1.ServiceToken).GetMetadata().GetUid()
	// From here on, a failure deletes the token again, which revokes
	// whatever was issued.
	ctx = context.WithoutCancel(ctx)
	issued, tok, err := s.issueTokenSecret(ctx, key, uid, ttl)
	if err != nil {
		if _, derr := s.backend(registry.ServiceToken).Delete(s.system(ctx), key, nil); derr != nil && connect.CodeOf(derr) != connect.CodeNotFound {
			s.log.Error("deleting a service token whose creation failed", "name", key.Name, "error", derr)
		}
		return nil, err
	}
	s.log.Info("created a service token", "name", key.Name, "role", msg.GetRole().String(), "by", p.String(), "expires", issued.ExpiresAt)
	return connect.NewResponse(&iamv1alpha1.CreateServiceTokenResponse{ServiceToken: tok, Secret: issued.Secret.Reveal()}), nil
}

func (h tokenIssuer) RotateServiceToken(ctx context.Context, req *connect.Request[iamv1alpha1.RotateServiceTokenRequest]) (*connect.Response[iamv1alpha1.RotateServiceTokenResponse], error) {
	s := h.s
	msg := req.Msg
	b := s.backend(registry.ServiceToken)
	key := resource.Key{Name: msg.GetName()}
	p, err := h.mayIssue(ctx, key)
	if err != nil {
		return nil, err
	}
	ttl, err := tokenDuration(msg.GetDuration())
	if err != nil {
		return nil, err
	}
	if s.sessions == nil {
		return nil, failedPrecondition("this apiserver keeps no credentials, so it issues no service tokens")
	}
	defer s.tokenSecrets.lock(key.Name)()
	o, err := s.get(ctx, registry.ServiceToken, key)
	if err != nil {
		return nil, apiError(err)
	}
	if o == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s not found", b.asked(key)))
	}
	if object.Meta(o).GetDeleteTime() != nil {
		return nil, failedPrecondition("%s is being deleted", b.asked(key))
	}
	issued, tok, err := s.issueTokenSecret(context.WithoutCancel(ctx), key, object.Meta(o).GetUid(), ttl)
	if err != nil {
		return nil, err
	}
	s.log.Info("rotated a service token", "name", key.Name, "by", p.String(), "expires", issued.ExpiresAt)
	return connect.NewResponse(&iamv1alpha1.RotateServiceTokenResponse{ServiceToken: tok, Secret: issued.Secret.Reveal()}), nil
}

// issueTokenSecret issues a secret for the token of the uid, in place of any
// it had, and records it in the token's status. If the token is gone, or
// being deleted, by then, it revokes the secret again and fails: a delete
// stored before the secret was revokes it itself (revokeServiceToken), and
// one stored after is seen here. The caller holds tokenSecrets.
func (s *Server) issueTokenSecret(ctx context.Context, key resource.Key, uid string, ttl time.Duration) (session.ServiceToken, *iamv1alpha1.ServiceToken, error) {
	p := tokenPrincipal(key)
	issued, err := s.sessions.IssueService(ctx, p, uid, ttl)
	if err != nil {
		return session.ServiceToken{}, nil, apiError(err)
	}
	tok, err := s.updateTokenStatus(ctx, key, uid, func(st *iamv1alpha1.ServiceTokenStatus) bool {
		st.SecretPrefix = issued.Secret.Reveal()[:tokenSecretPrefixLen]
		st.IssueTime = timestamppb.New(issued.IssuedAt)
		st.ExpireTime = timestamppb.New(issued.ExpiresAt)
		return true
	})
	if err == nil && tok.GetMetadata().GetDeleteTime() != nil {
		err = failedPrecondition("service token %q is being deleted", key.Name)
	}
	if err != nil {
		if _, rerr := s.sessions.RevokePrincipal(ctx, p); rerr != nil {
			s.log.Error("revoking a service token's secret issued as it was deleted", "name", key.Name, "error", rerr)
		}
		return session.ServiceToken{}, nil, err
	}
	return issued, tok, nil
}

// errTokenGone means the token of the uid is no longer there.
var errTokenGone = errors.New("the service token is gone")

// updateTokenStatus changes the status of the token of the uid as the
// system, with change, which reports whether there is anything to write.
func (s *Server) updateTokenStatus(ctx context.Context, key resource.Key, uid string, change func(*iamv1alpha1.ServiceTokenStatus) bool) (*iamv1alpha1.ServiceToken, error) {
	b := s.backend(registry.ServiceToken)
	for range maxRetries {
		o, err := s.get(ctx, registry.ServiceToken, key)
		if err != nil {
			return nil, apiError(err)
		}
		if o == nil || object.Meta(o).GetUid() != uid {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("service token %q: %w", key.Name, errTokenGone))
		}
		tok := o.(*iamv1alpha1.ServiceToken)
		st := proto.Clone(tok.GetStatus()).(*iamv1alpha1.ServiceTokenStatus)
		if st == nil {
			st = &iamv1alpha1.ServiceTokenStatus{}
		}
		if !change(st) {
			return tok, nil
		}
		st.ObservedGeneration = tok.GetMetadata().GetGeneration()
		rv := tok.GetMetadata().GetResourceVersion()
		out, err := b.UpdateStatus(s.system(ctx), key, st, &rv)
		if connect.CodeOf(err) == connect.CodeAborted {
			continue
		}
		if err != nil {
			return nil, err
		}
		return out.(*iamv1alpha1.ServiceToken), nil
	}
	return nil, connect.NewError(connect.CodeAborted, errors.New("too many concurrent writers; retry"))
}

// tokenUse is one use of a service token, as session.Manager reports it.
type tokenUse struct {
	p  auth.Principal
	at time.Time
}

// serviceTokenUsed hears of a service token's use, without blocking the
// call that used it: a use that finds the queue full is dropped, and the
// next one, a minute later, records it.
func (s *Server) serviceTokenUsed(p auth.Principal, at time.Time) {
	select {
	case s.tokenUses <- tokenUse{p, at}:
	default:
	}
}

// recordTokenUses writes the uses of service tokens to their status, at
// most one write per token an hour, until ctx is done.
func (s *Server) recordTokenUses(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case u := <-s.tokenUses:
			if !validName(u.p.ID) {
				continue
			}
			key := resource.Key{Name: u.p.ID}
			o, err := s.get(ctx, registry.ServiceToken, key)
			if err != nil || o == nil {
				continue
			}
			_, err = s.updateTokenStatus(ctx, key, object.Meta(o).GetUid(), func(st *iamv1alpha1.ServiceTokenStatus) bool {
				if last := st.GetLastUseTime(); last != nil && u.at.Sub(last.AsTime()) < tokenUseEvery {
					return false
				}
				st.LastUseTime = timestamppb.New(u.at)
				return true
			})
			if err != nil && ctx.Err() == nil && !errors.Is(err, errTokenGone) {
				s.log.Warn("could not record a service token's use", "name", key.Name, "error", err)
			}
		}
	}
}

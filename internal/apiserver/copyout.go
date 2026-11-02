package apiserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/authz"
	"github.com/YukinaMochizuki/tower/internal/provider"
)

// cleanupTimeout bounds the removal of a refused copy's partial files,
// which runs even once the call has ended.
const cleanupTimeout = 30 * time.Second

// unpackScript unpacks the tar $1 into the directory $1.d and puts its $3
// at $2. A directory onto a directory merges into it, as an extraction
// would; anything else replaces what is at $2, a symbolic link there
// itself and never what it names. It leaves nothing at $1 nor $1.d. tar
// reads the part on stdin, where it unpacks nothing compressed.
const unpackScript = `p=$1 t=$2 b=$3 d=$1.d
rm -rf -- "$d" && mkdir -- "$d" && tar -xf - -C "$d" < "$p" && rm -f -- "$p" || exit
if [ -d "$t" ] && [ ! -L "$t" ] && [ -d "$d/$b" ] && [ ! -L "$d/$b" ]; then
	cp -a -- "$d/$b/." "$t/"
elif [ -d "$t" ] && [ ! -L "$t" ]; then
	echo "a directory is there" >&2; false
else
	rm -f -- "$t" && mv -f -- "$d/$b" "$t"
fi
r=$?; rm -rf -- "$d"; exit $r`

// copyScanned copies through tower, for refuse_secrets or max_bytes: a
// tar of the source streams through a check that it is a plain tar of the
// path and a scanner into a file of a random name beside the destination,
// unpacked into its place only once all of it passed. A refusal or any
// failure leaves nothing at the destination. On a machine both ends run
// as its run_as user (docs/decisions/copy-out-scan.md).
func (h environmentService) copyScanned(ctx context.Context, c authz.Caller, from, to *environment, m *envv1alpha1.CopyFileRequest) error {
	start := h.s.clock.Now()
	fromPath, toPath := path.Clean("/"+m.GetFromPath()), path.Clean("/"+m.GetToPath())
	if fromPath == "/" || toPath == "/" {
		return invalid("a scanned copy copies a path under /, to a path under /")
	}
	base := path.Base(fromPath)
	var patterns []scanPattern
	if m.GetRefuseSecrets() {
		secrets, unopened := h.s.sourceSecrets(ctx, from)
		var tooShort []string
		patterns, tooShort = secretPatterns(secrets)
		slices.Sort(tooShort)
		if bad := slices.Compact(append(slices.Compact(tooShort), unopened...)); len(bad) > 0 {
			r := &envv1alpha1.CopyRefusal{Reason: envv1alpha1.CopyRefusalReason_COPY_REFUSAL_REASON_UNSCANNABLE, Secrets: bad}
			h.logCopy(c, from, to, m, start, 0, r)
			return copyRefused(r, "%s was given secrets a copy out of it cannot be scanned for: %s (shorter than %d bytes, not to be opened, or replaced longer ago than tower keeps)",
				from, strings.Join(bad, ", "), minScannableSecret)
		}
		if allowed := m.GetAllowSuspectFor(); len(allowed) > 0 {
			patterns = slices.DeleteFunc(patterns, func(p scanPattern) bool { return !p.strong && slices.Contains(allowed, p.name) })
		}
	}
	part := toPath + ".part-" + strings.ToLower(rand.Text()[:16])
	// On a machine the part is written as root, then, with the directories
	// the write made, handed to its run_as user, who unpacks it.
	give := ""
	if to.runAs != "" {
		var err error
		if give, err = to.firstMissing(ctx, part); err != nil {
			return err
		}
	}

	pr, pw := io.Pipe()
	sc := newScanner(patterns, m.GetMaxBytes(), pw)
	check := newTarCheck(base)
	written := make(chan error, 1)
	go func() {
		err := to.p.WriteFile(ctx, to.ref, part, 0o600, 0, pr)
		pr.CloseWithError(cmpErr(err, errStopped))
		written <- err
	}()
	var stderr tailBuffer
	code, err := from.p.Exec(ctx, provider.ExecRequest{
		Instance: from.ref, Command: from.asRunAs([]string{"tar", "-cf", "-", "-C", path.Dir(fromPath), "--", base}, "/"),
		Dir: "/", Stdout: teeWriter{check, sc}, Stderr: &stderr,
	})
	checkErr := check.Close()
	if sc.refusal() == nil && checkErr == nil && err == nil && code == 0 {
		err = sc.Flush()
	}
	if sc.refusal() != nil || checkErr != nil || err != nil || code != 0 {
		pw.CloseWithError(errStopped)
	} else {
		_ = pw.Close()
	}
	werr := <-written
	fail := func(stage string, code int, err error) error {
		h.removePart(ctx, to, part)
		h.s.log.Info("copy failed", "caller", c.String(), "from", from.String(), "to", to.String(), "stage", stage, "exit_code", code, "bytes", sc.n)
		return err
	}
	switch refusal := sc.refusal(); {
	case refusal != nil:
		h.removePart(ctx, to, part)
		h.logCopy(c, from, to, m, start, sc.n, refusal)
		return refusalError(refusal, from)
	case check.err != nil:
		// Refused while it streamed: what tar did after says nothing.
		return h.notATar(ctx, c, from, to, m, start, sc.n, part)
	case werr != nil && !errors.Is(werr, errStopped):
		// The destination failed first, and so the stream into it.
		return fail("write", 0, providerError(to, werr))
	case err != nil:
		return fail("tar", 0, providerError(from, err))
	case code != 0:
		// tar's words may hold a name, which may hold a secret: they are
		// read for whether the path is there, and go nowhere.
		if strings.Contains(stderr.String(), "No such file") {
			return fail("tar", code, connect.NewError(connect.CodeNotFound, fmt.Errorf("copy failed: tar exited %d: no such file or directory", code)))
		}
		return fail("tar", code, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("copy failed: tar exited %d", code)))
	case checkErr != nil:
		return h.notATar(ctx, c, from, to, m, start, sc.n, part)
	case werr != nil:
		return fail("write", 0, providerError(to, werr))
	}
	if give != "" {
		if err := to.giveToRunAs(ctx, h.s, give); err != nil {
			return fail("give", 0, err)
		}
	}
	code, err = to.p.Exec(ctx, provider.ExecRequest{
		Instance: to.ref, Command: to.asRunAs([]string{"sh", "-c", unpackScript, "sh", part, toPath, base}, "/"),
		Dir: "/", Stdout: io.Discard, Stderr: io.Discard,
	})
	switch {
	case err != nil:
		return fail("unpack", 0, providerError(to, err))
	case code != 0:
		return fail("unpack", code, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("copy failed: unpack exited %d", code)))
	}
	h.logCopy(c, from, to, m, start, sc.n, nil)
	return nil
}

// notATar refuses a copy whose source sent what is not a plain tar of the
// path: what the destination would unpack is not what was scanned.
func (h environmentService) notATar(ctx context.Context, c authz.Caller, from, to *environment, m *envv1alpha1.CopyFileRequest, start time.Time, n int64, part string) error {
	r := &envv1alpha1.CopyRefusal{Reason: envv1alpha1.CopyRefusalReason_COPY_REFUSAL_REASON_UNSCANNABLE}
	h.removePart(ctx, to, part)
	h.logCopy(c, from, to, m, start, n, r)
	return copyRefused(r, "what %s sent is not a plain tar of the path asked for, so it cannot be scanned", from)
}

// teeWriter writes to both: what the check refuses goes no further.
type teeWriter struct{ a, b io.Writer }

func (t teeWriter) Write(p []byte) (int, error) {
	if _, err := t.a.Write(p); err != nil {
		return 0, err
	}
	return t.b.Write(p)
}

// removePart removes what a copy that did not finish may have left beside
// its destination, even once the call has ended: its part, the directory
// it is unpacked in, and the file the guest writes the part through.
func (h environmentService) removePart(ctx context.Context, to *environment, part string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	for _, command := range [][]string{
		{"rm", "-rf", "--", part, part + ".d"},
		{"find", path.Dir(part), "-maxdepth", "1", "-name", "." + globEscape(path.Base(part)) + ".tower-*", "-delete"},
	} {
		code, err := to.p.Exec(ctx, provider.ExecRequest{Instance: to.ref, Command: command, Dir: "/"})
		if err != nil || code != 0 {
			h.s.log.Warn("copy: removing a partial copy", "environment", to.String(), "program", command[0], "exit_code", code, "failed", err != nil)
		}
	}
}

// globEscape is s matching itself alone in a find -name pattern.
func globEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`*?[]\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// logCopy logs a scanned copy: never its paths, which the caller chose,
// nor anything of what it copied.
func (h environmentService) logCopy(c authz.Caller, from, to *environment, m *envv1alpha1.CopyFileRequest, start time.Time, n int64, r *envv1alpha1.CopyRefusal) {
	attrs := []any{"caller", c.String(), "from", from.String(), "to", to.String(), "refuse_secrets", m.GetRefuseSecrets(),
		"allow_suspect_for", m.GetAllowSuspectFor(), "max_bytes", m.GetMaxBytes(), "bytes", n, "took", h.s.clock.Since(start)}
	if r != nil {
		attrs = append(attrs, "refused", refusalName(r.GetReason()), "secrets", r.GetSecrets(), "form", r.GetForm())
	}
	h.s.log.Info("copy scanned", attrs...)
}

// copyCapabilities are GetCapabilities' answer.
var copyCapabilities = []string{"copy.refuse_secrets.v1"}

func (h environmentService) GetCapabilities(ctx context.Context, _ *connect.Request[envv1alpha1.GetCapabilitiesRequest]) (*connect.Response[envv1alpha1.GetCapabilitiesResponse], error) {
	c, err := h.s.begin(ctx)
	if err != nil {
		return nil, err
	}
	if !authz.MayUseEnvironments(c) {
		return nil, denied("%s may not use environments: only ROLE_WORKSTREAM does", c)
	}
	return connect.NewResponse(&envv1alpha1.GetCapabilitiesResponse{Capabilities: slices.Clone(copyCapabilities)}), nil
}

// refusal is why the scan stopped the stream, nil if it did not.
// A strong secret found decides over a weak one, and either over the
// size, as the stream read past a weak one until it ended or passed max.
func (s *scanner) refusal() *envv1alpha1.CopyRefusal {
	switch {
	case s.stop != nil && s.stop.pattern != nil:
		return &envv1alpha1.CopyRefusal{
			Reason: envv1alpha1.CopyRefusalReason_COPY_REFUSAL_REASON_SECRET_FOUND, Secrets: []string{s.stop.pattern.name}, Form: s.stop.pattern.form,
		}
	case s.suspect != nil:
		return &envv1alpha1.CopyRefusal{
			Reason: envv1alpha1.CopyRefusalReason_COPY_REFUSAL_REASON_SUSPECT, Secrets: []string{s.suspect.name}, Form: s.suspect.form,
		}
	case s.stop != nil:
		return &envv1alpha1.CopyRefusal{Reason: envv1alpha1.CopyRefusalReason_COPY_REFUSAL_REASON_TOO_LARGE, MaxBytes: s.stop.max}
	}
	return nil
}

// refusalError says why a copy out of from was refused: never a path,
// which the caller chose and may hold anything.
func refusalError(r *envv1alpha1.CopyRefusal, from *environment) error {
	form := ""
	if r.GetForm() != "raw" {
		form = " (" + r.GetForm() + ")"
	}
	switch r.GetReason() {
	case envv1alpha1.CopyRefusalReason_COPY_REFUSAL_REASON_TOO_LARGE:
		return copyRefused(r, "the copy out of %s passed %d bytes", from, r.GetMaxBytes())
	case envv1alpha1.CopyRefusalReason_COPY_REFUSAL_REASON_SUSPECT:
		return copyRefused(r, "the copy out of %s holds a credential-shaped piece of %s%s, a secret it was given; a human should look", from, r.GetSecrets()[0], form)
	}
	return copyRefused(r, "the copy out of %s holds the value of %s%s, a secret it was given", from, r.GetSecrets()[0], form)
}

// copyRefused is a refusal of a scanned copy: FAILED_PRECONDITION, its
// message starting with "copy refused: " and the reason, and r its
// detail.
func copyRefused(r *envv1alpha1.CopyRefusal, format string, args ...any) error {
	ce := connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("copy refused: %s: "+format, append([]any{refusalName(r.GetReason())}, args...)...))
	if d, err := connect.NewErrorDetail(r); err == nil {
		ce.AddDetail(d)
	}
	return ce
}

func refusalName(r envv1alpha1.CopyRefusalReason) string {
	return strings.TrimPrefix(r.String(), "COPY_REFUSAL_REASON_")
}

// givenSecret is one value of a Secret tower gives an environment, and
// what a refusal calls it.
type givenSecret struct{ name, secret, key string }

// sourceSecrets are the secret values tower gave e, each named as a
// refusal names it: every value each Secret held since e was made, as a
// value given then may be on its disk still. unopened names those it
// cannot open now, or of which a value it may have given is no longer
// kept (docs/decisions/copy-out-scan.md).
func (s *Server) sourceSecrets(ctx context.Context, e *environment) (secrets []namedSecret, unopened []string) {
	var given []givenSecret
	var made time.Time
	if e.box != nil {
		made = e.box.GetMetadata().GetCreateTime().AsTime()
		for _, ref := range e.box.GetSpec().GetPrivilegedSecrets() {
			given = append(given, givenSecret{"$" + ref.GetAs(), ref.GetSecret(), ref.GetKey()})
		}
		for _, f := range e.box.GetSpec().GetSecretFiles() {
			given = append(given, givenSecret{f.GetPath(), f.GetSecret(), f.GetKey()})
		}
	}
	if e.machine != nil {
		made = e.machine.GetMetadata().GetCreateTime().AsTime()
		for _, ref := range e.machine.GetSpec().GetSecrets() {
			given = append(given, givenSecret{"$" + ref.GetAs(), ref.GetSecret(), ref.GetKey()})
		}
	}
	type history struct {
		values   []map[string][]byte
		complete bool
	}
	opened := map[string]*history{}
	for _, g := range given {
		h, ok := opened[g.secret]
		if !ok {
			values, complete, err := s.OpenSecretHistory(s.system(ctx), resource.Key{Project: e.key.Project, Name: g.secret}, made)
			if err != nil {
				values, complete = nil, false
			}
			h = &history{values, complete}
			opened[g.secret] = h
		}
		found := false
		for _, data := range h.values {
			if v, ok := data[g.key]; ok {
				secrets = append(secrets, namedSecret{name: g.name, value: v})
				found = true
			}
		}
		if !h.complete || !found {
			unopened = append(unopened, g.name)
		}
	}
	slices.Sort(unopened)
	return secrets, slices.Compact(unopened)
}

// errStopped ends the stream into a copy's destination early: the copy
// was refused, or its source failed.
var errStopped = errors.New("copy stopped")

// cmpErr is err, or def for none.
func cmpErr(err, def error) error {
	if err != nil {
		return err
	}
	return def
}

// tailBuffer keeps the last bytes written to it, for an error to quote.
type tailBuffer struct{ b []byte }

const tailBufferSize = 512

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > tailBufferSize {
		t.b = t.b[len(t.b)-tailBufferSize:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(bytes.TrimSpace(t.b)) }

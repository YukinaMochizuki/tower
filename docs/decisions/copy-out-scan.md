# A copy can be scanned for the source's secrets

Status: accepted, 2026-11.

## Context

workstream lets an agent copy a file out of a privileged box into an
ordinary box of the same workstream without asking a human, but only when
what is copied holds none of the secrets the privileged box was given. It
can scan for them only if it knows them, and for a product on tower it does
not: the values are tower's and never leave it (`secrets.md`). Without a
scan in tower, every copy out of a privileged box on tower asks a human.

## Decision

- **`CopyFile` takes `refuse_secrets` and `max_bytes`.** With either set,
  tower does not use the target's copy, whose bytes it never sees
  (`traffic.md`). It streams `tar -cf` of the source through a check and a
  scanner into `<to_path>.part-<random>` in the destination, and unpacks
  it into place only once the whole stream has passed. A refusal, a
  failure, or a call that ends midway removes the part, the directory it
  is unpacked in, and the temporary file the guest writes it through.
  Without either option the copy stays on the target, as before.
- **The stream must be a plain tar of the path.** tower reads it as a tar
  as it passes: ustar in its first header (so nothing compressed, which
  the destination's tar would quietly unpack), every entry the path or
  under it, none through a link the tar made, and only files,
  directories, links and hard links within it. Anything else is
  `UNSCANNABLE`: what would be unpacked is not what was scanned.
- **The unpack never writes through a link at `to_path`**: a directory
  onto a directory merges, anything else replaces what is there, a link
  itself. On a machine with a `run_as` user both tars run as that user,
  and the part is handed to them, so neither end reaches what the user
  could not.
- **The secrets are every value tower gave the source**: a privileged
  box's values and secret files, a machine's blueprint secrets, each with
  every value its Secret held since the environment was made, since one
  given then may still be on its disk. A Secret keeps the values it held
  before, sealed as its current one is, for 90 days and 16 values; past
  that, its `history_since` says from when it holds them all. A source made
  before that is `UNSCANNABLE`, naming the secret, as is one made before
  its Secret was (a Secret deleted and made again under the name holds
  none of the old one's values) and one whose Secret is gone. A Secret
  written more than once before this was kept starts its history when an
  apiserver that keeps it first starts.
- **Each needle is looked for in many forms**: hex, base64 and base64url
  at every byte alignment; URL-encoded as JavaScript's
  `encodeURIComponent`, Go's query, path and userinfo escaping, every byte
  but an unreserved one (Python's `quote(safe="")`, curl) and Python's
  default `quote`, which keeps `/`, each with upper- and lower-case
  escapes; and JSON-escaped, as JavaScript writes it, with `\/`, as Go
  writes it (`&`, `<` and `>` as `\u00XX`), and with every non-alphanumeric
  as `\uXXXX`; as well as raw. A second pass over the stream without line
  breaks finds a wrapped encoding. The scanner holds back the longest
  pattern's length, so a value split across reads is found before it is
  passed on. It is the scan workstream runs for the secrets it holds
  itself, and the two must stay the same, so a copy out is judged alike
  wherever the values are.
- **Needles come in two tiers.** A strong hit is `SECRET_FOUND`, always
  refused; a weak one is `SUSPECT`, which workstream has a human judge.
  Strong needles are what is a credential whole:
  - the value whole and trimmed;
  - a PEM block's base64 body, its lines joined, also inside JSON with its
    line breaks escaped;
  - a .netrc's `password` value, and the value of each `KEY=value` line
    (`export` before it, quotes around the value dropped) whose key's
    name holds, in any case, `PASS`, `PASSWORD`, `SECRET`, `TOKEN`, `KEY`,
    `CREDENTIAL`, `AUTH`, `PRIVATE`, `APIKEY` or `API_KEY`, whatever it
    looks like;
  - a .netrc's `login` value, any other `KEY=value` value, and each string
    a JSON value holds at any depth, unescaped, only if it looks like a
    token: credential-shaped, and neither a DNS name nor a JWT's header,
    as a weak piece must be. Else it is no needle at all, so
    `AWS_REGION=us-east-1` or `login ci-bot-2024` in a secret refuses no
    copy that names them, which as strong needles they would, with no
    human able to let it through;
  - in a JSON value, each string that is a PEM block, whole, trimmed,
    and its body.

  Weak needles are the credential-shaped pieces of a value, single-line
  included: each trimmed line, and each segment split on whitespace,
  quotes, brackets and `: / , ; = @ & ? < >`, then on `.`. A segment that
  is a DNS name (labels, the last 2 to 24 letters), as is a line, is
  skipped whole, so an EKS server's id is no needle; so is a part that is
  a JWT's header (base64url of JSON naming `alg`), which every token of
  its algorithm shares. A JSON value's strings give pieces too.

  Credential-shaped is 16 bytes at least of `[A-Za-z0-9+/=_.-]` and one
  of: hex digits mixing letters and digits; a UUID; letters of both cases
  and digits with 3 bits a byte of entropy; or 20 bytes of lower case and
  digits, no dot or slash, switching between the two at least every 8
  bytes, with 3.5 bits a byte. Structure (`apiVersion: v1`, a server URL,
  a path, a host, a dated name) is never a needle, since a structural line
  would refuse most copies of a config by chance.

  After a weak hit the scan passes nothing more on but reads to the end,
  and every window is searched for strong needles before weak ones, so a
  strong hit anywhere decides; a `SUSPECT` names the secret its piece is
  of.
- **`allow_suspect_for` lets a human's approval through, for what they
  saw.** Once a human has approved a copy that came back `SUSPECT`,
  workstream copies again naming the secrets the refusal named: their weak
  needles are dropped, every other secret's stay, and every strong one
  still refuses. Without `refuse_secrets` it does nothing.
- **A caller asks before it relies on the scan, and checks after.** An
  older tower drops `refuse_secrets` unread and copies unscanned, so
  `EnvironmentService.GetCapabilities` names `copy.refuse_secrets.v1`; a
  tower without the RPC answers UNIMPLEMENTED, which is to have none. And
  `CopyFileResponse.scanned` is true only for a copy that went through
  the scan, so a tower rolled back to one that ignores the options is
  caught on the copy it made.
- **Patterns are found by their first 8 bytes**, through a hashed bitset
  and an index: 50 MB against 1,600 patterns scans in about a third of a
  second, where a search per pattern took 30.
- **A value under 8 bytes cannot be scanned for** without refusing most
  copies by chance, so a source with one (or one tower cannot open) is
  refused before anything is read, and the caller asks a human instead.
- **A refusal is FAILED_PRECONDITION with a `CopyRefusal` detail**, and a
  message starting `copy refused: <REASON>: `: `SECRET_FOUND` or
  `SUSPECT` (naming the variable or path and the form, never the value),
  `TOO_LARGE`, or `UNSCANNABLE`. A copy with more than one is refused for
  the first of `SECRET_FOUND`, `SUSPECT`, `TOO_LARGE`: the write that
  crosses `max_bytes` is scanned up to it first. A copy that fails says
  `copy failed: tar exited N` or `copy failed: unpack exited N`, never
  what tar wrote, which names paths in the source. An error may name a
  path the caller gave, as a provider's error about writing it does;
  none holds a secret's value. The values are opened for the call alone
  and reach no error, log line, event or object; the log says the
  refusal and the secret's name, never a path.

## Consequences

- A scanned copy's bytes pass through tower, and count against what a copy
  may move: `max_bytes` bounds the tar, each entry a 512-byte header and
  its contents rounded up to 512 bytes, the whole padded to 10240-byte
  records.
- The source needs `tar`, the destination `tar`, `sh`, `cp` and `find`, as
  tower's other file operations already need a shell. The destination's
  `tar` is the box's own, and reads the part on stdin, where it unpacks
  nothing compressed even if the check let it by: the check makes sure
  what it is given is what was scanned, not that it unpacks it faithfully,
  which only matters to an agent sabotaging its own box. Unpacking through
  tower-guest's `tarx` would close that too, once every guest runs a
  tower-guest that has it.
- A copy into a root-owned directory of a machine with a `run_as` user
  fails, as that user could not write there.
- A compressed or encrypted secret is not found, nor a credential the box
  minted from what it was given: the scan is against accidents, not a
  determined agent.

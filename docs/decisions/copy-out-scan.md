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
- **Each value is looked for in many forms**: hex, base64 and base64url at
  every byte alignment, URL-encoded (query, path and userinfo escaping),
  and JSON-escaped, as well as raw; a second pass over the stream without
  line breaks finds a wrapped encoding. The scanner holds back the longest
  pattern's length, so a value split across reads is found before it is
  passed on. It is the scan workstream runs for the secrets it holds
  itself, so a copy out is judged alike wherever the values are.
- **Needles come in two tiers.** Strong: a value whole and trimmed, and a
  PEM key's base64 body, also inside JSON with its line breaks escaped; a
  hit is `SECRET_FOUND`, always refused. Weak: a line or field of a value
  made of several (a kubeconfig's token, a key's line, a .netrc's
  password), counted only when it looks like a credential: 16 bytes at
  least of a token's characters and 3 bits a byte of entropy, with letters
  of both cases and digits, 32 hex digits, a UUID, or 20 bytes of lower
  case and digits that switch between the two at least every 8 bytes, with
  no dot or slash. Structure (`apiVersion: v1`, a server URL, a path, a
  host, a dated name) is never a needle, since a structural line would
  refuse most copies of a config by chance. A weak hit is `SUSPECT`:
  workstream has a human judge it. After one, the scan passes nothing more
  on but reads to the end, so a strong hit later decides.
- **`allow_suspect_for` lets a human's approval through, for what they
  saw.** Once a human has approved a copy that came back `SUSPECT`,
  workstream copies again naming the secrets the refusal named: their weak
  needles are dropped, every other secret's stay, and every strong one
  still refuses. Without `refuse_secrets` it does nothing.
- **A caller asks before it relies on the scan.** An older tower drops
  `refuse_secrets` unread and copies unscanned, so
  `EnvironmentService.GetCapabilities` names `copy.refuse_secrets.v1`;
  a tower without the RPC answers UNIMPLEMENTED, which is to have none.
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

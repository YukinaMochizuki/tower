package fake

import (
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
)

// shell runs a command of a fake guest's small set:
//
//   - true, false, exit N
//   - echo ARGS...
//   - env: the environment given this command alone, one NAME=value a line
//   - cat [FILE...]: the files, or stdin with none
//   - emit ARG...: each "out:text" or "err:text" writes a line of text to
//     stdout or stderr, in order
//   - sh -c LINE: LINE split on spaces, run as a command
//   - children: how many children of the guest's processes are alive
//   - sleep (in Exec): until it is killed
//
// Any other command exits 0, so a guest takes any install step. The caller
// holds h.mu.
type shell struct {
	g     guests
	gu    *guest
	d     *disk
	env   map[string]string
	dir   string
	stdin []byte
	out   *output
}

// readsStdin reports whether the command reads its stdin.
func readsStdin(command []string) bool {
	if len(command) == 3 && command[0] == "sh" && command[1] == "-c" {
		command = strings.Fields(command[2])
	}
	return len(command) == 1 && command[0] == "cat"
}

func (s shell) run(command []string) int {
	if len(command) == 0 {
		return 127
	}
	stdout, stderr := s.out.stream(false), s.out.stream(true)
	name, args := command[0], command[1:]
	switch name {
	case "true":
	case "false":
		return 1
	case "exit":
		if len(args) == 1 {
			n, err := strconv.Atoi(args[0])
			if err != nil {
				return 2
			}
			return n
		}
	case "echo":
		_, _ = fmt.Fprintln(stdout, strings.Join(args, " "))
	case "emit":
		for _, a := range args {
			w := stdout
			if text, ok := strings.CutPrefix(a, "err:"); ok {
				w, a = stderr, text
			} else {
				a = strings.TrimPrefix(a, "out:")
			}
			_, _ = fmt.Fprintln(w, a)
		}
	case "env":
		for _, k := range slices.Sorted(maps.Keys(s.env)) {
			_, _ = fmt.Fprintf(stdout, "%s=%s\n", k, s.env[k])
		}
	case "cat":
		if len(args) == 0 {
			_, _ = stdout.Write(s.stdin)
		}
		for _, p := range args {
			if !path.IsAbs(p) {
				p = path.Join(cmpDir(s.dir), p)
			}
			files, k := s.g.files(s.gu, s.d, p)
			b, ok := files[k]
			if !ok {
				_, _ = fmt.Fprintf(stderr, "cat: %s: No such file or directory\n", p)
				return 1
			}
			_, _ = stdout.Write(b)
		}
	case "children":
		n := 0
		for _, p := range s.gu.Procs {
			n += p.Children
		}
		_, _ = fmt.Fprintln(stdout, n)
	case "sh":
		if len(args) == 2 && args[0] == "-c" {
			return s.run(strings.Fields(args[1]))
		}
		_, _ = io.WriteString(stderr, "sh: only sh -c LINE runs here\n")
		return 2
	}
	return 0
}

func cmpDir(dir string) string {
	if dir == "" {
		return "/"
	}
	return dir
}

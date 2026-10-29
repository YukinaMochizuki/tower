package apiserver

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"slices"
	"strings"
)

// shells are the programs whose "-c" line is a command's text: what
// commandLine runs a shell line with, and what a caller's argv may name.
var shells = []string{"sh", "/bin/sh", "/usr/bin/sh", "bash", "/bin/bash", "/usr/bin/bash"}

// commandText is a process's command as one line: a shell's -c line as it
// is, other argv joined by spaces, as ListProcesses shows it.
func commandText(argv []string) string {
	if len(argv) == 3 && slices.Contains(shells, argv[0]) && (argv[1] == "-c" || argv[1] == "-lc") {
		return argv[2]
	}
	return strings.Join(argv, " ")
}

// workspacePrefix is the cd a caller puts before a command to run it in
// /workspace, the directory it may as well have asked for.
const workspacePrefix = "cd /workspace && "

// outputFilters are the programs a trailing pipe stage may run and still
// be stripped: they shorten what a command prints, never what it does.
var outputFilters = []string{"tail", "head", "grep"}

// normalizeCommand is a command as its exact key hashes it: whitespace
// collapsed, a leading "cd /workspace &&" and, on a line of one pipeline,
// trailing output filters stripped (docs/decisions/process-usage.md).
func normalizeCommand(text string) string {
	s := strings.Join(strings.Fields(text), " ")
	if rest, ok := strings.CutPrefix(s, workspacePrefix); ok && rest != "" {
		s = rest
	}
	if compound(s) {
		return s
	}
	for {
		before := s
		if rest, ok := strings.CutSuffix(s, " 2>&1"); ok {
			s = rest
		}
		if i := lastPipe(s); i > 0 {
			stage := strings.TrimSpace(strings.TrimPrefix(s[i+1:], "&"))
			program, _, _ := strings.Cut(stage, " ")
			if rest := strings.TrimSpace(s[:i]); slices.Contains(outputFilters, program) && rest != "" {
				s = rest
			}
		}
		if s == before {
			return s
		}
	}
}

// exactKey is the exact key of a command's usage records: the sha256 of
// it normalized, so no record holds its text.
func exactKey(text string) string {
	sum := sha256.Sum256([]byte(normalizeCommand(text)))
	return hex.EncodeToString(sum[:])
}

// compound reports whether s holds a list (";", "&&", "||"), a background
// "&", a substitution ("$(", a backtick) or a parenthesis outside single
// quotes: more than one pipeline, whose filters may matter.
func compound(s string) bool {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
		case c == '\\':
			i++
		case c == '`' || (c == '$' && i+1 < len(s) && s[i+1] == '('):
			return true
		case quote == '"':
			if c == '"' {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == ';' || c == '(' || c == ')':
			return true
		case c == '|' && i+1 < len(s) && s[i+1] == '|':
			return true
		case c == '&':
			// A redirection's ("2>&1", ">&2", "&>f") or "|&" is no list.
			prev := byte(0)
			if i > 0 {
				prev = s[i-1]
			}
			if prev != '>' && prev != '<' && prev != '|' && (i+1 >= len(s) || s[i+1] != '>') {
				return true
			}
		}
	}
	return false
}

// lastPipe is the index of s's last pipe ("|", or the "|" of "|&") outside
// quotes and not part of "||"; -1 if there is none.
func lastPipe(s string) int {
	last := -1
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
		case c == '\\':
			i++
		case quote == '"':
			if c == '"' {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '|':
			if i+1 < len(s) && s[i+1] == '|' {
				i++
				continue
			}
			last = i
		}
	}
	return last
}

var (
	// envAssignment is a word setting a variable for the command after it.
	envAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	// subcommand is a word that may name a program's subcommand: no flag,
	// path, assignment or quoted text.
	subcommand = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]*$`)
)

// wrappers are the programs that run the command after them, skipped to
// find the program, each with its options that take an argument.
var wrappers = map[string][]string{
	"env":     {"-u", "--unset", "-C", "--chdir", "-S", "--split-string"},
	"time":    {"-f", "--format", "-o", "--output"},
	"timeout": {"-s", "--signal", "-k", "--kill-after"},
	"sudo":    {"-u", "--user", "-g", "--group", "-C", "-D", "-h", "-p", "-r", "-t", "-U"},
	"nice":    {"-n", "--adjustment"},
}

// coarseCommand is the coarse key of a command's usage records, the only
// command text a record holds: its program and first subcommand ("go
// test"), past variables, wrappers and a leading "cd DIR &&". Advisory.
func coarseCommand(text string) string {
	words := strings.Fields(normalizeCommand(text))
	if len(words) >= 3 && words[0] == "cd" && words[2] == "&&" {
		words = words[3:]
	}
	for len(words) > 0 {
		w := words[0]
		withArg, wrapper := wrappers[w]
		if !wrapper {
			if !envAssignment.MatchString(w) {
				break
			}
			words = words[1:]
			continue
		}
		words = words[1:]
		for len(words) > 0 && strings.HasPrefix(words[0], "-") {
			opt := words[0]
			words = words[1:]
			if opt == "--" {
				break
			}
			if slices.Contains(withArg, opt) && len(words) > 0 {
				words = words[1:]
			}
		}
		if w == "timeout" && len(words) > 0 {
			words = words[1:] // its duration
		}
	}
	if len(words) == 0 {
		return ""
	}
	if len(words) > 1 && subcommand.MatchString(words[1]) {
		return words[0] + " " + words[1]
	}
	return words[0]
}

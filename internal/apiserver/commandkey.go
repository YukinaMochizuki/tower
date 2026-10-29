package apiserver

import (
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

// normalizeCommand is the exact key of a command's usage records: its text
// with whitespace collapsed, a leading "cd /workspace &&" and trailing
// output filters ("2>&1", "| tail ...", "| head ...", "| grep ...")
// stripped, so a run of `go test ./... 2>&1 | tail -50` counts as one of
// `go test ./...`. Anything else, quoting included, stays as it is.
func normalizeCommand(text string) string {
	s := strings.Join(strings.Fields(text), " ")
	if rest, ok := strings.CutPrefix(s, workspacePrefix); ok && rest != "" {
		s = rest
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

// coarseCommand is the coarse key of a normalized command: its program and
// first subcommand, such as "go test", "make e2e" or "bun test", or the
// program alone when the next word is no subcommand ("python3 -c ..." is
// "python3"). Variables set before the program, and a leading
// "cd DIR &&", are skipped. Empty for a command of none of that.
//
// It is advisory: what it groups shares a program, not a cost ("go test
// ./x" and "go test ./..." are one key), so it may hint at a run's size,
// never decide where a run goes. docs/decisions/process-usage.md.
func coarseCommand(normalized string) string {
	words := strings.Fields(normalized)
	if len(words) >= 3 && words[0] == "cd" && words[2] == "&&" {
		words = words[3:]
	}
	for len(words) > 0 && envAssignment.MatchString(words[0]) {
		words = words[1:]
	}
	if len(words) == 0 {
		return ""
	}
	if len(words) > 1 && subcommand.MatchString(words[1]) {
		return words[0] + " " + words[1]
	}
	return words[0]
}

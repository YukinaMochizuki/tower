package apiserver

import (
	"strings"
	"testing"
)

func TestCommandText(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"a shell line", []string{"/bin/sh", "-c", "go test ./..."}, "go test ./..."},
		{"bash -lc", []string{"bash", "-lc", "make e2e"}, "make e2e"},
		{"argv", []string{"go", "test", "./..."}, "go test ./..."},
		{"a shell with more arguments is argv", []string{"sh", "-c", "echo $0", "x"}, "sh -c echo $0 x"},
		{"a shell running a script is argv", []string{"sh", "script.sh"}, "sh script.sh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := commandText(tc.argv); got != tc.want {
				t.Errorf("commandText(%q) = %q, want %q", tc.argv, got, tc.want)
			}
		})
	}
}

func TestNormalizeCommand(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"as it is", "go test ./...", "go test ./..."},
		{"whitespace collapses", "  go\ttest   ./...\n", "go test ./..."},
		{"whitespace in quotes collapses too", `echo "a   b"`, `echo "a b"`},
		{"cd /workspace", "cd /workspace && go test ./...", "go test ./..."},
		{"cd /workspace with odd spacing", "cd  /workspace&&  go test", "cd /workspace&& go test"},
		{"cd elsewhere stays", "cd /workspace/sub && go test ./...", "cd /workspace/sub && go test ./..."},
		{"cd /workspace alone stays", "cd /workspace &&", "cd /workspace &&"},
		{"cd /workspace only once", "cd /workspace && cd /workspace && make", "cd /workspace && make"},
		{"2>&1", "go test ./... 2>&1", "go test ./..."},
		{"tail", "go test ./... | tail -50", "go test ./..."},
		{"head", "go test ./... | head -n 20", "go test ./..."},
		{"grep", "go test ./... | grep -v ok", "go test ./..."},
		{"all of them", "cd /workspace && go test ./... 2>&1 | grep -v '^ok' | tail -n 30", "go test ./..."},
		{"|&", "make e2e |& tail -5", "make e2e"},
		{"no space before the pipe", "make e2e|tail", "make e2e"},
		{"a filter before another stage stays", "go test ./... | grep FAIL | wc -l", "go test ./... | grep FAIL | wc -l"},
		{"tee stays", "make check 2>&1 | tee out.log", "make check 2>&1 | tee out.log"},
		{"a redirect to a file stays", "make check > out.log 2>&1", "make check > out.log"},
		{"|| is no pipe", "make check || tail log", "make check || tail log"},
		{"a pipe in single quotes", "echo 'a | tail'", "echo 'a | tail'"},
		{"a pipe in double quotes", `echo "a | head"`, `echo "a | head"`},
		{"an escaped pipe", `echo a \| tail`, `echo a \| tail`},
		{"a quote in double quotes", `echo "it's" | tail`, `echo "it's"`},
		{"a filter alone stays", "| tail", "| tail"},
		{"grep alone stays", "grep -r foo .", "grep -r foo ."},
		{"tailing is no tail", "make all | tailing", "make all | tailing"},
		{"empty", "  ", ""},
		// A filter is stripped only where it ends a line that is one
		// pipeline: a list, a background job, a substitution or a
		// subshell anywhere keeps the line as it is.
		{"&& after a tail", "go test ./... | tail -5 && make lint", "go test ./... | tail -5 && make lint"},
		{"; after a grep", "make e2e 2>&1 | grep FAIL; make deploy", "make e2e 2>&1 | grep FAIL; make deploy"},
		{"|| after a head", "a | head -1 || exit 1", "a | head -1 || exit 1"},
		{"a tail in the background", "a | tail -5 &", "a | tail -5 &"},
		{"a background job before", "server & go test ./... | tail -5", "server & go test ./... | tail -5"},
		{"a filter in a substitution", "echo $(go test ./... | tail -1)", "echo $(go test ./... | tail -1)"},
		{"a substitution in double quotes", `echo "$(go test ./... | tail -1)"`, `echo "$(go test ./... | tail -1)"`},
		{"a backtick", "echo `go test ./... | tail -1`", "echo `go test ./... | tail -1`"},
		{"a subshell", "(cd sub && go test ./...) | tail -5", "(cd sub && go test ./...) | tail -5"},
		{"a group", "{ make a; make b; } 2>&1 | tail", "{ make a; make b; } 2>&1 | tail"},
		{"a list after cd /workspace", "cd /workspace && make a && make b | tail", "make a && make b | tail"},
		{"cd elsewhere is a list", "cd sub && go test ./... | tail", "cd sub && go test ./... | tail"},
		{"&> is a redirection", "make e2e &> out.log | tail", "make e2e &> out.log"},
		{">&2 is a redirection", "echo x >&2 | tail", "echo x >&2"},
		{"; in single quotes", "go test -run 'A;B' ./... | tail", "go test -run 'A;B' ./..."},
		{"$( in single quotes", "echo '$(x)' | tail", "echo '$(x)'"},
		{"&& in double quotes", `echo "a && b" | tail`, `echo "a && b"`},
		{"an escaped ;", `echo a\; | tail`, `echo a\;`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeCommand(tc.in); got != tc.want {
				t.Errorf("normalizeCommand(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestExactKey(t *testing.T) {
	a, b := exactKey("cd /workspace && go test ./... 2>&1 | tail -5"), exactKey("go  test ./...")
	if a != b || len(a) != 64 {
		t.Errorf("exactKey = %q and %q, want one sha256", a, b)
	}
	if exactKey("go test ./... | tail -5 && make lint") == b {
		t.Error("a compound line has the key of its first command")
	}
}

func TestCoarseCommand(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"go test ./...", "go test"},
		{"go test -race ./internal/...", "go test"},
		{"make e2e", "make e2e"},
		{"bun test", "bun test"},
		{"npm run build", "npm run"},
		{"make", "make"},
		{"make -j4 check", "make"},
		{"python3 -c 'b=bytearray(8<<30)'", "python3"},
		{"pytest tests/test_x.py", "pytest"},
		{"./scripts/test.sh all", "<program>"},
		{"/usr/local/bin/go test ./...", "go test"},
		{"/usr/bin/env FOO=1 make e2e", "make e2e"},
		{"CGO_ENABLED=0 GOOS=linux go build ./cmd/x", "go build"},
		{"cd /workspace && go test ./... 2>&1 | tail", "go test"},
		{"cd sub && make e2e", "make e2e"},
		{"go test ./... | grep FAIL | wc -l", "go test"},
		{"cargo nextest:run", "cargo"},
		{"npm run build:prod", "npm run"},
		{"Make e2e", "<program>"},
		{"make E2E", "make"},
		{"python3.12 -m pytest", "<program>"},
		{"k8s-deploy apply-all", "k8s-deploy apply-all"},
		{"make " + strings.Repeat("z", 32), "make " + strings.Repeat("z", 32)},
		{"make " + strings.Repeat("z", 33), "make"},
		// What may be a secret never reaches the key.
		{"./deploy.sh ghp_xxx", "<program>"},
		// Token-shaped, built so that no secret scanner reads them as real.
		{"deploy " + "ghp" + "_" + strings.Repeat("Xy9", 12), "deploy"},
		{"deploy " + "sk" + "_live_" + strings.Repeat("Qz7", 8), "deploy"},
		{"deploy a9f8e7d6c5b4", "deploy"},
		{"deploy x7k2m9q4p1z8", "deploy"},
		{"curl https://user:hunter2@example.com/hook", "curl"},
		{"git clone https://x-access-token:ghs_abc@github.com/o/r", "git clone"},
		{"/run/secrets/tok3n-a9f8e7d6c5b4/run.sh build", "<program>"},
		{"cat /run/secrets/abcd1234ef567890", "cat"},
		{"deploy --token=abc123", "deploy"},
		{"deploy --token abc123", "deploy"},
		{"git 3f2a9c1b7e4d5f6071829304a5b6c7d8e9f00112", "git"},
		{"make deadbeefcafe", "make"},
		{"git checkout 3f2a9c1b7e4d", "git checkout"},
		{"env FOO=1 go test ./...", "go test"},
		{"env -u HOME -i PATH=/bin make check", "make check"},
		{"time go test ./...", "go test"},
		{"time -p make e2e", "make e2e"},
		{"timeout 10m go test ./...", "go test"},
		{"timeout -s KILL -k 5s 600 make e2e", "make e2e"},
		{"sudo -u dev make install", "make install"},
		{"sudo -- make install", "make install"},
		{"nice -n 10 make -j8", "make"},
		{"GOFLAGS=-count=1 timeout 1h nice go test ./...", "go test"},
		{"time", ""},
		{"FOO=1", ""},
		{"", ""},
	} {
		t.Run(tc.in, func(t *testing.T) {
			if got := coarseCommand(tc.in); got != tc.want {
				t.Errorf("coarseCommand(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

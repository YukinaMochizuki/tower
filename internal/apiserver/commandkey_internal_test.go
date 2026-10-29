package apiserver

import "testing"

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
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeCommand(tc.in); got != tc.want {
				t.Errorf("normalizeCommand(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
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
		{"./scripts/test.sh all", "./scripts/test.sh all"},
		{"CGO_ENABLED=0 GOOS=linux go build ./cmd/x", "go build"},
		{"cd sub && make e2e", "make e2e"},
		{"go test ./... | grep FAIL | wc -l", "go test"},
		{"cargo nextest:run", "cargo nextest:run"},
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

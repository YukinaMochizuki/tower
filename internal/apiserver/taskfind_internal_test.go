package apiserver

import (
	"testing"
)

// webScripts are the package.json scripts of a repository's web/ and of
// its root, as scriptsFunc reads them.
func webScripts(dir string) (map[string]string, bool) {
	switch dir {
	case "web":
		return map[string]string{"lint": "eslint .", "test": "vitest run", "build": "vite build", "test:unit": "vitest run unit"}, true
	case ".":
		return map[string]string{"typecheck": "tsc --noEmit"}, true
	}
	return nil, false
}

// TestFindTasks: a line's tasks are found past setup, wrappers and output
// filters, however an agent writes the line around them; a line with
// none, or with one that cannot be keyed by plain names, has none.
func TestFindTasks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		line, dir string
		want      string
	}{
		// make: its directory, targets and default goal.
		{"make test", "", "make test"},
		{"make", "/workspace", "make"},
		{"make -C svc test 2>&1 | tee log", "", "make test (in svc)"},
		{"make -Csvc -j8 lint test V=1", "", "make lint test (in svc)"},
		{"make --directory=svc -j 4 test", "", "make test (in svc)"},
		{"make -k -s test", "", "make test"},
		{"cd /workspace && make e2e 2>&1 | tail -50", "", "make e2e"},
		{"make gen && make test", "", "make gen && make test"},
		{"time make check", "", "make check"},
		{"/usr/bin/time -v make check", "", "make check"},
		{"timeout 600 make e2e", "", "make e2e"},
		{"env -C svc CI=1 make test", "", "make test (in svc)"},
		{"sudo -u dev nice -n 10 nohup make test &", "", "make test"},
		// Shells within the line, and subshells.
		{"bash -lc 'cd web && pnpm test'", "", "pnpm test (in web)"},
		{`sh -c "make -C api test"`, "", "make test (in api)"},
		{"bash --login -o pipefail -c 'go test ./...'", "", "go test ./..."},
		{"(cd svc && make test); make lint", "", "make test (in svc) && make lint"},
		{"{ cd svc; make test; }", "", "make test (in svc)"},
		// A pipe's stages run in subshells: a cd there stays there.
		{"cd svc | cat; make test", "", "make test"},
		{"true | cd svc && make test", "", "make test"},
		// Package scripts, with run and with shorthands.
		{"cd web && export CI=1; pnpm vitest run src/x | tail -50", "", "vitest (in web)"},
		{"cd web && pnpm lint", "", "pnpm lint (in web)"},
		{"pnpm -C web run build", "", "pnpm build (in web)"},
		{"pnpm --dir=web test", "", "pnpm test (in web)"},
		{"npm test", "/workspace/web", "npm test (in web)"},
		{"npm run test:unit --silent", "web", "npm test:unit (in web)"},
		{"npm --prefix web run lint", "", "npm lint (in web)"},
		{"yarn --cwd web lint", "", "yarn lint (in web)"},
		{"yarn jest --ci", "", "jest"},
		{"bun run typecheck", "", "bun typecheck"},
		{"bun typecheck", "", "bun typecheck"},
		{"npx tsc --noEmit", "", "tsc"},
		{"pnpm exec tsc -p .", "", "tsc"},
		{"./node_modules/.bin/vitest run", "", "vitest"},
		// Direct heavy tools, go's package pattern its key.
		{"go test ./... -run X", "", "go test ./..."},
		{"go test -race -count=1 -run TestX ./internal/apiserver/", "", "go test ./internal/apiserver/"},
		{"go test -C svc -v", "", "go test . (in svc)"},
		{"GOFLAGS=-mod=mod go build -o bin/tower ./cmd/tower", "", "go build ./cmd/tower"},
		{"go test -args -foo ./x", "", "go test ."},
		{"cargo test -p core -- --nocapture", "", "cargo test core"},
		{"cargo build --release", "", "cargo build"},
		{"just test", "", "just test"},
		{"just", "", "just"},
		{"task build:web", "", "task build:web"},
		{"task -d web lint", "", "task lint (in web)"},
		// No task: setup alone, a command of its own, a script file.
		{"ls -la", "", ""},
		{"git status && git diff", "", ""},
		{"./scripts/e2e.sh", "", ""},
		{"go vet ./...", "", ""},
		{"pnpm install", "", ""},
		{"npm ci", "", ""},
		{"bun test", "", ""},
		{"make -n test", "/elsewhere", ""},
		{"pnpm eslint .", "web", ""},
		{"just --list", "", ""},
		// Tasks that cannot be keyed by plain names: none.
		{"make $TARGET", "", ""},
		{"make gen && make $TARGET", "", ""},
		{"make gen; cd $X; make test", "", ""},
		{"cd $DIR && make test", "", ""},
		{"cd && make test", "", ""},
		{"make test-0123456789abcdef", "", ""},
		{"make bin/tower", "", ""},
		{"make -f other.mk test", "", ""},
		{"go test $(go list ./...)", "", ""},
		{"cd .. && make test", "", ""},
		{"pnpm -r test", "", ""},
		{"pnpm --filter web test", "", ""},
		{"npm -w web test", "", ""},
		{"make 'unclosed", "", ""},
		{"go test ./deadbeefcafe0123/...", "", ""},
		// Loops and conditionals are not read for a line's tasks.
		{"for d in a b; do make -C $d test; done", "", ""},
		{"if true; then make test; fi", "", ""},
	} {
		t.Run(tc.line, func(t *testing.T) {
			t.Parallel()
			if got := tasksText(findTasks(tc.line, tc.dir, webScripts)); got != tc.want {
				t.Errorf("findTasks(%q, %q) = %q, want %q", tc.line, tc.dir, got, tc.want)
			}
		})
	}
}

// TestFindTasksWithoutPackageJSON: a runner's shorthand is a script only
// when package.json says so; with none to read, a heavy tool is still one.
func TestFindTasksWithoutPackageJSON(t *testing.T) {
	t.Parallel()
	none := func(string) (map[string]string, bool) { return nil, false }
	for line, want := range map[string]string{
		"pnpm lint":         "",
		"pnpm vitest run":   "vitest",
		"pnpm run lint":     "pnpm lint",
		"yarn lint":         "",
		"bun lint":          "",
		"npm test":          "npm test",
		"yarn run tsc":      "tsc",
		"bun run lint":      "",
		"pnpm test -- --ci": "pnpm test",
	} {
		if got := tasksText(findTasks(line, "", none)); got != want {
			t.Errorf("findTasks(%q) with no package.json = %q, want %q", line, got, want)
		}
	}
	if got := tasksText(findTasks("pnpm lint", "", nil)); got != "" {
		t.Errorf("findTasks with no scriptsFunc = %q", got)
	}
}

// TestExpansionTasks: an expansion is read whole, inside conditionals,
// loops and substitutions, with the bundlers and builds a line's tasks
// are not; a line that is not shell is passed over.
func TestExpansionTasks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ text, want string }{
		{"echo building\ngo build -o bin/x ./cmd/x", "go build"},
		{"if [ -n \"$CI\" ]; then go test ./...; fi", "go test"},
		{"for p in a b; do\n\tcargo build -p $p\ndone", "cargo build"},
		{"x=$(tsc --version)", "tsc"},
		{"echo \"$(jest --listTests)\"", "jest"},
		{"cd web && vite build", "vite build (in web)"},
		{"docker buildx build -t x .", "docker build"},
		{"next build", "next build"},
		{"webpack --mode production", "webpack"},
		{"make[1]: Entering directory '/workspace/svc'\nif then\ngo test ./x", "go test"},
		{"cd web && pnpm lint && make $X", "pnpm lint (in web)"},
		{"echo $(", ""},
		{"go vet ./... && eslint .", ""},
	} {
		if got := tasksText(expansionTasks(tc.text, ".", webScripts)); got != tc.want {
			t.Errorf("expansionTasks(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

// TestPlainNames: a key's words are names the repository gives.
func TestPlainNames(t *testing.T) {
	t.Parallel()
	for n, want := range map[string]bool{
		"test": true, "test-e2e-race": true, "build_deploy": true, "a": true,
		"Test": false, "9lives": false, "deadbeef": false, "abc12345": false, "ab1234567": false,
		"thirty-two-characters-is-the-max": true, "thirty-three-characters-is-too-lo": false,
	} {
		if got := nameWord(n); got != want {
			t.Errorf("nameWord(%q) = %v, want %v", n, got, want)
		}
	}
	for n, want := range map[string]bool{
		"test:unit": true, "mod::recipe": true, "lint:": false, ":lint": false, "test:Unit": false, "": false,
	} {
		if got := recipeName(n); got != want {
			t.Errorf("recipeName(%q) = %v, want %v", n, got, want)
		}
	}
	for d, want := range map[string]bool{
		".": true, "web": true, "internal/apiserver": true, "Web/App.v2": true, "a/0123456789abcdef": false, unknownDir: false, "a b": false,
	} {
		if got := plainDir(d); got != want {
			t.Errorf("plainDir(%q) = %v, want %v", d, got, want)
		}
	}
	for p, want := range map[string]bool{
		".": true, "./...": true, "./internal/x/...": true, "github.com/a/b": true, "./pkg...": true, "./x y": false, "$X": false,
	} {
		if got := packagePattern(p); got != want {
			t.Errorf("packagePattern(%q) = %v, want %v", p, got, want)
		}
	}
}

// TestCommandText: argv is one line as shell reads it again.
func TestCommandText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"/bin/sh", "-c", "make test | tail"}, "make test | tail"},
		{[]string{"bash", "-lc", "cd web && pnpm test"}, "cd web && pnpm test"},
		{[]string{"make", "-C", "svc", "test"}, "make -C svc test"},
		{[]string{"echo", "a b", "it's"}, `echo 'a b' 'it'\''s'`},
		{[]string{"x", ""}, "x ''"},
	} {
		if got := commandText(tc.argv); got != tc.want {
			t.Errorf("commandText(%q) = %q, want %q", tc.argv, got, tc.want)
		}
	}
}

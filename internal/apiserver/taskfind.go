package apiserver

import (
	"cmp"
	"path"
	"regexp"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// A command line's tasks: the targets, scripts, recipes and heavy tools
// it runs, found by reading it as shell (docs/decisions/boost.md,
// "Routing by task").

// taskEntry is one task a line runs: what the repository declares, or a
// heavy tool called directly.
type taskEntry struct {
	// Dir is where it runs, relative to /workspace: "." for /workspace.
	Dir string `json:"dir"`
	// Tool is what runs it: make, just or task with a recipe; npm, pnpm,
	// yarn or bun with a script; or a heavy tool called directly ("go
	// test", "go build", "go vet", "cargo test", "cargo build", "bun
	// test", tsc, vitest, jest).
	// A script's text also finds the heavy tools of scriptTools.
	Tool string `json:"tool"`
	// Name is its targets, recipe, script or package patterns, by spaces;
	// empty for a default goal, or a tool with none.
	Name string `json:"name,omitempty"`
}

// String is the task in its own words, as a routed answer says it.
func (t taskEntry) String() string {
	s := t.Tool
	if t.Name != "" {
		s += " " + t.Name
	}
	if t.Dir != "." {
		s += " (in " + t.Dir + ")"
	}
	return s
}

// script says t runs a package.json script.
func (t taskEntry) script() bool { return slices.Contains(scriptRunners, t.Tool) }

// tasksText is a line's tasks in their words, "" for none.
func tasksText(tasks []taskEntry) string {
	words := make([]string, len(tasks))
	for i, t := range tasks {
		words[i] = t.String()
	}
	return strings.Join(words, " && ")
}

// scriptRunners run package.json scripts.
var scriptRunners = []string{"npm", "pnpm", "yarn", "bun"}

// directTools are the heavy tools a line's task may be by themselves,
// named alone; go and cargo are tasks by their subcommand.
var directTools = []string{"tsc", "vitest", "jest"}

// scriptTools are the heavy tools a script's text is judged heavy by,
// beyond the tasks a line may be: bundlers and builds.
var scriptTools = []string{"vite build", "next build", "webpack", "docker build"}

// boxView is what a line's tasks are read against: the box's files, as
// its user reads them, never anything run there.
type boxView interface {
	// scripts are the scripts of the package.json in dir, relative to
	// /workspace; ok is false when there is none or it cannot be read.
	scripts(dir string) (scripts map[string]string, ok bool)
	// isDir says dir, relative to /workspace, is a directory of the box.
	isDir(dir string) bool
}

// unknownDir is a directory a line moves to that cannot be told, as "cd
// $X": a task there is none the line can be keyed by.
const unknownDir = "\x00"

// maxShellDepth bounds the shells within shells ("bash -c '...'") read.
const maxShellDepth = 4

// taskFinder reads a line for its tasks.
type taskFinder struct {
	view boxView
	// loose reads a script's text rather than a line: every command, inside
	// conditionals, loops and substitutions too, and the heavy tools of
	// scriptTools; a word it cannot read is passed over, not the end of
	// the line's tasks.
	loose bool
	tasks []taskEntry
	// bad says the line runs something it cannot be keyed by: a target, a
	// directory or a pattern that is not a plain name, or not literal.
	bad   bool
	depth int
}

// findTasks is the tasks line runs from dir (as a process's directory,
// absolute or relative to /workspace), in order; none when it runs no
// task, or runs one it cannot be keyed by: a word no plain name, or a
// directory, its own or its package pattern's, that the box does not have.
func findTasks(line, dir string, view boxView) []taskEntry {
	f := &taskFinder{view: view}
	f.line(line, startDir(dir))
	if f.bad {
		return nil
	}
	for _, t := range f.tasks {
		for _, d := range t.dirs() {
			if d != "." && (view == nil || !view.isDir(d)) {
				return nil
			}
		}
	}
	return f.tasks
}

// dirs are the directories t names, relative to /workspace: its own, and
// those of its package patterns; unknownDir for one that cannot be told.
func (t taskEntry) dirs() []string {
	out := []string{t.Dir}
	if t.Tool != "go test" && t.Tool != "go build" && t.Tool != "go vet" {
		return out
	}
	for _, p := range splitNames(t.Name) {
		if !strings.HasPrefix(p, ".") {
			// A module's path, or a file: no directory of the box.
			out = append(out, unknownDir)
			continue
		}
		p = strings.TrimSuffix(strings.TrimSuffix(p, "..."), "/")
		out = append(out, joinDir(t.Dir, cmp.Or(p, ".")))
	}
	return out
}

// splitNames is a task's names, as Name joins them.
func splitNames(name string) []string { return strings.Fields(name) }

// scriptTasks is what a script's text runs from dir: its heavy tools
// and the scripts it runs.
func scriptTasks(text, dir string, view boxView) []taskEntry {
	f := &taskFinder{view: view, loose: true}
	f.line(text, dir)
	return f.tasks
}

// startDir is a process's directory relative to /workspace, "." when it
// asked for none (a box's processes start there), unknownDir outside it.
func startDir(dir string) string {
	rel := workspaceRelative(dir)
	switch {
	case rel == "":
		return "."
	case strings.HasPrefix(rel, "/"):
		return unknownDir
	}
	return rel
}

func (f *taskFinder) line(text, dir string) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(text), "")
	if err != nil {
		if !f.loose {
			// Not shell this reads: its tasks cannot be told.
			f.bad = true
			return
		}
		// A script's text is read line by line where it is not shell as a
		// whole, a recipe's line being shell alone.
		for l := range strings.SplitSeq(text, "\n") {
			if file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(l), ""); err == nil {
				d := dir
				f.stmts(file.Stmts, &d)
			}
		}
		return
	}
	f.stmts(file.Stmts, &dir)
}

func (f *taskFinder) stmts(stmts []*syntax.Stmt, dir *string) {
	for _, st := range stmts {
		f.stmt(st, dir)
	}
}

func (f *taskFinder) stmt(st *syntax.Stmt, dir *string) {
	if st == nil {
		return
	}
	switch c := st.Cmd.(type) {
	case *syntax.CallExpr:
		f.call(c, dir)
	case *syntax.BinaryCmd:
		if c.Op == syntax.Pipe || c.Op == syntax.PipeAll {
			// Each stage runs in a subshell of its own: an output filter
			// runs no task, and a cd in a stage stays there.
			x, y := *dir, *dir
			f.stmt(c.X, &x)
			f.stmt(c.Y, &y)
			return
		}
		f.stmt(c.X, dir)
		f.stmt(c.Y, dir)
	case *syntax.Subshell:
		d := *dir
		f.stmts(c.Stmts, &d)
	case *syntax.Block:
		f.stmts(c.Stmts, dir)
	case *syntax.TimeClause:
		f.stmt(c.Stmt, dir)
	case *syntax.DeclClause:
		// Setup: export, declare, local.
	default:
		// Conditionals, loops, functions: a line's tasks are not read in
		// them; a script's are.
		if f.loose && c != nil {
			f.nested(c, *dir)
		}
	}
}

// nested reads the commands of a node a script holds, each from dir.
func (f *taskFinder) nested(n syntax.Node, dir string) {
	syntax.Walk(n, func(n syntax.Node) bool {
		if c, ok := n.(*syntax.CallExpr); ok {
			d := dir
			f.call(c, &d)
			return false
		}
		return true
	})
}

// word is a word of a command: its text, if it is literal.
type word struct {
	text string
	lit  bool
}

func literal(w *syntax.Word) word {
	var b strings.Builder
	for _, p := range w.Parts {
		switch p := p.(type) {
		case *syntax.Lit:
			if strings.ContainsAny(p.Value, `\*?[`) {
				// Escapes and globs: its text is not what runs.
				return word{}
			}
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			if p.Dollar {
				return word{}
			}
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, q := range p.Parts {
				l, ok := q.(*syntax.Lit)
				if !ok || strings.Contains(l.Value, `\`) {
					return word{}
				}
				b.WriteString(l.Value)
			}
		default:
			return word{}
		}
	}
	return word{text: b.String(), lit: true}
}

func (f *taskFinder) call(c *syntax.CallExpr, dir *string) {
	if f.loose {
		// What its words and assignments substitute runs too.
		syntax.Walk(c, func(n syntax.Node) bool {
			if inner, ok := n.(*syntax.CallExpr); ok && inner != c {
				d := *dir
				f.call(inner, &d)
				return false
			}
			return true
		})
	}
	words := make([]word, len(c.Args))
	for i, w := range c.Args {
		words[i] = literal(w)
	}
	d := *dir
	words, ok := skipWrappers(words, &d)
	if !ok || len(words) == 0 || !words[0].lit {
		return
	}
	program, args := path.Base(words[0].text), words[1:]
	switch program {
	case "cd", "pushd":
		*dir = changeDir(*dir, args)
	case "sh", "bash", "dash", "zsh":
		f.shell(args, d)
	case "make", "gmake":
		f.make(args, d)
	case "just":
		f.just(args, d)
	case "task":
		f.goTask(args, d)
	case "npm":
		f.npm(args, d)
	case "pnpm":
		f.pnpm(args, d)
	case "yarn":
		f.yarn(args, d)
	case "bun":
		f.bun(args, d)
	case "npx", "bunx":
		f.tool(args, d)
	default:
		f.tool(words, d)
	}
}

// add keeps the task, unless its words are no plain names.
func (f *taskFinder) add(t taskEntry) {
	if f.loose {
		if t.Dir != unknownDir {
			f.tasks = append(f.tasks, t)
		}
		return
	}
	if !plainDir(t.Dir) {
		f.bad = true
		return
	}
	f.tasks = append(f.tasks, t)
}

// none says the line runs a task it cannot be keyed by.
func (f *taskFinder) none() {
	if !f.loose {
		f.bad = true
	}
}

// wrappers are the programs that run the command after them, each with
// its options that take an argument.
var wrappers = map[string][]string{
	"env":     {"-u", "--unset", "-C", "--chdir", "-S", "--split-string"},
	"time":    {"-f", "--format", "-o", "--output"},
	"timeout": {"-s", "--signal", "-k", "--kill-after"},
	"sudo":    {"-u", "--user", "-g", "--group", "-C", "-D", "--chdir", "-h", "-p", "-r", "-t", "-U"},
	"nice":    {"-n", "--adjustment"},
	"nohup":   nil,
	"exec":    {"-a"},
	"command": nil,
}

// envAssignment is a word setting a variable, for the command after it or
// for make.
var envAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// skipWrappers is words past the wrappers that run the command after
// them, and env's variables; a directory one moves to (env -C, sudo -D)
// moves dir. ok is false when a wrapper's words cannot be read.
func skipWrappers(words []word, dir *string) ([]word, bool) {
	for len(words) > 0 {
		if !words[0].lit {
			return words, true
		}
		w := path.Base(words[0].text)
		withArg, wrapper := wrappers[w]
		if !wrapper {
			return words, true
		}
		words = words[1:]
		for len(words) > 0 {
			if !words[0].lit {
				return nil, false
			}
			opt := words[0].text
			if w == "env" && envAssignment.MatchString(opt) {
				words = words[1:]
				continue
			}
			if !strings.HasPrefix(opt, "-") || opt == "-" {
				break
			}
			if w == "command" && (opt == "-v" || opt == "-V") {
				// A lookup of what a name is: it runs nothing.
				return nil, false
			}
			words = words[1:]
			if opt == "--" {
				break
			}
			name, value, hasValue := strings.Cut(opt, "=")
			if !slices.Contains(withArg, name) {
				continue
			}
			if !hasValue {
				if len(words) == 0 || !words[0].lit {
					return nil, false
				}
				value, words = words[0].text, words[1:]
			}
			if slices.Contains([]string{"-C", "--chdir", "-D"}, name) {
				*dir = changeDir(*dir, []word{{text: value, lit: true}})
			}
		}
		if w == "timeout" && len(words) > 0 {
			words = words[1:] // its duration
		}
	}
	return words, true
}

// changeDir is dir after a cd with args.
func changeDir(dir string, args []word) string {
	if len(args) != 1 || !args[0].lit || dir == unknownDir {
		// cd alone goes home; cd - and cd $X go where the line cannot say.
		return unknownDir
	}
	return joinDir(dir, args[0].text)
}

// joinDir is to, from dir, relative to /workspace; unknownDir outside it.
func joinDir(dir, to string) string {
	if dir == unknownDir || to == "" || to == "-" || strings.HasPrefix(to, "~") {
		return unknownDir
	}
	if path.IsAbs(to) {
		return startDir(to)
	}
	joined := path.Join(dir, to)
	if joined == ".." || strings.HasPrefix(joined, "../") {
		return unknownDir
	}
	return joined
}

// shell reads the line a shell's -c runs, as a line of its own.
func (f *taskFinder) shell(args []word, dir string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !a.lit || !strings.HasPrefix(a.text, "-") {
			// A script file, or a shell reading its input: no line to read.
			return
		}
		if strings.HasPrefix(a.text, "--") {
			continue
		}
		if a.text == "-o" || a.text == "-O" {
			i++ // its option's name
			continue
		}
		if !strings.Contains(a.text, "c") {
			continue
		}
		if i+1 >= len(args) || !args[i+1].lit {
			f.none()
			return
		}
		if f.depth >= maxShellDepth {
			f.none()
			return
		}
		f.depth++
		f.line(args[i+1].text, dir)
		f.depth--
		return
	}
}

// makeFlagsWithArg are make's short options that take an argument, joined
// or as the next word; j and l take one optionally, a number.
const makeFlagsWithArg = "CEfIoW"

func (f *taskFinder) make(args []word, dir string) {
	var targets []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !a.lit {
			f.none()
			return
		}
		next := func() (string, bool) {
			if i+1 >= len(args) || !args[i+1].lit {
				return "", false
			}
			i++
			return args[i].text, true
		}
		switch {
		case a.text == "--":
			for _, t := range args[i+1:] {
				if !t.lit {
					f.none()
					return
				}
				targets = append(targets, t.text)
			}
			i = len(args)
		case strings.HasPrefix(a.text, "--"):
			name, value, hasValue := strings.Cut(a.text, "=")
			switch name {
			case "--file", "--makefile", "--eval":
				// Another makefile than the directory's: not its task.
				f.none()
				return
			case "--directory":
				if !hasValue {
					var ok bool
					if value, ok = next(); !ok {
						f.none()
						return
					}
				}
				dir = joinDir(dir, value)
			case "--include-dir", "--old-file", "--assume-old", "--new-file", "--assume-new", "--what-if":
				if !hasValue {
					next()
				}
			case "--jobs", "--load-average":
				if !hasValue && i+1 < len(args) && isNumber(args[i+1]) {
					i++
				}
			}
		case strings.HasPrefix(a.text, "-") && len(a.text) > 1:
			flags := a.text[1:]
			for k := 0; k < len(flags); k++ {
				c := flags[k]
				rest := flags[k+1:]
				switch {
				case strings.IndexByte(makeFlagsWithArg, c) >= 0:
					value := rest
					if value == "" {
						var ok bool
						if value, ok = next(); !ok {
							f.none()
							return
						}
					}
					switch c {
					case 'f', 'E':
						// Another makefile, or more of one (-E is --eval).
						f.none()
						return
					case 'C':
						dir = joinDir(dir, value)
					}
					k = len(flags)
				case c == 'j' || c == 'l':
					if rest == "" && i+1 < len(args) && isNumber(args[i+1]) {
						i++
					}
					k = len(flags)
				}
			}
		case envAssignment.MatchString(a.text):
			// A variable, not a target.
		default:
			targets = append(targets, a.text)
		}
	}
	if !f.names(targets, makeTarget) {
		return
	}
	f.add(taskEntry{Dir: dir, Tool: "make", Name: strings.Join(targets, " ")})
}

func isNumber(w word) bool {
	if !w.lit || w.text == "" {
		return false
	}
	for _, c := range w.text {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// names checks each of a task's names by valid, the line no task if one
// fails.
func (f *taskFinder) names(names []string, valid func(string) bool) bool {
	for _, n := range names {
		if !valid(n) {
			f.none()
			return false
		}
	}
	return true
}

// justListing are just's options that run no recipe.
var justListing = []string{"-l", "--list", "-s", "--show", "--summary", "--evaluate", "-e", "--edit", "--fmt", "--init", "--dump",
	"--choose", "--variables", "--completions", "-h", "--help", "-V", "--version", "--groups", "--man", "--changelog"}

func (f *taskFinder) just(args []word, dir string) {
	recipe := ""
	for i := 0; i < len(args) && recipe == ""; i++ {
		a := args[i]
		if !a.lit {
			f.none()
			return
		}
		name, _, hasValue := strings.Cut(a.text, "=")
		switch {
		case slices.Contains(justListing, a.text):
			return
		case name == "-f" || name == "--justfile" || name == "-d" || name == "--working-directory":
			f.none()
			return
		case name == "--set":
			i += 2
		case slices.Contains([]string{"--shell", "--shell-arg", "--color", "--dotenv-filename", "--dotenv-path", "-E", "--command-color", "--chooser", "--list-heading", "--list-prefix", "--timestamp-format", "--tempdir", "--alias-style"}, name):
			if !hasValue {
				i++
			}
		case strings.HasPrefix(a.text, "-"):
		case envAssignment.MatchString(a.text):
			// An override of a variable.
		default:
			recipe = a.text
		}
	}
	if recipe != "" && !f.names([]string{recipe}, recipeName) {
		return
	}
	f.add(taskEntry{Dir: dir, Tool: "just", Name: recipe})
}

// taskListing are task's options that run no task.
var taskListing = []string{"-l", "--list", "-a", "--list-all", "--summary", "-i", "--init", "--version", "-h", "--help", "--completion"}

// goTask reads a call of task (taskfile.dev).
func (f *taskFinder) goTask(args []word, dir string) {
	var names []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !a.lit {
			f.none()
			return
		}
		name, value, hasValue := strings.Cut(a.text, "=")
		switch {
		case a.text == "--":
			i = len(args)
		case slices.Contains(taskListing, a.text):
			return
		case name == "-t" || name == "--taskfile":
			f.none()
			return
		case name == "-d" || name == "--dir":
			if !hasValue {
				if i+1 >= len(args) || !args[i+1].lit {
					f.none()
					return
				}
				i++
				value = args[i].text
			}
			dir = joinDir(dir, value)
		case slices.Contains([]string{"-o", "--output", "-I", "--interval", "-c", "--concurrency", "--output-group-begin", "--output-group-end"}, name):
			if !hasValue {
				i++
			}
		case strings.HasPrefix(a.text, "-"):
		case envAssignment.MatchString(a.text):
		default:
			names = append(names, a.text)
		}
	}
	if !f.names(names, recipeName) {
		return
	}
	f.add(taskEntry{Dir: dir, Tool: "task", Name: strings.Join(names, " ")})
}

// runnerFlags reads the options before a script runner's command: the
// directory one moves to by dirFlags, those taking an argument by
// withArg, and whether one runs another package's (badFlags). rest starts
// at its command.
func runnerFlags(args []word, dir string, dirFlags, withArg, badFlags []string) (rest []word, d string, ok bool) {
	d = dir
	for len(args) > 0 {
		a := args[0]
		if !a.lit {
			return nil, d, false
		}
		if !strings.HasPrefix(a.text, "-") || a.text == "-" {
			return args, d, true
		}
		args = args[1:]
		if a.text == "--" {
			return args, d, true
		}
		name, value, hasValue := strings.Cut(a.text, "=")
		switch {
		case slices.Contains(badFlags, name):
			return nil, d, false
		case slices.Contains(dirFlags, name):
			if !hasValue {
				if len(args) == 0 || !args[0].lit {
					return nil, d, false
				}
				value, args = args[0].text, args[1:]
			}
			d = joinDir(d, value)
		case slices.Contains(withArg, name) && !hasValue:
			if len(args) > 0 {
				args = args[1:]
			}
		}
	}
	return args, d, true
}

// script adds tool's script name, run in dir.
func (f *taskFinder) script(tool, name string, dir string) {
	if !f.names([]string{name}, recipeName) {
		return
	}
	f.add(taskEntry{Dir: dir, Tool: tool, Name: name})
}

// hasScript says package.json in dir has the script name.
func (f *taskFinder) hasScript(dir, name string) bool {
	if f.view == nil || dir == unknownDir {
		return false
	}
	scripts, ok := f.view.scripts(dir)
	_, has := scripts[name]
	return ok && has
}

// runScript reads the words after a runner's "run": its options, then
// the script; with orTool, one that is no script is a tool run so, as
// pnpm, yarn and bun do.
func (f *taskFinder) runScript(tool string, args []word, dir string, withArg, badFlags []string, orTool bool) {
	args, dir, ok := runnerFlags(args, dir, nil, withArg, badFlags)
	if !ok {
		f.none()
		return
	}
	if len(args) == 0 {
		// It lists the scripts.
		return
	}
	if !args[0].lit {
		f.none()
		return
	}
	if orTool && !f.hasScript(dir, args[0].text) {
		f.tool(args, dir)
		return
	}
	f.script(tool, args[0].text, dir)
}

var (
	// npmScriptShorthands are npm's commands that run a script: npm test
	// runs its test script, as npm t and npm tst do.
	npmScriptShorthands = map[string]string{"test": "test", "t": "test", "tst": "test", "start": "start", "stop": "stop", "restart": "restart"}
	npmWithArg          = []string{"--prefix", "--loglevel", "--userconfig", "--cache", "--registry"}
	npmOtherPackage     = []string{"-w", "--workspace", "--workspaces", "-ws", "--include-workspace-root"}
)

func (f *taskFinder) npm(args []word, dir string) {
	args, dir, ok := runnerFlags(args, dir, []string{"--prefix"}, npmWithArg, npmOtherPackage)
	if !ok {
		f.none()
		return
	}
	if len(args) == 0 || !args[0].lit {
		return
	}
	sub, rest := args[0].text, args[1:]
	switch sub {
	case "run", "run-script", "rum", "urn":
		f.runScript("npm", rest, dir, npmWithArg, npmOtherPackage, false)
	case "exec", "x":
		rest, dir, ok := runnerFlags(rest, dir, nil, []string{"-p", "--package", "-c", "--call"}, npmOtherPackage)
		if ok {
			f.tool(rest, dir)
		}
	default:
		if name, ok := npmScriptShorthands[sub]; ok {
			f.script("npm", name, dir)
		}
	}
}

var (
	// pnpmCommands are pnpm's own commands: anything else is a script, or
	// a package's binary.
	pnpmCommands = []string{"add", "audit", "bin", "ci", "config", "create", "dedupe", "deploy", "doctor", "env", "fetch", "i", "import",
		"init", "install", "install-test", "it", "licenses", "link", "list", "ln", "ls", "outdated", "pack", "patch", "patch-commit",
		"patch-remove", "prune", "publish", "rb", "rebuild", "recursive", "remove", "rm", "root", "server", "setup", "store", "un",
		"uninstall", "unlink", "up", "update", "upgrade", "why", "self-update", "cat-file", "cat-index", "find-hash", "approve-builds",
		"ignored-builds", "help", "-v", "--version"}
	pnpmWithArg      = []string{"--reporter", "--loglevel", "--workspace-concurrency", "--resume-from", "--changed-files-ignore-pattern", "--test-pattern"}
	pnpmOtherPackage = []string{"-r", "--recursive", "-F", "--filter", "--filter-prod", "-w", "--workspace-root", "--include-workspace-root"}
)

func (f *taskFinder) pnpm(args []word, dir string) {
	args, dir, ok := runnerFlags(args, dir, []string{"-C", "--dir"}, pnpmWithArg, pnpmOtherPackage)
	if !ok {
		f.none()
		return
	}
	if len(args) == 0 || !args[0].lit {
		return
	}
	sub, rest := args[0].text, args[1:]
	switch {
	case sub == "run" || sub == "run-script":
		f.runScript("pnpm", rest, dir, pnpmWithArg, pnpmOtherPackage, false)
	case sub == "exec" || sub == "dlx":
		rest, dir, ok := runnerFlags(rest, dir, nil, []string{"--package"}, pnpmOtherPackage)
		if ok {
			f.tool(rest, dir)
		}
	case sub == "test" || sub == "t" || sub == "tst":
		f.script("pnpm", "test", dir)
	case sub == "start":
		f.script("pnpm", "start", dir)
	case slices.Contains(pnpmCommands, sub):
	case f.hasScript(dir, sub):
		f.script("pnpm", sub, dir)
	default:
		f.tool(args, dir)
	}
}

var (
	yarnCommands = []string{"add", "audit", "autoclean", "bin", "cache", "check", "config", "constraints", "create", "dedupe", "explain",
		"generate-lock-entry", "global", "help", "import", "info", "init", "install", "licenses", "link", "list", "login", "logout",
		"node", "npm", "outdated", "owner", "pack", "patch", "patch-commit", "plugin", "policies", "publish", "rebuild", "remove", "set",
		"tag", "team", "unlink", "unplug", "up", "upgrade", "upgrade-interactive", "version", "versions", "why", "workspace",
		"workspaces", "-v", "--version"}
	yarnWithArg = []string{"--cache-folder", "--modules-folder", "--network-timeout", "--mutex"}
)

func (f *taskFinder) yarn(args []word, dir string) {
	args, dir, ok := runnerFlags(args, dir, []string{"--cwd"}, yarnWithArg, nil)
	if !ok {
		f.none()
		return
	}
	if len(args) == 0 || !args[0].lit {
		// yarn alone installs.
		return
	}
	sub, rest := args[0].text, args[1:]
	switch {
	case sub == "run":
		f.runScript("yarn", rest, dir, yarnWithArg, nil, true)
	case sub == "exec" || sub == "dlx":
		f.tool(rest, dir)
	case sub == "test" || sub == "start":
		f.script("yarn", sub, dir)
	case slices.Contains(yarnCommands, sub):
	case f.hasScript(dir, sub):
		f.script("yarn", sub, dir)
	default:
		f.tool(args, dir)
	}
}

// bunCommands are bun's own commands, none a repository's script; bun
// test, bun's own test runner, is a heavy tool.
var bunCommands = []string{"install", "i", "add", "a", "remove", "rm", "update", "outdated", "link", "unlink", "pm", "build",
	"init", "create", "c", "upgrade", "repl", "exec", "publish", "patch", "patch-commit", "audit", "info", "why", "completions",
	"help", "-v", "--version"}

func (f *taskFinder) bun(args []word, dir string) {
	args, dir, ok := runnerFlags(args, dir, []string{"--cwd"}, []string{"--filter", "-F", "--config", "-c", "--preload", "-r", "--env-file"}, []string{"--filter", "-F"})
	if !ok {
		f.none()
		return
	}
	if len(args) == 0 || !args[0].lit {
		return
	}
	sub, rest := args[0].text, args[1:]
	switch {
	case sub == "run":
		f.runScript("bun", rest, dir, nil, []string{"--filter", "-F"}, true)
	case sub == "x":
		f.tool(rest, dir)
	case sub == "test":
		f.add(taskEntry{Dir: dir, Tool: "bun test"})
	case slices.Contains(bunCommands, sub):
	case f.hasScript(dir, sub):
		f.script("bun", sub, dir)
	}
}

// goFlagsWithArg are the go command's flags, build and test, that take
// the next word when not given one by "=".
var goFlagsWithArg = []string{"-C", "-o", "-p", "-run", "-skip", "-bench", "-benchtime", "-count", "-cpu", "-timeout", "-tags", "-ldflags",
	"-gcflags", "-asmflags", "-mod", "-modfile", "-pkgdir", "-toolexec", "-coverprofile", "-covermode", "-coverpkg", "-cpuprofile",
	"-memprofile", "-memprofilerate", "-blockprofile", "-blockprofilerate", "-mutexprofile", "-mutexprofilefraction", "-trace",
	"-outputdir", "-parallel", "-list", "-shuffle", "-fuzz", "-fuzztime", "-fuzzminimizetime", "-exec", "-vet", "-overlay", "-pgo",
	"-installsuffix", "-compiler", "-gccgoflags", "-buildmode", "-buildvcs", "-covermode", "-test.run", "-test.count"}

// cargoFlagsWithArg are cargo build's and test's options that take the
// next word.
var cargoFlagsWithArg = []string{"-F", "--features", "--target", "--target-dir", "-j", "--jobs", "--profile", "--bin", "--test",
	"--example", "--bench", "--color", "--message-format", "-Z", "--config", "--exclude", "--timings", "--lockfile-path"}

// tool reads words as a tool called directly: a heavy one is a task.
func (f *taskFinder) tool(words []word, dir string) {
	if len(words) == 0 || !words[0].lit {
		return
	}
	program, args := path.Base(words[0].text), words[1:]
	sub := ""
	if len(args) > 0 && args[0].lit {
		sub = args[0].text
	}
	switch {
	case f.loose && (program == "go" && (sub == "test" || sub == "build" || sub == "vet") || program == "cargo" && (sub == "test" || sub == "build")):
		// A script is judged by its tools alone, whatever their words.
		f.add(taskEntry{Dir: dir, Tool: program + " " + sub})
	case program == "go" && (sub == "test" || sub == "build" || sub == "vet"):
		f.goTool("go "+sub, args[1:], dir)
	case program == "cargo" && (sub == "test" || sub == "build" || sub == "t" || sub == "b"):
		tool := map[string]string{"t": "test", "b": "build"}[sub]
		if tool == "" {
			tool = sub
		}
		f.cargo("cargo "+tool, args[1:], dir)
	case slices.Contains(directTools, program):
		f.add(taskEntry{Dir: dir, Tool: program})
	case !f.loose:
	case program == "webpack":
		f.add(taskEntry{Dir: dir, Tool: program})
	case (program == "vite" || program == "next") && sub == "build":
		f.add(taskEntry{Dir: dir, Tool: program + " build"})
	case program == "docker" && (sub == "build" || (sub == "buildx" && len(args) > 1 && args[1].text == "build")):
		f.add(taskEntry{Dir: dir, Tool: "docker build"})
	}
}

func (f *taskFinder) goTool(tool string, args []word, dir string) {
	var patterns []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !a.lit {
			f.none()
			return
		}
		if a.text == "-args" || a.text == "--args" {
			break
		}
		if strings.HasPrefix(a.text, "-") {
			name, value, hasValue := strings.Cut(strings.TrimPrefix(a.text, "-"), "=")
			name = "-" + strings.TrimPrefix(name, "-")
			if !slices.Contains(goFlagsWithArg, name) {
				continue
			}
			if !hasValue {
				if i+1 >= len(args) || !args[i+1].lit {
					f.none()
					return
				}
				i++
				value = args[i].text
			}
			if name == "-C" {
				dir = joinDir(dir, value)
			}
			continue
		}
		patterns = append(patterns, a.text)
	}
	if len(patterns) == 0 {
		patterns = []string{"."}
	}
	if !f.names(patterns, packagePattern) {
		return
	}
	f.add(taskEntry{Dir: dir, Tool: tool, Name: strings.Join(patterns, " ")})
}

func (f *taskFinder) cargo(tool string, args []word, dir string) {
	var packages []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !a.lit {
			f.none()
			return
		}
		if a.text == "--" {
			break
		}
		name, value, hasValue := strings.Cut(a.text, "=")
		switch {
		case name == "--manifest-path" || name == "-C":
			f.none()
			return
		case name == "-p" || name == "--package":
			if !hasValue {
				if i+1 >= len(args) || !args[i+1].lit {
					f.none()
					return
				}
				i++
				value = args[i].text
			}
			packages = append(packages, value)
		case slices.Contains(cargoFlagsWithArg, name) && !hasValue:
			i++
		}
	}
	if !f.names(packages, recipeName) {
		return
	}
	f.add(taskEntry{Dir: dir, Tool: tool, Name: strings.Join(packages, " ")})
}

// The words a task's key holds, as its text, are plain names
// (docs/decisions/process-usage.md, docs/decisions/boost.md): names the
// repository gives, never a value a caller may put a secret in.

// keyWord is the shape of a plain name: lower-case letters, digits, "-"
// and "_", starting with a letter, 32 characters at most.
var keyWord = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// nameWord reports whether w is a plain name: a keyWord that does not
// look like a token (hex, or digit-heavy as base64 is), which a short
// secret could still be.
func nameWord(w string) bool {
	return keyWord.MatchString(w) && !tokenShaped(w)
}

// tokenShaped says w, of 8 characters or more, is all hex or a quarter or
// more digits.
func tokenShaped(w string) bool {
	if len(w) < 8 {
		return false
	}
	hexDigits, digits := 0, 0
	for _, c := range strings.ToLower(w) {
		switch {
		case c >= '0' && c <= '9':
			digits++
			hexDigits++
		case c >= 'a' && c <= 'f':
			hexDigits++
		}
	}
	return hexDigits == len(w) || digits*4 >= len(w)
}

// makeTarget is a make target a key may hold: a plain name.
func makeTarget(t string) bool { return nameWord(t) }

// recipeName is a script's, recipe's or package's name a key may hold:
// plain names joined by ":", as "test:unit" and just's "mod::recipe" are,
// 64 characters at most.
func recipeName(n string) bool {
	if len(n) > 64 || strings.HasPrefix(n, ":") || strings.HasSuffix(n, ":") {
		return false
	}
	for part := range strings.SplitSeq(strings.ReplaceAll(n, "::", ":"), ":") {
		if !nameWord(part) {
			return false
		}
	}
	return true
}

// pathPart is a part of a directory or a package path a key may hold, as
// "internal", "v1alpha1" or "github.com": letters, digits, ".", "-" and
// "_", that no token looks like.
var pathPart = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$`)

// plainDir says dir, relative to /workspace, is one a key may hold.
func plainDir(dir string) bool {
	if dir == "." {
		return true
	}
	if dir == unknownDir || len(dir) > 200 {
		return false
	}
	for part := range strings.SplitSeq(dir, "/") {
		if !pathPart.MatchString(part) || tokenShaped(part) {
			return false
		}
	}
	return true
}

// packagePattern is a go package pattern a key may hold: ".", "./...",
// "./internal/x", a module's path, each part a pathPart or "...".
func packagePattern(p string) bool {
	if len(p) > 200 {
		return false
	}
	p = strings.TrimPrefix(p, "./")
	if p == "." || p == "..." {
		return true
	}
	for part := range strings.SplitSeq(p, "/") {
		part = strings.TrimSuffix(part, "...")
		if part == "" || part == "." || part == ".." {
			continue
		}
		if !pathPart.MatchString(part) || tokenShaped(part) {
			return false
		}
	}
	return true
}

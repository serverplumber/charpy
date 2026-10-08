package oracle_test

import (
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/vocabulary.txt from the declared constants")

const (
	moduleRoot     = "../.."
	vocabularyFile = "../../testdata/vocabulary.txt"
	// The package that declares the Check and Reason types.
	oracleDir = "internal/oracle"
)

// The vocabulary is what the oracle can report: every check and every reason,
// discovered from the constants that declare them rather than from the
// findings a transcript happens to produce. A verdict added, renamed or
// removed changes this file, so it is a diff someone reads even when no golden
// transcript reaches it. Regenerate with just vocabulary.
func TestVocabularyMatchesCheckedIn(t *testing.T) {
	v := scan(t)
	got := strings.Join(v.lines(), "\n") + "\n"

	if *update {
		if err := os.WriteFile(vocabularyFile, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(vocabularyFile)
	if err != nil {
		t.Fatalf("%v: run just vocabulary", err)
	}
	if got != string(want) {
		t.Errorf("the declared vocabulary differs from %s; read the diff, then run just vocabulary:\n--- want\n%s--- got\n%s",
			vocabularyFile, want, got)
	}
}

// Discovery is only as good as the convention it relies on: a check or reason
// set from anything but a declared constant would be reported without ever
// appearing in the vocabulary. The types alone do not stop that -- an untyped
// string literal converts implicitly -- so the convention is checked here.
func TestVocabularyIsOnlySetFromDeclaredConstants(t *testing.T) {
	for _, problem := range scan(t).problems {
		t.Error(problem)
	}
}

type entry struct{ layer, kind, value string }

type vocabulary struct {
	entries  []entry
	problems []string
}

func (v vocabulary) lines() []string {
	var out []string
	for _, e := range v.entries {
		out = append(out, e.layer+" "+e.kind+" "+e.value)
	}
	slices.Sort(out)
	return out
}

type source struct {
	path string // relative to the module root
	file *ast.File
	fset *token.FileSet
}

func (s source) at(n ast.Node) string {
	return s.path + ":" + strconv.Itoa(s.fset.Position(n.Pos()).Line)
}

// scan parses every non-test Go file under internal/ and cmd/, collects the
// constants of type oracle.Check and oracle.Reason, and reports every place a
// finding's Check or Reason is set from something else.
func scan(t *testing.T) vocabulary {
	t.Helper()
	byDir := map[string][]source{}
	fset := token.NewFileSet()
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(moduleRoot, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(moduleRoot, path)
			rel = filepath.ToSlash(rel)
			byDir[filepath.Dir(rel)] = append(byDir[filepath.Dir(rel)], source{rel, f, fset})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	var v vocabulary
	declared := map[string]bool{} // constant names, for the use check
	seen := map[string]string{}   // kind+value -> where it was declared
	dirs := make([]string, 0, len(byDir))
	for dir := range byDir {
		dirs = append(dirs, dir)
	}
	slices.Sort(dirs)

	for _, dir := range dirs {
		layer := layerOf(byDir[dir])
		for _, s := range byDir[dir] {
			for _, spec := range constSpecs(s.file) {
				kind := vocabularyKind(spec.Type, dir)
				if kind == "" {
					continue
				}
				for i, name := range spec.Names {
					where := s.at(name)
					value, ok := stringValue(spec, i)
					switch {
					case !ok:
						v.problems = append(v.problems, where+": "+name.Name+" must be a string literal, so it can be read without running anything")
					case layer == "":
						v.problems = append(v.problems, where+": "+name.Name+" is declared in a package with no Layer constant")
					case seen[kind+" "+value] != "":
						v.problems = append(v.problems, where+": "+kind+" "+value+" is already declared at "+seen[kind+" "+value]+"; a value means one thing")
					default:
						seen[kind+" "+value] = where
						declared[name.Name] = true
						v.entries = append(v.entries, entry{layer, kind, value})
					}
				}
			}
		}
	}

	for _, dir := range dirs {
		for _, s := range byDir[dir] {
			v.problems = append(v.problems, misuses(s, dir, declared)...)
		}
	}
	if len(v.entries) == 0 {
		t.Fatal("no vocabulary declared; the scan is looking in the wrong place")
	}
	return v
}

func constSpecs(f *ast.File) []*ast.ValueSpec {
	var out []*ast.ValueSpec
	for _, d := range f.Decls {
		if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.CONST {
			for _, sp := range g.Specs {
				out = append(out, sp.(*ast.ValueSpec))
			}
		}
	}
	return out
}

// vocabularyKind reports whether a type expression names oracle.Check or
// oracle.Reason, as "check" or "reason".
func vocabularyKind(expr ast.Expr, dir string) string {
	var name string
	switch e := expr.(type) {
	case *ast.SelectorExpr:
		if x, ok := e.X.(*ast.Ident); ok && x.Name == "oracle" {
			name = e.Sel.Name
		}
	case *ast.Ident:
		if dir == oracleDir {
			name = e.Name
		}
	}
	switch name {
	case "Check":
		return "check"
	case "Reason":
		return "reason"
	}
	return ""
}

func stringValue(spec *ast.ValueSpec, i int) (string, bool) {
	if i >= len(spec.Values) {
		return "", false
	}
	lit, ok := spec.Values[i].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// layerOf is the value of the package's Layer constant, which is what its
// findings are tagged with and so what the vocabulary is grouped by.
func layerOf(files []source) string {
	for _, s := range files {
		for _, spec := range constSpecs(s.file) {
			for i, name := range spec.Names {
				if name.Name == "Layer" {
					if v, ok := stringValue(spec, i); ok {
						return v
					}
				}
			}
		}
	}
	return ""
}

// misuses finds every way around the convention: a conversion to Check or
// Reason anywhere, and, inside the oracle, a Check or Reason field set from
// anything but a declared constant.
func misuses(s source, dir string, declared map[string]bool) []string {
	inOracle := dir == oracleDir || strings.HasPrefix(dir, oracleDir+"/")
	var out []string
	settable := func(field string, value ast.Expr, n ast.Node) {
		if field != "Check" && field != "Reason" {
			return
		}
		var name string
		switch e := value.(type) {
		case *ast.Ident:
			name = e.Name
		case *ast.SelectorExpr:
			name = e.Sel.Name
		}
		if !declared[name] {
			out = append(out, s.at(n)+": "+field+" is set from something that is not a declared "+
				strings.ToLower(field)+" constant; declare one, so the vocabulary can see it")
		}
	}

	ast.Inspect(s.file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if len(n.Args) == 1 && vocabularyKind(n.Fun, dir) != "" {
				out = append(out, s.at(n)+": a conversion to a vocabulary type makes a value no declaration names")
			}
		case *ast.KeyValueExpr:
			if key, ok := n.Key.(*ast.Ident); ok && inOracle {
				settable(key.Name, n.Value, n)
			}
		case *ast.AssignStmt:
			if !inOracle || len(n.Lhs) != len(n.Rhs) {
				return true
			}
			for i, lhs := range n.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok {
					settable(sel.Sel.Name, n.Rhs[i], n)
				}
			}
		}
		return true
	})
	return out
}

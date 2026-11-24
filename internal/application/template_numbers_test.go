package application_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Edge's templates are the Go code of this package (Edge ADR 0002), and every number that they
// write into a rendered file comes from the defaults file or the inputs (section 03). This check
// reads the package's source and finds every number in it except 0 and 1, which count, index and
// state the fixed decisions of the design, such as a weight or "never retry".

// allowedNumbers are the other numbers that the package may carry, by the function that carries
// them and with the reason. None of them is a value that an installation tunes.
var allowedNumbers = map[string]map[string]string{
	"routeID": {"6": "a route's id carries the first 12 hexadecimal digits of the hash, 6 bytes (section 03)"},
	"check":   {"65535": "the largest TCP port, which the rule of the instances file names"},
}

func TestNoTemplateCarriesANumberOfItsOwn(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		found = append(found, numbersIn(t, name, string(source))...)
	}
	if len(found) > 0 {
		t.Errorf("numbers that belong in the defaults file:\n%s", strings.Join(found, "\n"))
	}
}

func TestANumberInATemplateIsFound(t *testing.T) {
	const source = `package application

func upstream(slug Slug) Upstream {
	return Upstream{ID: string(slug), Retries: 0, Timeout: 30, Nodes: make([]Node, 1)}
}

func routeID() string { return hash()[:6] }

var bodyMax = 8.5e6
`

	found := numbersIn(t, "fixture.go", source)

	want := []string{
		"fixture.go:4: upstream carries 30, which belongs in the defaults file",
		"fixture.go:9: the package carries 8.5e6, which belongs in the defaults file",
	}
	if !slices.Equal(found, want) {
		t.Errorf("got %q, want %q", found, want)
	}
}

// numbersIn lists every number in source except 0, 1 and allowedNumbers, with its line and the
// function that carries it.
func numbersIn(t *testing.T, name, source string) []string {
	t.Helper()
	files := token.NewFileSet()
	file, err := parser.ParseFile(files, name, source, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	inspect := func(carrier string, node ast.Node) {
		ast.Inspect(node, func(n ast.Node) bool {
			literal, isLiteral := n.(*ast.BasicLit)
			if !isLiteral || (literal.Kind != token.INT && literal.Kind != token.FLOAT && literal.Kind != token.IMAG) {
				return true
			}
			if literal.Value == "0" || literal.Value == "1" {
				return true
			}
			if _, allowed := allowedNumbers[carrier][literal.Value]; allowed {
				return true
			}
			where := "the package"
			if carrier != "" {
				where = carrier
			}
			found = append(found, fmt.Sprintf("%s:%d: %s carries %s, which belongs in the defaults file",
				name, files.Position(literal.Pos()).Line, where, literal.Value))
			return true
		})
	}
	for _, declaration := range file.Decls {
		if function, isFunction := declaration.(*ast.FuncDecl); isFunction {
			inspect(function.Name.Name, function)
		} else {
			inspect("", declaration)
		}
	}
	return found
}

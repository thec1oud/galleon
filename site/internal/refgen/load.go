// Package refgen generates the docs' reference pages for error codes and
// config sections from the source of Galleon's core module, so they can't
// drift from it. It reads the code with full type information, and fails
// rather than guess when it meets a code or a section it can't resolve.
package refgen

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Module is the core module's path.
const Module = "github.com/thec1oud/galleon"

// source is the core module's packages, loaded with types.
type source struct {
	fset *token.FileSet
	pkgs []*packages.Package
	// docs holds the doc comment of every struct field and constant of the
	// module, by the position of its name.
	docs map[token.Pos]doc
}

type doc struct {
	text string // its own doc comment, or one it shares
	note string // the comment at the end of its line
}

// load reads the non-test packages of the core module, from root, a
// directory in its workspace. env adds to the go command's environment.
func load(root string, env ...string) (*source, error) {
	cfg := &packages.Config{
		Env: append(os.Environ(), env...),
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes |
			packages.NeedTypesInfo | packages.NeedModule,
		Dir:  root,
		Fset: token.NewFileSet(),
	}
	pkgs, err := packages.Load(cfg, Module+"/...")
	if err != nil {
		return nil, err
	}
	s := &source{fset: cfg.Fset, docs: map[token.Pos]doc{}}
	for _, p := range pkgs {
		if p.Module == nil || p.Module.Path != Module {
			continue
		}
		if len(p.Errors) > 0 {
			return nil, fmt.Errorf("refgen: %s: %v", p.PkgPath, p.Errors[0])
		}
		s.pkgs = append(s.pkgs, p)
		for _, f := range p.Syntax {
			s.indexDocs(f)
		}
	}
	if len(s.pkgs) == 0 {
		return nil, fmt.Errorf("refgen: no packages of %s found from %s", Module, root)
	}
	return s, nil
}

// indexDocs records the docs of f's constants and struct fields. One
// without a doc comment of its own shares the doc of the one on the line
// above when that doc names it, as in "MaxConns and MinConns size the
// pool"; a doc that doesn't name it may be about the line above only.
func (s *source) indexDocs(f *ast.File) {
	line := func(p token.Pos) int { return s.fset.Position(p).Line }
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.GenDecl:
			if x.Tok != token.CONST && x.Tok != token.VAR {
				return true
			}
			prev, prevEnd := "", -1
			for _, sp := range x.Specs {
				vs := sp.(*ast.ValueSpec)
				text := strings.TrimSpace(vs.Doc.Text())
				if text == "" && line(vs.Pos()) == prevEnd+1 && mentions(prev, vs.Names[0].Name) {
					text = prev
				}
				if text == "" && len(x.Specs) == 1 {
					text = strings.TrimSpace(x.Doc.Text())
				}
				if text != "" {
					prev = text
				}
				prevEnd = line(vs.End())
				d := doc{text, strings.TrimSpace(vs.Comment.Text())}
				for _, n := range vs.Names {
					s.docs[n.Pos()] = d
				}
			}
		case *ast.StructType:
			prev, prevEnd := "", -1
			for _, fl := range x.Fields.List {
				text := strings.TrimSpace(fl.Doc.Text())
				if text == "" && line(fl.Pos()) == prevEnd+1 && len(fl.Names) > 0 && mentions(prev, fl.Names[0].Name) {
					text = prev
				}
				if text != "" {
					prev = text
				}
				prevEnd = line(fl.End())
				d := doc{text, strings.TrimSpace(fl.Comment.Text())}
				for _, n := range fl.Names {
					s.docs[n.Pos()] = d
				}
				if len(fl.Names) == 0 {
					s.docs[fl.Type.Pos()] = d
				}
			}
		}
		return true
	})
}

// mentions reports whether text has name as a whole word.
func mentions(text, name string) bool {
	for _, w := range strings.FieldsFunc(text, func(r rune) bool {
		return !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	}) {
		if w == name {
			return true
		}
	}
	return false
}

// rel returns a package path relative to the module: "" for the root.
func rel(pkgPath string) string {
	return strings.TrimPrefix(strings.TrimPrefix(pkgPath, Module), "/")
}

// where describes a position for an error message.
func (s *source) where(p token.Pos) string {
	pos := s.fset.Position(p)
	return fmt.Sprintf("%s:%d", path.Join(path.Base(path.Dir(pos.Filename)), path.Base(pos.Filename)), pos.Line)
}

// isObj reports whether obj is the package-level object name of package
// pkgPath.
func isObj(obj types.Object, pkgPath, name string) bool {
	return obj != nil && obj.Pkg() != nil && obj.Pkg().Path() == pkgPath && obj.Name() == name
}

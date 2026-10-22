package refgen

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/types"
	"slices"
	"sort"
	"strings"
)

const serverPath = Module + "/server"

// statuses finds the codes the HTTP server sends with a status other than
// their kind's: the calls of Server.writeError with a status that isn't
// zero. The status is a constant, or a variable the function sets to one
// in an `if errors.Is(err, e)` block, where e is a package variable made
// with errs.New. The error is an errs.New call or such a variable.
func (s *source) statuses() (map[string]int, error) {
	vars := map[types.Object]ast.Expr{} // package variables, by what they are set to
	for _, p := range s.pkgs {
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				g, ok := d.(*ast.GenDecl)
				if !ok {
					continue
				}
				for _, sp := range g.Specs {
					if vs, ok := sp.(*ast.ValueSpec); ok && len(vs.Names) == len(vs.Values) {
						for i, n := range vs.Names {
							vars[p.TypesInfo.Defs[n]] = vs.Values[i]
						}
					}
				}
			}
		}
	}
	out := map[string]int{}
	var problems []string
	for _, p := range s.pkgs {
		info := p.TypesInfo
		// codeOf returns the constant code of an errs.New call, or of the
		// package variable e names.
		codeOf := func(e ast.Expr) string {
			if id, ok := ast.Unparen(e).(*ast.Ident); ok {
				if v, ok := vars[info.Uses[id]]; ok {
					e = v
				}
			}
			prs, _ := pairs(ast.Unparen(e), info)
			for _, pr := range prs {
				if tv := info.Types[pr.code]; tv.Value != nil && tv.Value.Kind() == constant.String {
					return constant.StringVal(tv.Value)
				}
			}
			return ""
		}
		statusOf := func(e ast.Expr) (int, bool) {
			tv := info.Types[e]
			if tv.Value == nil {
				return 0, false
			}
			n, _ := constant.Int64Val(tv.Value)
			return int(n), true
		}
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok || !isMethod(callee(call, info), serverPath, "Server", "writeError") || len(call.Args) != 4 {
						return true
					}
					errArg, statusArg := call.Args[2], call.Args[3]
					if st, ok := statusOf(statusArg); ok {
						if st == 0 {
							return true
						}
						if code := codeOf(errArg); code != "" {
							if out[code] != 0 && out[code] != st {
								problems = append(problems, fmt.Sprintf("%s: %s is sent with status %d here and %d elsewhere", s.where(call.Pos()), code, st, out[code]))
							}
							out[code] = st
							return true
						}
						problems = append(problems, fmt.Sprintf("%s: writeError sends status %d with an error whose code can't be told", s.where(call.Pos()), st))
						return true
					}
					id, ok := statusArg.(*ast.Ident)
					if !ok {
						problems = append(problems, fmt.Sprintf("%s: writeError's status is neither a constant nor a variable", s.where(call.Pos())))
						return true
					}
					// The status must be a local of this function, and each use
					// of it the call or an assignment that sets it to 0, or to
					// a constant inside an `if errors.Is(err, e)` block.
					target := info.ObjectOf(id)
					if target == nil || target.Pos() < fd.Body.Pos() || target.Pos() > fd.Body.End() {
						problems = append(problems, fmt.Sprintf("%s: writeError's status %s isn't a local of %s, so the codes it is sent with can't be told", s.where(call.Pos()), id.Name, fd.Name.Name))
						return true
					}
					seen := map[*ast.Ident]bool{}
					ast.Inspect(fd.Body, func(n ast.Node) bool {
						if c, ok := n.(*ast.CallExpr); ok && isMethod(callee(c, info), serverPath, "Server", "writeError") && len(c.Args) == 4 {
							if a, ok := c.Args[3].(*ast.Ident); ok {
								seen[a] = true
							}
						}
						return true
					})
					var stack []ast.Node
					ast.Inspect(fd.Body, func(n ast.Node) bool {
						if n == nil {
							stack = stack[:len(stack)-1]
							return true
						}
						stack = append(stack, n)
						as, ok := n.(*ast.AssignStmt)
						if !ok || len(as.Lhs) != len(as.Rhs) {
							return true
						}
						for i, l := range as.Lhs {
							lid, ok := l.(*ast.Ident)
							if !ok || info.ObjectOf(lid) != target {
								continue
							}
							seen[lid] = true
							v, isConst := statusOf(as.Rhs[i])
							if isConst && v == 0 {
								continue
							}
							code := ""
							for j := len(stack) - 2; j >= 0 && code == ""; j-- {
								if ifs, ok := stack[j].(*ast.IfStmt); ok {
									if cond, ok := ifs.Cond.(*ast.CallExpr); ok && stack[j+1] == ifs.Body && isObj(callee(cond, info), "errors", "Is") && len(cond.Args) == 2 {
										code = codeOf(cond.Args[1])
									}
									break
								}
							}
							switch {
							case !isConst || code == "":
								problems = append(problems, fmt.Sprintf("%s: %s is set to a status that isn't a constant inside an `if errors.Is(err, e)` block with a known code", s.where(as.Pos()), id.Name))
							case out[code] != 0 && out[code] != v:
								problems = append(problems, fmt.Sprintf("%s: %s is sent with status %d here and %d elsewhere", s.where(as.Pos()), code, v, out[code]))
							default:
								out[code] = v
							}
						}
						return true
					})
					for use, obj := range info.Defs {
						if obj == target {
							seen[use] = true // its declaration, which an assignment checked above or a var gives 0
							if vs, ok := enclosingSpec(fd.Body, use); ok && len(vs.Values) > 0 {
								problems = append(problems, fmt.Sprintf("%s: declare %s with := 0 or no value, and set it in an `if errors.Is(err, e)` block", s.where(use.Pos()), id.Name))
							}
						}
					}
					for use, obj := range info.Uses {
						if obj == target && !seen[use] {
							problems = append(problems, fmt.Sprintf("%s: %s is used other than by writeError or an assignment, so the codes it is sent with can't be told", s.where(use.Pos()), id.Name))
						}
					}
					return true
				})
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("refgen:\n  %s", strings.Join(slices.Compact(problems), "\n  "))
	}
	return out, nil
}

// isMethod reports whether obj is method name of type typ in package
// pkgPath.
func isMethod(obj types.Object, pkgPath, typ, name string) bool {
	fn, ok := obj.(*types.Func)
	if !ok || !isObj(fn, pkgPath, name) {
		return false
	}
	recv := fn.Signature().Recv()
	if recv == nil {
		return false
	}
	n, ok := deref(recv.Type()).(*types.Named)
	return ok && n.Obj().Name() == typ
}

// durationConst returns the value of constant name of package pkgPath, a
// time.Duration.
func (s *source) durationConst(pkgPath, name string) (int64, error) {
	for _, p := range s.pkgs {
		if p.PkgPath != pkgPath {
			continue
		}
		c, ok := p.Types.Scope().Lookup(name).(*types.Const)
		var t *types.Named
		if ok {
			t, ok = c.Type().(*types.Named)
		}
		if !ok || !isObj(t.Obj(), "time", "Duration") {
			return 0, fmt.Errorf("refgen: %s.%s isn't a time.Duration constant", pkgPath, name)
		}
		v, _ := constant.Int64Val(c.Val())
		return v, nil
	}
	return 0, fmt.Errorf("refgen: package %s not found", pkgPath)
}

// enclosingSpec returns the var spec in body that declares id, if a var
// statement does.
func enclosingSpec(body ast.Node, id *ast.Ident) (*ast.ValueSpec, bool) {
	var out *ast.ValueSpec
	ast.Inspect(body, func(n ast.Node) bool {
		if vs, ok := n.(*ast.ValueSpec); ok && slices.Contains(vs.Names, id) {
			out = vs
		}
		return out == nil
	})
	return out, out != nil
}

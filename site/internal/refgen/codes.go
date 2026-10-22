package refgen

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// A Code is one error catalog code.
type Code struct {
	Code     string
	Module   string   // the module it is listed under
	Packages []string // the packages that return it, relative to the core module
	Kinds    []string // the kinds it is returned with
	Doc      string   // the doc comment of its constant, if it has one
	Note     string   // the comment at the end of its constant's line
	Messages []string // the messages it is returned with, as written
}

// dynamic lists the functions that pass on a kind and a code they computed,
// as "package.func". Their codes are found where they are computed, such as
// pg's sqlstate table, so the generator doesn't need to follow them.
var dynamic = map[string]bool{
	"pg.classify": true,
}

const errsPath = Module + "/errs"

// codes finds every error code the module returns: wherever a constant
// errs.Kind is followed by a string (errs.New(kind, code, …), errs.Wrap(err,
// kind, code, …), return kind, code, …) or set beside one in an errs.Error,
// and wherever a helper that passes its parameter on as such a code is
// called. A code that isn't a constant fails, unless its function is listed
// in dynamic.
func (s *source) codes() ([]Code, error) {
	kinds, err := s.kinds()
	if err != nil {
		return nil, err
	}
	sc := &scanner{s: s, kinds: kinds, helpers: map[types.Object][]helper{}, all: map[string]*Code{}, called: map[*ast.Ident]bool{}}
	// Find the helpers, and the helpers that call them, until there are
	// no more; then record what every call returns.
	for sc.grew = true; sc.grew; {
		sc.grew = false
		sc.scan(false)
	}
	sc.scan(true)
	// A helper used as a value, not called, returns codes no call shows.
	for _, p := range s.pkgs {
		for id, obj := range p.TypesInfo.Uses {
			if fn, ok := obj.(*types.Func); ok {
				obj = fn.Origin()
			}
			if _, ok := sc.helpers[obj]; ok && !sc.called[id] {
				sc.problem(id.Pos(), "%s passes a parameter on as an error's code, and is used other than in a call, so the codes it returns can't be listed", id.Name)
			}
		}
	}
	if len(sc.problems) > 0 {
		sort.Strings(sc.problems)
		return nil, fmt.Errorf("refgen:\n  %s", strings.Join(slices.Compact(sc.problems), "\n  "))
	}
	if err := s.describe(sc.all); err != nil {
		return nil, err
	}
	var out []Code
	for _, c := range sc.all {
		sort.Strings(c.Kinds)
		sort.Strings(c.Packages)
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out, nil
}

// checkInterfaceCall fails a call of an interface method that a helper
// method implements, since the call can't tell which method it reaches.
func (sc *scanner) checkInterfaceCall(call *ast.CallExpr, info *types.Info) {
	fn, ok := callee(call, info).(*types.Func)
	if !ok || fn.Signature().Recv() == nil {
		return
	}
	iface, ok := fn.Signature().Recv().Type().Underlying().(*types.Interface)
	if !ok {
		return
	}
	for obj := range sc.helpers {
		m, ok := obj.(*types.Func)
		if !ok || m.Name() != fn.Name() || m.Signature().Recv() == nil {
			continue
		}
		recv := m.Signature().Recv().Type()
		if types.Implements(recv, iface) || types.Implements(types.NewPointer(deref(recv)), iface) {
			sc.problem(call.Pos(), "%s is called through an interface, and %s passes a parameter on as an error's code, so the codes it returns can't be listed", fn.Name(), m.FullName())
			return
		}
	}
}

// kinds returns the names of errs' Kind constants, in the order of their
// values, which must run from 0 without gaps.
func (s *source) kinds() ([]string, error) {
	for _, p := range s.pkgs {
		if p.PkgPath != errsPath {
			continue
		}
		var names []string
		scope := p.Types.Scope()
		for _, name := range scope.Names() {
			c, ok := scope.Lookup(name).(*types.Const)
			if !ok || !isErrsKind(c.Type()) {
				continue
			}
			i, _ := constant.Int64Val(c.Val())
			for int64(len(names)) <= i {
				names = append(names, "")
			}
			if names[i] != "" {
				return nil, fmt.Errorf("refgen: errs.%s and errs.%s are both Kind %d", names[i], name, i)
			}
			names[i] = name
		}
		if slices.Contains(names, "") || len(names) == 0 {
			return nil, fmt.Errorf("refgen: errs' kinds don't run from 0 without gaps: %v", names)
		}
		return names, nil
	}
	return nil, fmt.Errorf("refgen: package %s not found", errsPath)
}

// describe gives each code the doc and line comment of the package-level
// constant that declares it, whose name starts with "Code". Two constants
// with docs for one code fail.
func (s *source) describe(all map[string]*Code) error {
	from := map[string]string{}
	for _, p := range s.pkgs {
		for _, name := range p.Types.Scope().Names() {
			c, ok := p.Types.Scope().Lookup(name).(*types.Const)
			if !ok || !strings.HasPrefix(name, "Code") || c.Val().Kind() != constant.String {
				continue
			}
			code, ok := all[constant.StringVal(c.Val())]
			if !ok {
				continue
			}
			d := s.docs[c.Pos()]
			text := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(d.text, name+" is "), name+": "))
			if text == "" && d.note == "" {
				continue
			}
			if prev, ok := from[code.Code]; ok {
				return fmt.Errorf("refgen: %s and %s both describe %s: keep one", prev, s.where(c.Pos()), code.Code)
			}
			from[code.Code] = s.where(c.Pos())
			code.Doc, code.Note = text, d.note
		}
	}
	return nil
}

// A helper is a function that passes a parameter on as an error's code:
// code is that parameter's index. Its kind is a constant, or kindParam is
// the index of the parameter it passes on as the kind; likewise its
// message is msg, or the parameter msgParam.
type helper struct {
	code      int
	kind      string
	kindParam int
	msg       string
	msgParam  int
}

type scanner struct {
	s        *source
	kinds    []string
	helpers  map[types.Object][]helper // every way each helper returns a code
	grew     bool
	all      map[string]*Code
	problems []string
	called   map[*ast.Ident]bool // the names calls call
}

func (sc *scanner) addHelper(obj types.Object, h helper) {
	if !slices.Contains(sc.helpers[obj], h) {
		sc.helpers[obj] = append(sc.helpers[obj], h)
		sc.grew = true
	}
}

func (sc *scanner) problem(pos token.Pos, format string, args ...any) {
	sc.problems = append(sc.problems, sc.s.where(pos)+": "+fmt.Sprintf(format, args...))
}

func (sc *scanner) get(code string) *Code {
	c := sc.all[code]
	if c == nil {
		c = &Code{Code: code}
		sc.all[code] = c
	}
	return c
}

// scan walks the module. Only the last scan, with record set, records
// codes and reports problems; the others find helpers.
func (sc *scanner) scan(record bool) {
	s := sc.s
	for _, p := range s.pkgs {
		info := p.TypesInfo
		kindOf := func(e ast.Expr) string {
			tv := info.Types[e]
			if tv.Value == nil {
				return ""
			}
			i, _ := constant.Int64Val(tv.Value)
			if i < 0 || int(i) >= len(sc.kinds) {
				return ""
			}
			return sc.kinds[i]
		}
		constCode := func(e ast.Expr) (string, bool) {
			tv := info.Types[e]
			if tv.Value == nil || tv.Value.Kind() != constant.String {
				return "", false
			}
			return constant.StringVal(tv.Value), true
		}
		for _, f := range p.Syntax {
			var funcs []ast.Node // enclosing functions, innermost last
			var stack []ast.Node
			ast.Inspect(f, func(n ast.Node) bool {
				if n == nil {
					top := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					if len(funcs) > 0 && funcs[len(funcs)-1] == top {
						funcs = funcs[:len(funcs)-1]
					}
					return true
				}
				stack = append(stack, n)
				switch n.(type) {
				case *ast.FuncDecl, *ast.FuncLit:
					funcs = append(funcs, n)
				}
				fn, obj := s.enclosing(funcs, info, stack)
				dynamicHere := dynamic[p.Name+"."+fn]
				msgOf := func(e ast.Expr) (string, int) {
					if e == nil {
						return "", -1
					}
					if i := paramIndex(funcs, info, e); i >= 0 {
						return "", i
					}
					return message(e, info), -1
				}
				found := func(codeExpr ast.Expr, code, kind, msg string) {
					if !record {
						return
					}
					if !catalogCode(code) {
						sc.problem(codeExpr.Pos(), "%q isn't a catalog code, <module>.<name> in lower snake case", code)
						return
					}
					sc.record(sc.get(code), kind, p.PkgPath, msg)
				}
				prs, bad := pairs(n, info)
				if bad != nil && record {
					sc.problem(bad.Pos(), "an errs.Error literal sets a Code but no Kind, so the reference can't tell its kind; set both")
				}
				// A call of a helper, such as errs.New, is followed as one, below.
				if call, ok := n.(*ast.CallExpr); ok {
					hs := sc.helpers[callee(call, info)]
					prs = slices.DeleteFunc(prs, func(pr pair) bool {
						return slices.ContainsFunc(hs, func(h helper) bool { return h.code < len(call.Args) && call.Args[h.code] == pr.code })
					})
				}
				for _, pr := range prs {
					kind := kindOf(pr.kind)
					if code, ok := constCode(pr.code); ok {
						if _, ret := n.(*ast.ReturnStmt); ret && code == "" {
							continue // a zero result beside an ok of false, as pg's sqlstate returns
						}
						if kind == "" && record {
							sc.problem(pr.code.Pos(), "code %q comes with a kind that isn't a constant", code)
						}
						m, _ := msgOf(pr.msg)
						found(pr.code, code, kind, m)
						continue
					}
					codeParam := paramIndex(funcs, info, pr.code)
					kindParam := paramIndex(funcs, info, pr.kind)
					switch {
					case dynamicHere:
					case codeParam >= 0 && obj != nil && (kind != "" || kindParam >= 0):
						m, mp := msgOf(pr.msg)
						sc.addHelper(obj, helper{codeParam, kind, kindParam, m, mp})
					case !record:
					case codeParam >= 0 && obj == nil:
						sc.problem(pr.code.Pos(), "a function literal passes its parameter on as an error's code, but isn't assigned to a variable, so its calls can't be followed")
					case codeParam >= 0:
						sc.problem(pr.kind.Pos(), "an error's kind is neither a constant nor a parameter, so the reference can't list its codes")
					default:
						sc.problem(pr.code.Pos(), "an error's code isn't a constant, so the reference can't list it; use a constant, or list %s.%s in refgen.dynamic if its codes are found elsewhere", p.Name, fn)
					}
				}
				if as, ok := n.(*ast.AssignStmt); ok {
					for i, r := range as.Rhs {
						if _, lit := r.(*ast.FuncLit); lit && i < len(as.Lhs) {
							if id, ok := as.Lhs[i].(*ast.Ident); ok {
								sc.called[id] = true // f = func(code string) …, followed through f's calls
							}
						}
					}
				}
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id := calleeIdent(call); id != nil {
					sc.called[id] = true
				}
				if record {
					sc.checkInterfaceCall(call, info)
				}
				for _, h := range sc.helpers[callee(call, info)] {
					if h.code >= len(call.Args) || h.kindParam >= len(call.Args) {
						continue // a variadic call that leaves them out
					}
					codeArg := call.Args[h.code]
					kind, kindParam := h.kind, -1
					if h.kindParam >= 0 {
						kind = kindOf(call.Args[h.kindParam])
						if kind == "" {
							kindParam = paramIndex(funcs, info, call.Args[h.kindParam])
						}
					}
					msg, msgParam := h.msg, -1
					if h.msgParam >= 0 && h.msgParam < len(call.Args) {
						msg, msgParam = msgOf(call.Args[h.msgParam])
					}
					code, isConst := constCode(codeArg)
					codeParam := paramIndex(funcs, info, codeArg)
					switch {
					case isConst && kind != "":
						found(codeArg, code, kind, msg)
					case dynamicHere:
					case codeParam >= 0 && obj != nil && (kind != "" || kindParam >= 0):
						sc.addHelper(obj, helper{codeParam, kind, kindParam, msg, msgParam})
					case !record:
					case isConst:
						sc.problem(codeArg.Pos(), "code %q comes with a kind that isn't a constant", code)
					case codeParam >= 0 && obj == nil:
						sc.problem(codeArg.Pos(), "a function literal passes its parameter on as an error's code, but isn't assigned to a variable, so its calls can't be followed")
					case codeParam >= 0:
						sc.problem(call.Args[h.kindParam].Pos(), "an error's kind is neither a constant nor a parameter, so the reference can't list its codes")
					default:
						sc.problem(codeArg.Pos(), "an error's code isn't a constant, so the reference can't list it; use a constant, or list %s.%s in refgen.dynamic if its codes are found elsewhere", p.Name, fn)
					}
				}
				return true
			})
		}
	}
}

// record notes that c is returned with kind by package pkgPath, with the
// message msg, unless that is "".
func (sc *scanner) record(c *Code, kind, pkgPath, msg string) {
	// A code is listed under the first module, in the pages' order, that
	// returns it: testkit's fakes return the core's codes, which stay the
	// core's.
	if mod := sc.s.moduleOf(pkgPath); c.Module == "" || slices.Index(sc.s.modules, mod) < slices.Index(sc.s.modules, c.Module) {
		c.Module = mod
	}
	if kind != "" && !slices.Contains(c.Kinds, kind) {
		c.Kinds = append(c.Kinds, kind)
	}
	if r := rel(pkgPath); !slices.Contains(c.Packages, r) {
		c.Packages = append(c.Packages, r)
	}
	if msg != "" && !slices.Contains(c.Messages, msg) {
		c.Messages = append(c.Messages, msg)
	}
}

// verbs matches the verbs of a format string.
var verbs = regexp.MustCompile(`%[-+# 0-9.*]*[a-zA-Z%]`)

// message returns what msg says: a constant string, the format of an
// fmt.Sprintf or fmt.Errorf with verbs shown as "…", or the constant start
// of a concatenation.
func message(msg ast.Expr, info *types.Info) string {
	if msg == nil {
		return ""
	}
	if tv := info.Types[msg]; tv.Value != nil && tv.Value.Kind() == constant.String {
		return constant.StringVal(tv.Value)
	}
	switch m := msg.(type) {
	case *ast.CallExpr:
		if fn := callee(m, info); fn != nil && fn.Pkg() != nil && fn.Pkg().Path() == "fmt" && len(m.Args) > 0 {
			if f := message(m.Args[0], info); f != "" {
				return verbs.ReplaceAllString(f, "…")
			}
		}
	case *ast.BinaryExpr:
		if m.Op == token.ADD {
			return concat(m, info)
		}
	}
	return ""
}

// concat writes a concatenation's constant parts, and "…" for the others.
func concat(e ast.Expr, info *types.Info) string {
	if tv := info.Types[e]; tv.Value != nil && tv.Value.Kind() == constant.String {
		return constant.StringVal(tv.Value)
	}
	if b, ok := ast.Unparen(e).(*ast.BinaryExpr); ok && b.Op == token.ADD {
		return concat(b.X, info) + concat(b.Y, info)
	}
	return "…"
}

// pair is a kind followed by a code, and the message after it, if any.
type pair struct{ kind, code, msg ast.Expr }

// pairs finds the kinds followed by codes in n: in a call's arguments, a
// return's results, or a composite literal's elements, a struct's in the
// order of its fields, so an errs.Error's Kind, Code and Message are a
// pair. An errs.Error literal that sets a Code but no Kind is returned as
// bad, since its kind is set elsewhere and its code can't be listed.
func pairs(n ast.Node, info *types.Info) (out []pair, bad ast.Expr) {
	var list []ast.Expr
	switch x := n.(type) {
	case *ast.CallExpr:
		list = x.Args
	case *ast.ReturnStmt:
		list = x.Results
	case *ast.CompositeLit:
		t, ok := info.Types[x]
		if !ok {
			return nil, nil
		}
		st, ok := t.Type.Underlying().(*types.Struct)
		if !ok {
			list = x.Elts
			break
		}
		list = make([]ast.Expr, st.NumFields())
		for i, e := range x.Elts {
			if kv, ok := e.(*ast.KeyValueExpr); ok {
				for j := 0; j < st.NumFields(); j++ {
					if st.Field(j).Name() == kv.Key.(*ast.Ident).Name {
						list[j] = kv.Value
					}
				}
			} else if i < len(list) {
				list[i] = e
			}
		}
		if isErrsError(t.Type) && list[1] != nil && list[0] == nil {
			return nil, list[1]
		}
	}
	for i := 0; i+1 < len(list); i++ {
		if list[i] == nil || list[i+1] == nil {
			continue
		}
		tv, ok := info.Types[list[i]]
		if !ok || !isErrsKind(tv.Type) {
			continue
		}
		next, ok := info.Types[list[i+1]]
		if !ok {
			continue // a key-value element of a map or slice
		}
		if b, ok := next.Type.Underlying().(*types.Basic); !ok || b.Info()&types.IsString == 0 {
			continue
		}
		p := pair{kind: list[i], code: list[i+1]}
		if i+2 < len(list) {
			p.msg = list[i+2]
		}
		out = append(out, p)
	}
	return out, nil
}

func isErrsKind(t types.Type) bool {
	n, ok := t.(*types.Named)
	return ok && isObj(n.Obj(), errsPath, "Kind")
}

func isErrsError(t types.Type) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	return ok && isObj(n.Obj(), errsPath, "Error")
}

// callee returns the function or variable a call calls, if it can tell:
// for a generic function or method, the generic one.
func callee(call *ast.CallExpr, info *types.Info) types.Object {
	id := calleeIdent(call)
	if id == nil {
		return nil
	}
	obj := info.Uses[id]
	if fn, ok := obj.(*types.Func); ok {
		return fn.Origin()
	}
	return obj
}

// calleeIdent returns the name a call calls: f in f(), x.f(), f[T]() or
// x.f[T, U]().
func calleeIdent(call *ast.CallExpr) *ast.Ident {
	fn := ast.Unparen(call.Fun)
	switch x := fn.(type) {
	case *ast.IndexExpr:
		fn = x.X
	case *ast.IndexListExpr:
		fn = x.X
	}
	switch x := fn.(type) {
	case *ast.Ident:
		return x
	case *ast.SelectorExpr:
		return x.Sel
	}
	return nil
}

// paramIndex returns the index of the parameter of the innermost function
// that e names, or -1.
func paramIndex(funcs []ast.Node, info *types.Info, e ast.Expr) int {
	id, ok := ast.Unparen(e).(*ast.Ident)
	if !ok || len(funcs) == 0 {
		return -1
	}
	obj := info.Uses[id]
	var ft *ast.FuncType
	switch f := funcs[len(funcs)-1].(type) {
	case *ast.FuncDecl:
		ft = f.Type
	case *ast.FuncLit:
		ft = f.Type
	}
	i := 0
	for _, fld := range ft.Params.List {
		for _, n := range fld.Names {
			if info.Defs[n] == obj {
				return i
			}
			i++
		}
		if len(fld.Names) == 0 {
			i++
		}
	}
	return -1
}

// enclosing names the innermost function, and returns the object a call
// reaches it through: a declared function or method, or the variable a
// function literal is assigned to.
func (s *source) enclosing(funcs []ast.Node, info *types.Info, stack []ast.Node) (string, types.Object) {
	if len(funcs) == 0 {
		return "", nil
	}
	switch f := funcs[len(funcs)-1].(type) {
	case *ast.FuncDecl:
		return f.Name.Name, info.Defs[f.Name]
	case *ast.FuncLit:
		// Find the assignment the literal is the value of.
		for i := len(stack) - 1; i > 0; i-- {
			if stack[i] != f {
				continue
			}
			switch a := stack[i-1].(type) {
			case *ast.AssignStmt:
				for j, r := range a.Rhs {
					if r == f && j < len(a.Lhs) {
						if id, ok := a.Lhs[j].(*ast.Ident); ok {
							obj := info.Defs[id]
							if obj == nil {
								obj = info.Uses[id]
							}
							return outerName(funcs, info), obj
						}
					}
				}
			case *ast.ValueSpec:
				for j, r := range a.Values {
					if r == f && j < len(a.Names) {
						return outerName(funcs, info), info.Defs[a.Names[j]]
					}
				}
			}
			break
		}
		return outerName(funcs, info), nil
	}
	return "", nil
}

// outerName names the declared function a literal is inside.
func outerName(funcs []ast.Node, info *types.Info) string {
	for i := len(funcs) - 1; i >= 0; i-- {
		if fd, ok := funcs[i].(*ast.FuncDecl); ok {
			return fd.Name.Name
		}
	}
	return ""
}

// catalogCode reports whether s is "<module>.<name>" in lower snake case.
func catalogCode(s string) bool {
	mod, name, ok := strings.Cut(s, ".")
	return ok && snake(mod) && snake(name)
}

func snake(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}

// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Annotate Ref in Prog with C types by parsing gcc debug output.
// Conversion of debug output to Go types.

package main

import (
	"bytes"
	"debug/dwarf"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

var debugDefine = flag.Bool("debug-define", false, "print relevant #defines")
var debugGcc = flag.Bool("debug-gcc", false, "print gcc invocations")

var nameToC = map[string]string{
	"schar":         "signed char",
	"uchar":         "unsigned char",
	"ushort":        "unsigned short",
	"uint":          "unsigned int",
	"ulong":         "unsigned long",
	"longlong":      "long long",
	"ulonglong":     "unsigned long long",
	"complexfloat":  "float _Complex",
	"complexdouble": "double _Complex",
}

// cname returns the C name to use for C.s.
// The expansions are listed in nameToC and also
// struct_foo becomes "struct foo", and similarly for
// union and enum.
func cname(s string) string {
	if t, ok := nameToC[s]; ok {
		return t
	}

	if strings.HasPrefix(s, "struct_") {
		return "struct " + s[len("struct_"):]
	}
	if strings.HasPrefix(s, "union_") {
		return "union " + s[len("union_"):]
	}
	if strings.HasPrefix(s, "enum_") {
		return "enum " + s[len("enum_"):]
	}
	if strings.HasPrefix(s, "sizeof_") {
		return "sizeof(" + cname(s[len("sizeof_"):]) + ")"
	}
	return s
}

// DiscardCgoDirectives processes the import C preamble, and discards
// all #cgo CFLAGS and LDFLAGS directives, so they don't make their
// way into _cgo_export.h.
func (f *File) DiscardCgoDirectives() {
	linesIn := strings.Split(f.Preamble, "\n")
	linesOut := make([]string, 0, len(linesIn))
	for _, line := range linesIn {
		l := strings.TrimSpace(line)
		if len(l) < 5 || l[:4] != "#cgo" || !unicode.IsSpace(rune(l[4])) {
			linesOut = append(linesOut, line)
		} else {
			linesOut = append(linesOut, "")
		}
	}
	f.Preamble = strings.Join(linesOut, "\n")
}

// addToFlag appends args to flag. All flags are later written out onto the
// _cgo_flags file for the build system to use.
func (p *Package) addToFlag(flag string, args []string) {
	if flag == "CFLAGS" {
		// We'll need these when preprocessing for dwarf information.
		p.GccOptions = append(p.GccOptions, args...)
	}

	skip := false
	for i, arg := range args {
		// The go tool will pass us a -I option pointing to objdir;
		// we don't need to record that for later, as the objdir
		// will disappear anyhow.
		if skip {
			// Discard argument in "-I objdir" case.
			skip = false
		} else if strings.HasPrefix(arg, "-I") && strings.HasPrefix(arg[2:], *objDir) {
			// This is -Iobjdir. Don't save this argument.
		} else if arg == "-I" && i+1 < len(args) && strings.HasPrefix(args[i+1], *objDir) {
			// This is -I objdir. Don't save this argument
			// or the next one.
			skip = true
		} else {
			p.CgoFlags[flag] = append(p.CgoFlags[flag], arg)
		}
	}
}

// splitQuoted splits the string s around each instance of one or more consecutive
// white space characters while taking into account quotes and escaping, and
// returns an array of substrings of s or an empty list if s contains only white space.
// Single quotes and double quotes are recognized to prevent splitting within the
// quoted region, and are removed from the resulting substrings. If a quote in s
// isn't closed err will be set and r will have the unclosed argument as the
// last element. The backslash is used for escaping.
//
// For example, the following string:
//
//     `a b:"c d" 'e''f'  "g\""`
//
// Would be parsed as:
//
//     []string{"a", "b:c d", "ef", `g"`}
//
func splitQuoted(s string) (r []string, err error) {
	var args []string
	arg := make([]rune, len(s))
	escaped := false
	quoted := false
	quote := '\x00'
	i := 0
	for _, r := range s {
		switch {
		case escaped:
			escaped = false
		case r == '\\':
			escaped = true
			continue
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
		case r == '"' || r == '\'':
			quoted = true
			quote = r
			continue
		case unicode.IsSpace(r):
			if quoted || i > 0 {
				quoted = false
				args = append(args, string(arg[:i]))
				i = 0
			}
			continue
		}
		arg[i] = r
		i++
	}
	if quoted || i > 0 {
		args = append(args, string(arg[:i]))
	}
	if quote != 0 {
		err = errors.New("unclosed quote")
	} else if escaped {
		err = errors.New("unfinished escaping")
	}
	return args, err
}

// Translate rewrites f.AST, the original Go input, to remove
// references to the imported package C, replacing them with
// references to the equivalent Go types, functions, and variables.
func (p *Package) Translate(f *File) {
	for _, cref := range f.Ref {
		// Convert C.ulong to C.unsigned long, etc.
		cref.Name.C = cname(cref.Name.Go)
	}
	p.loadDefines(f)
	needType := p.guessKinds(f)
	if len(needType) > 0 {
		p.loadDWARF(f, needType)
	}
	if p.rewriteCalls(f) {
		// Add `import _cgo_unsafe "unsafe"` as the first decl
		// after the package statement.
		imp := &ast.GenDecl{
			Tok: token.IMPORT,
			Specs: []ast.Spec{
				&ast.ImportSpec{
					Name: ast.NewIdent("_cgo_unsafe"),
					Path: &ast.BasicLit{
						Kind:  token.STRING,
						Value: `"unsafe"`,
					},
				},
			},
		}
		f.AST.Decls = append([]ast.Decl{imp}, f.AST.Decls...)
	}
	p.rewriteRef(f)
}

// loadDefines coerces gcc into spitting out the #defines in use
// in the file f and saves relevant renamings in f.Name[name].Define.
func (p *Package) loadDefines(f *File) {
	var b bytes.Buffer
	b.WriteString(f.Preamble)
	b.WriteString(builtinProlog)
	stdout := p.gccDefines(b.Bytes())

	for _, line := range strings.Split(stdout, "\n") {
		if len(line) < 9 || line[0:7] != "#define" {
			continue
		}

		line = strings.TrimSpace(line[8:])

		var key, val string
		spaceIndex := strings.Index(line, " ")
		tabIndex := strings.Index(line, "\t")

		if spaceIndex == -1 && tabIndex == -1 {
			continue
		} else if tabIndex == -1 || (spaceIndex != -1 && spaceIndex < tabIndex) {
			key = line[0:spaceIndex]
			val = strings.TrimSpace(line[spaceIndex:])
		} else {
			key = line[0:tabIndex]
			val = strings.TrimSpace(line[tabIndex:])
		}

		if key == "__clang__" {
			p.GccIsClang = true
		}

		if n := f.Name[key]; n != nil {
			if *debugDefine {
				fmt.Fprintf(os.Stderr, "#define %s %s\n", key, val)
			}
			n.Define = val
		}
	}
}

// guessKinds tricks gcc into revealing the kind of each
// name xxx for the references C.xxx in the Go input.
// The kind is either a constant, type, or variable.
func (p *Package) guessKinds(f *File) []*Name {
	// Determine kinds for names we already know about,
	// like #defines or 'struct foo', before bothering with gcc.
	var names, needType []*Name
	for _, key := range nameKeys(f.Name) {
		n := f.Name[key]
		// If we've already found this name as a #define
		// and we can translate it as a constant value, do so.
		if n.Define != "" {
			if i, err := strconv.ParseInt(n.Define, 0, 64); err == nil {
				n.Kind = "iconst"
				// Turn decimal into hex, just for consistency
				// with enum-derived constants. Otherwise
				// in the cgo -godefs output half the constants
				// are in hex and half are in whatever the #define used.
				n.Const = fmt.Sprintf("%#x", i)
			} else if n.Define[0] == '\'' {
				if _, err := parser.ParseExpr(n.Define); err == nil {
					n.Kind = "iconst"
					n.Const = n.Define
				}
			} else if n.Define[0] == '"' {
				if _, err := parser.ParseExpr(n.Define); err == nil {
					n.Kind = "sconst"
					n.Const = n.Define
				}
			}

			if n.IsConst() {
				continue
			}

			if isName(n.Define) {
				n.C = n.Define
			}
		}

		// If this is a struct, union, or enum type name, no need to guess the kind.
		if strings.HasPrefix(n.C, "struct ") || strings.HasPrefix(n.C, "union ") || strings.HasPrefix(n.C, "enum ") {
			n.Kind = "type"
			needType = append(needType, n)
			continue
		}

		// Otherwise, we'll need to find out from gcc.
		names = append(names, n)
	}

	// Bypass gcc if there's nothing left to find out.
	if len(names) == 0 {
		return needType
	}

	// Coerce gcc into telling us whether each name is a type, a value, or undeclared.
	// For names, find out whether they are integer constants.
	// We used to look at specific warning or error messages here, but that tied the
	// behavior too closely to specific versions of the compilers.
	// Instead, arrange that we can infer what we need from only the presence or absence
	// of an error on a specific line.
	//
	// For each name, we generate these lines, where xxx is the index in toSniff plus one.
	//
	//	#line xxx "not-declared"
	//	void __cgo_f_xxx_1(void) { __typeof__(name) *__cgo_undefined__; }
	//	#line xxx "not-type"
	//	void __cgo_f_xxx_2(void) { name *__cgo_undefined__; }
	//	#line xxx "not-int-const"
	//	void __cgo_f_xxx_3(void) { enum { __cgo_undefined__ = (name)*1 }; }
	//	#line xxx "not-num-const"
	//	void __cgo_f_xxx_4(void) { static const double x = (name); }
	//	#line xxx "not-str-lit"
	//	void __cgo_f_xxx_5(void) { static const char x[] = (name); }
	//	#line xxx "not-signed-int-const"
	//	#if 0 < -(name)
	//	#line xxx "not-signed-int-const"
	//	#error found unsigned int
	//	#endif
	//
	// If we see an error at not-declared:xxx, the corresponding name is not declared.
	// If we see an error at not-type:xxx, the corresponding name is a type.
	// If we see an error at not-int-const:xxx, the corresponding name is not an integer constant.
	// If we see an error at not-num-const:xxx, the corresponding name is not a number constant.
	// If we see an error at not-str-lit:xxx, the corresponding name is not a string literal.
	// If we see an error at not-signed-int-const:xxx, the corresponding name is not a signed integer literal.
	//
	// The specific input forms are chosen so that they are valid C syntax regardless of
	// whether name denotes a type or an expression.

	var b bytes.Buffer
	b.WriteString(f.Preamble)
	b.WriteString(builtinProlog)

	for i, n := range names {
		fmt.Fprintf(&b, "#line %d \"not-declared\"\n"+
			"void __cgo_f_%d_1(void) { __typeof__(%s) *__cgo_undefined__; }\n"+
			"#line %d \"not-type\"\n"+
			"void __cgo_f_%d_2(void) { %s *__cgo_undefined__; }\n"+
			"#line %d \"not-int-const\"\n"+
			"void __cgo_f_%d_3(void) { enum { __cgo_undefined__ = (%s)*1 }; }\n"+
			"#line %d \"not-num-const\"\n"+
			"void __cgo_f_%d_4(void) { static const double x = (%s); }\n"+
			"#line %d \"not-str-lit\"\n"+
			"void __cgo_f_%d_5(void) { static const char s[] = (%s); }\n"+
			"#line %d \"not-signed-int-const\"\n"+
			"#if 0 < (%s)\n"+
			"#line %d \"not-signed-int-const\"\n"+
			"#error found unsigned int\n"+
			"#endif\n",
			i+1, i+1, n.C,
			i+1, i+1, n.C,
			i+1, i+1, n.C,
			i+1, i+1, n.C,
			i+1, i+1, n.C,
			i+1, n.C, i+1,
		)
	}
	fmt.Fprintf(&b, "#line 1 \"completed\"\n"+
		"int __cgo__1 = __cgo__2;\n")

	stderr := p.gccErrors(b.Bytes())
	if stderr == "" {
		fatalf("%s produced no output\non input:\n%s", p.gccBaseCmd()[0], b.Bytes())
	}

	completed := false
	sniff := make([]int, len(names))
	const (
		notType = 1 << iota
		notIntConst
		notNumConst
		notStrLiteral
		notDeclared
		notSignedIntConst
	)
	sawUnmatchedErrors := false
	for _, line := range strings.Split(stderr, "\n") {
		// Ignore warnings and random comments, with one
		// exception: newer GCC versions will sometimes emit
		// an error on a macro #define with a note referring
		// to where the expansion occurs. We care about where
		// the expansion occurs, so in that case treat the note
		// as an error.
		isError := strings.Contains(line, ": error:")
		isErrorNote := strings.Contains(line, ": note:") && sawUnmatchedErrors
		if !isError && !isErrorNote {
			continue
		}

		c1 := strings.Index(line, ":")
		if c1 < 0 {
			continue
		}
		c2 := strings.Index(line[c1+1:], ":")
		if c2 < 0 {
			continue
		}
		c2 += c1 + 1

		filename := line[:c1]
		i, _ := strconv.Atoi(line[c1+1 : c2])
		i--
		if i < 0 || i >= len(names) {
			if isError {
				sawUnmatchedErrors = true
			}
			continue
		}

		switch filename {
		case "completed":
			// Strictly speaking, there is no guarantee that seeing the error at completed:1
			// (at the end of the file) means we've seen all the errors from earlier in the file,
			// but usually it does. Certainly if we don't see the completed:1 error, we did
			// not get all the errors we expected.
			completed = true

		case "not-declared":
			sniff[i] |= notDeclared
		case "not-type":
			sniff[i] |= notType
		case "not-int-const":
			sniff[i] |= notIntConst
		case "not-num-const":
			sniff[i] |= notNumConst
		case "not-str-lit":
			sniff[i] |= notStrLiteral
		case "not-signed-int-const":
			sniff[i] |= notSignedIntConst
		default:
			if isError {
				sawUnmatchedErrors = true
			}
			continue
		}

		sawUnmatchedErrors = false
	}

	if !completed {
		fatalf("%s did not produce error at completed:1\non input:\n%s\nfull error output:\n%s", p.gccBaseCmd()[0], b.Bytes(), stderr)
	}

	for i, n := range names {
		switch sniff[i] &^ notSignedIntConst {
		default:
			var tpos token.Pos
			for _, ref := range f.Ref {
				if ref.Name == n {
					tpos = ref.Pos()
					break
				}
			}
			error_(tpos, "could not determine kind of name for C.%s", fixGo(n.Go))
		case notStrLiteral | notType:
			if sniff[i]&notSignedIntConst != 0 {
				n.Kind = "uconst"
			} else {
				n.Kind = "iconst"
			}
		case notIntConst | notStrLiteral | notType:
			n.Kind = "fconst"
		case notIntConst | notNumConst | notType:
			n.Kind = "sconst"
		case notIntConst | notNumConst | notStrLiteral:
			n.Kind = "type"
		case notIntConst | notNumConst | notStrLiteral | notType:
			n.Kind = "not-type"
		}
	}
	if nerrors > 0 {
		// Check if compiling the preamble by itself causes any errors,
		// because the messages we've printed out so far aren't helpful
		// to users debugging preamble mistakes. See issue 8442.
		preambleErrors := p.gccErrors([]byte(f.Preamble))
		if len(preambleErrors) > 0 {
			error_(token.NoPos, "\n%s errors for preamble:\n%s", p.gccBaseCmd()[0], preambleErrors)
		}

		fatalf("unresolved names")
	}

	needType = append(needType, names...)
	return needType
}

// loadDWARF parses the DWARF debug information generated
// by gcc to learn the details of the constants, variables, and types
// being referred to as C.xxx.
func (p *Package) loadDWARF(f *File, names []*Name) {
	// Extract the types from the DWARF section of an object
	// from a well-formed C program. Gcc only generates DWARF info
	// for symbols in the object file, so it is not enough to print the
	// preamble and hope the symbols we care about will be there.
	// Instead, emit
	//	__typeof__(names[i]) *__cgo__i;
	// for each entry in names and then dereference the type we
	// learn for __cgo__i.
	var b bytes.Buffer
	b.WriteString(f.Preamble)
	b.WriteString(builtinProlog)
	b.WriteString("#line 1 \"cgo-dwarf-inference\"\n")
	for i, n := range names {
		fmt.Fprintf(&b, "__typeof__(%s) *__cgo__%d;\n", n.C, i)
		if n.Kind == "iconst" || n.Kind == "uconst" {
			fmt.Fprintf(&b, "enum { __cgo_enum__%d = %s };\n", i, n.C)
		}
	}

	// We create a data block initialized with the values,
	// so we can read them out of the object file.
	fmt.Fprintf(&b, "long long __cgodebug_ints[] = {\n")
	for _, n := range names {
		if n.Kind == "iconst" || n.Kind == "uconst" {
			fmt.Fprintf(&b, "\t%s,\n", n.C)
		} else {
			fmt.Fprintf(&b, "\t0,\n")
		}
	}
	// for the last entry, we cannot use 0, otherwise
	// in case all __cgodebug_data is zero initialized,
	// LLVM-based gcc will place the it in the __DATA.__common
	// zero-filled section (our debug/macho doesn't support
	// this)
	fmt.Fprintf(&b, "\t1\n")
	fmt.Fprintf(&b, "};\n")

	// do the same work for floats.
	fmt.Fprintf(&b, "double __cgodebug_floats[] = {\n")
	for _, n := range names {
		if n.Kind == "fconst" {
			fmt.Fprintf(&b, "\t%s,\n", n.C)
		} else {
			fmt.Fprintf(&b, "\t0,\n")
		}
	}
	fmt.Fprintf(&b, "\t1\n")
	fmt.Fprintf(&b, "};\n")

	// do the same work for strings.
	for i, n := range names {
		if n.Kind == "sconst" {
			fmt.Fprintf(&b, "const char __cgodebug_str__%d[] = %s;\n", i, n.C)
			fmt.Fprintf(&b, "const unsigned long long __cgodebug_strlen__%d = sizeof(%s)-1;\n", i, n.C)
		}
	}

	d, ints, floats, strs := p.gccDebug(b.Bytes(), len(names))

	// Scan DWARF info for top-level TagVariable entries with AttrName __cgo__i.
	types := make([]dwarf.Type, len(names))
	nameToIndex := make(map[*Name]int)
	for i, n := range names {
		nameToIndex[n] = i
	}
	nameToRef := make(map[*Name]*Ref)
	for _, ref := range f.Ref {
		nameToRef[ref.Name] = ref
	}
	r := d.Reader()
	for {
		e, err := r.Next()
		if err != nil {
			fatalf("reading DWARF entry: %s", err)
		}
		if e == nil {
			break
		}
		switch e.Tag {
		case dwarf.TagVariable:
			name, _ := e.Val(dwarf.AttrName).(string)
			typOff, _ := e.Val(dwarf.AttrType).(dwarf.Offset)
			if name == "" || typOff == 0 {
				if e.Val(dwarf.AttrSpecification) != nil {
					// Since we are reading all the DWARF,
					// assume we will see the variable elsewhere.
					break
				}
				fatalf("malformed DWARF TagVariable entry")
			}
			if !strings.HasPrefix(name, "__cgo__") {
				break
			}
			typ, err := d.Type(typOff)
			if err != nil {
				fatalf("loading DWARF type: %s", err)
			}
			t, ok := typ.(*dwarf.PtrType)
			if !ok || t == nil {
				fatalf("internal error: %s has non-pointer type", name)
			}
			i, err := strconv.Atoi(name[7:])
			if err != nil {
				fatalf("malformed __cgo__ name: %s", name)
			}
			types[i] = t.Type
		}
		if e.Tag != dwarf.TagCompileUnit {
			r.SkipChildren()
		}
	}

	// Record types and typedef information.
	var conv typeConv
	conv.Init(p.PtrSize, p.IntSize)
	for i, n := range names {
		if types[i] == nil {
			continue
		}
		pos := token.NoPos
		if ref, ok := nameToRef[n]; ok {
			pos = ref.Pos()
		}
		f, fok := types[i].(*dwarf.FuncType)
		if n.Kind != "type" && fok {
			n.Kind = "func"
			n.FuncType = conv.FuncType(f, pos)
		} else {
			n.Type = conv.Type(types[i], pos)
			switch n.Kind {
			case "iconst":
				if i < len(ints) {
					n.Const = fmt.Sprintf("%#x", ints[i])
				}
			case "uconst":
				if i < len(ints) {
					n.Const = fmt.Sprintf("%#x", uint64(ints[i]))
				}
			case "fconst":
				if i < len(floats) {
					n.Const = fmt.Sprintf("%f", floats[i])
				}
			case "sconst":
				if i < len(strs) {
					n.Const = fmt.Sprintf("%q", strs[i])
				}
			}
		}
		conv.FinishType(pos)
	}
}

// mangleName does name mangling to translate names
// from the original Go source files to the names
// used in the final Go files generated by cgo.
func (p *Package) mangleName(n *Name) {
	// When using gccgo variables have to be
	// exported so that they become global symbols
	// that the C code can refer to.
	prefix := "_C"
	if *gccgo && n.IsVar() {
		prefix = "C"
	}
	n.Mangle = prefix + n.Kind + "_" + n.Go
}

// rewriteCalls rewrites all calls that pass pointers to check that
// they follow the rules for passing pointers between Go and C.
// This returns whether the package needs to import unsafe as _cgo_unsafe.
func (p *Package) rewriteCalls(f *File) bool {
	needsUnsafe := false
	for _, call := range f.Calls {
		// This is a call to C.xxx; set goname to "xxx".
		goname := call.Call.Fun.(*ast.SelectorExpr).Sel.Name
		if goname == "malloc" {
			continue
		}
		name := f.Name[goname]
		if name.Kind != "func" {
			// Probably a type conversion.
			continue
		}
		if p.rewriteCall(f, call, name) {
			needsUnsafe = true
		}
	}
	return needsUnsafe
}

// rewriteCall rewrites one call to add pointer checks.
// If any pointer checks are required, we rewrite the call into a
// function literal that calls _cgoCheckPointer for each pointer
// argument and then calls the original function.
// This returns whether the package needs to import unsafe as _cgo_unsafe.
func (p *Package) rewriteCall(f *File, call *Call, name *Name) bool {
	// Avoid a crash if the number of arguments is
	// less than the number of parameters.
	// This will be caught when the generated file is compiled.
	if len(call.Call.Args) < len(name.FuncType.Params) {
		return false
	}

	any := false
	for i, param := range name.FuncType.Params {
		if p.needsPointerCheck(f, param.Go, call.Call.Args[i]) {
			any = true
			break
		}
	}
	if !any {
		return false
	}

	// We need to rewrite this call.
	//
	// We are going to rewrite C.f(p) to
	//    func (_cgo0 ptype) {
	//            _cgoCheckPointer(_cgo0)
	//            C.f(_cgo0)
	//    }(p)
	// Using a function literal like this lets us do correct
	// argument type checking, and works correctly if the call is
	// deferred.
	needsUnsafe := false
	params := make([]*ast.Field, len(name.FuncType.Params))
	nargs := make([]ast.Expr, len(name.FuncType.Params))
	var stmts []ast.Stmt
	for i, param := range name.FuncType.Params {
		// params is going to become the parameters of the
		// function literal.
		// nargs is going to become the list of arguments made
		// by the call within the function literal.
		// nparam is the parameter of the function literal that
		// corresponds to param.

		origArg := call.Call.Args[i]
		nparam := ast.NewIdent(fmt.Sprintf("_cgo%d", i))
		nargs[i] = nparam

		// The Go version of the C type might use unsafe.Pointer,
		// but the file might not import unsafe.
		// Rewrite the Go type if necessary to use _cgo_unsafe.
		ptype := p.rewriteUnsafe(param.Go)
		if ptype != param.Go {
			needsUnsafe = true
		}

		params[i] = &ast.Field{
			Names: []*ast.Ident{nparam},
			Type:  ptype,
		}

		if !p.needsPointerCheck(f, param.Go, origArg) {
			continue
		}

		// Run the cgo pointer checks on nparam.

		// Change the function literal to call the real function
		// with the parameter passed through _cgoCheckPointer.
		c := &ast.CallExpr{
			Fun: ast.NewIdent("_cgoCheckPointer"),
			Args: []ast.Expr{
				nparam,
			},
		}

		// Add optional additional arguments for an address
		// expression.
		c.Args = p.checkAddrArgs(f, c.Args, origArg)

		stmt := &ast.ExprStmt{
			X: c,
		}
		stmts = append(stmts, stmt)
	}

	fcall := &ast.CallExpr{
		Fun:  call.Call.Fun,
		Args: nargs,
	}
	ftype := &ast.FuncType{
		Params: &ast.FieldList{
			List: params,
		},
	}
	if name.FuncType.Result != nil {
		rtype := p.rewriteUnsafe(name.FuncType.Result.Go)
		if rtype != name.FuncType.Result.Go {
			needsUnsafe = true
		}
		ftype.Results = &ast.FieldList{
			List: []*ast.Field{
				&ast.Field{
					Type: rtype,
				},
			},
		}
	}

	// There is a Ref pointing to the old call.Call.Fun.
	for _, ref := range f.Ref {
		if ref.Expr == &call.Call.Fun {
			ref.Expr = &fcall.Fun

			// If this call expects two results, we have to
			// adjust the results of the function we generated.
			if ref.Context == "call2" {
				if ftype.Results == nil {
					// An explicit void argument
					// looks odd but it seems to
					// be how cgo has worked historically.
					ftype.Results = &ast.FieldList{
						List: []*ast.Field{
							&ast.Field{
								Type: ast.NewIdent("_Ctype_void"),
							},
						},
					}
				}
				ftype.Results.List = append(ftype.Results.List,
					&ast.Field{
						Type: ast.NewIdent("error"),
					})
			}
		}
	}

	var fbody ast.Stmt
	if ftype.Results == nil {
		fbody = &ast.ExprStmt{
			X: fcall,
		}
	} else {
		fbody = &ast.ReturnStmt{
			Results: []ast.Expr{fcall},
		}
	}
	call.Call.Fun = &ast.FuncLit{
		Type: ftype,
		Body: &ast.BlockStmt{
			List: append(stmts, fbody),
		},
	}
	call.Call.Lparen = token.NoPos
	call.Call.Rparen = token.NoPos

	return needsUnsafe
}

// needsPointerCheck returns whether the type t needs a pointer check.
// This is true if t is a pointer and if the value to which it points
// might contain a pointer.
func (p *Package) needsPointerCheck(f *File, t ast.Expr, arg ast.Expr) bool {
	// An untyped nil does not need a pointer check, and when
	// _cgoCheckPointer returns the untyped nil the type assertion we
	// are going to insert will fail.  Easier to just skip nil arguments.
	// TODO: Note that this fails if nil is shadowed.
	if id, ok := arg.(*ast.Ident); ok && id.Name == "nil" {
		return false
	}

	return p.hasPointer(f, t, true)
}

// hasPointer is used by needsPointerCheck. If top is true it returns
// whether t is or contains a pointer that might point to a pointer.
// If top is false it returns whether t is or contains a pointer.
// f may be nil.
func (p *Package) hasPointer(f *File, t ast.Expr, top bool) bool {
	switch t := t.(type) {
	case *ast.ArrayType:
		if t.Len == nil {
			if !top {
				return true
			}
			return p.hasPointer(f, t.Elt, false)
		}
		return p.hasPointer(f, t.Elt, top)
	case *ast.StructType:
		for _, field := range t.Fields.List {
			if p.hasPointer(f, field.Type, top) {
				return true
			}
		}
		return false
	case *ast.StarExpr: // Pointer type.
		if !top {
			return true
		}
		// Check whether this is a pointer to a C union (or class)
		// type that contains a pointer.
		if unionWithPointer[t.X] {
			return true
		}
		return p.hasPointer(f, t.X, false)
	case *ast.FuncType, *ast.InterfaceType, *ast.MapType, *ast.ChanType:
		return true
	case *ast.Ident:
		// TODO: Handle types defined within function.
		for _, d := range p.Decl {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if ts.Name.Name == t.Name {
					return p.hasPointer(f, ts.Type, top)
				}
			}
		}
		if def := typedef[t.Name]; def != nil {
			return p.hasPointer(f, def.Go, top)
		}
		if t.Name == "string" {
			return !top
		}
		if t.Name == "error" {
			return true
		}
		if goTypes[t.Name] != nil {
			return false
		}
		// We can't figure out the type. Conservative
		// approach is to assume it has a pointer.
		return true
	case *ast.SelectorExpr:
		if l, ok := t.X.(*ast.Ident); !ok || l.Name != "C" {
			// Type defined in a different package.
			// Conservative approach is to assume it has a
			// pointer.
			return true
		}
		if f == nil {
			// Conservative approach: assume pointer.
			return true
		}
		name := f.Name[t.Sel.Name]
		if name != nil && name.Kind == "type" && name.Type != nil && name.Type.Go != nil {
			return p.hasPointer(f, name.Type.Go, top)
		}
		// We can't figure out the type. Conservative
		// approach is to assume it has a pointer.
		return true
	default:
		error_(t.Pos(), "could not understand type %s", gofmt(t))
		return true
	}
}

// checkAddrArgs tries to add arguments to the call of
// _cgoCheckPointer when the argument is an address expression. We
// pass true to mean that the argument is an address operation of
// something other than a slice index, which means that it's only
// necessary to check the specific element pointed to, not the entire
// object. This is for &s.f, where f is a field in a struct. We can
// pass a slice or array, meaning that we should check the entire
// slice or array but need not check any other part of the object.
// This is for &s.a[i], where we need to check all of a. However, we
// only pass the slice or array if we can refer to it without side
// effects.
func (p *Package) checkAddrArgs(f *File, args []ast.Expr, x ast.Expr) []ast.Expr {
	// Strip type conversions.
	for {
		c, ok := x.(*ast.CallExpr)
		if !ok || len(c.Args) != 1 || !p.isType(c.Fun) {
			break
		}
		x = c.Args[0]
	}
	u, ok := x.(*ast.UnaryExpr)
	if !ok || u.Op != token.AND {
		return args
	}
	index, ok := u.X.(*ast.IndexExpr)
	if !ok {
		// This is the address of something that is not an
		// index expression. We only need to examine the
		// single value to which it points.
		// TODO: what if true is shadowed?
		return append(args, ast.NewIdent("true"))
	}
	if !p.hasSideEffects(f, index.X) {
		// Examine the entire slice.
		return append(args, index.X)
	}
	// Treat the pointer as unknown.
	return args
}

// hasSideEffects returns whether the expression x has any side
// effects.  x is an expression, not a statement, so the only side
// effect is a function call.
func (p *Package) hasSideEffects(f *File, x ast.Expr) bool {
	found := false
	f.walk(x, "expr",
		func(f *File, x interface{}, context string) {
			switch x.(type) {
			case *ast.CallExpr:
				found = true
			}
		})
	return found
}

// isType returns whether the expression is definitely a type.
// This is conservative--it returns false for an unknown identifier.
func (p *Package) isType(t ast.Expr) bool {
	switch t := t.(type) {
	case *ast.SelectorExpr:
		id, ok := t.X.(*ast.Ident)
		if !ok {
			return false
		}
		if id.Name == "unsafe" && t.Sel.Name == "Pointer" {
			return true
		}
		if id.Name == "C" && typedef["_Ctype_"+t.Sel.Name] != nil {
			return true
		}
		return false
	case *ast.Ident:
		// TODO: This ignores shadowing.
		switch t.Name {
		case "unsafe.Pointer", "bool", "byte",
			"complex64", "complex128",
			"error",
			"float32", "float64",
			"int", "int8", "int16", "int32", "int64",
			"rune", "string",
			"uint", "uint8", "uint16", "uint32", "uint64", "uintptr":

			return true
		}
	case *ast.StarExpr:
		return p.isType(t.X)
	case *ast.ArrayType, *ast.StructType, *ast.FuncType, *ast.InterfaceType,
		*ast.MapType, *ast.ChanType:

		return true
	}
	return false
}

// rewriteUnsafe returns a version of t with references to unsafe.Pointer
// rewritten to use _cgo_unsafe.Pointer instead.
func (p *Package) rewriteUnsafe(t ast.Expr) ast.Expr {
	switch t := t.(type) {
	case *ast.Ident:
		// We don't see a SelectorExpr for unsafe.Pointer;
		// this is created by code in this file.
		if t.Name == "unsafe.Pointer" {
			return ast.NewIdent("_cgo_unsafe.Pointer")
		}
	case *ast.ArrayType:
		t1 := p.rewriteUnsafe(t.Elt)
		if t1 != t.Elt {
			r := *t
			r.Elt = t1
			return &r
		}
	case *ast.StructType:
		changed := false
		fields := *t.Fields
		fields.List = nil
		for _, f := range t.Fields.List {
			ft := p.rewriteUnsafe(f.Type)
			if ft == f.Type {
				fields.List = append(fields.List, f)
			} else {
				fn := *f
				fn.Type = ft
				fields.List = append(fields.List, &fn)
				changed = true
			}
		}
		if changed {
			r := *t
			r.Fields = &fields
			return &r
		}
	case *ast.StarExpr: // Pointer type.
		x1 := p.rewriteUnsafe(t.X)
		if x1 != t.X {
			r := *t
			r.X = x1
			return &r
		}
	}
	return t
}

// rewriteRef rewrites all the C.xxx references in f.AST to refer to the
// Go equivalents, now that we have figured out the meaning of all
// the xxx. In *godefs mode, rewriteRef replaces the names
// with full definitions instead of mangled names.
func (p *Package) rewriteRef(f *File) {
	// Keep a list of all the functions, to remove the ones
	// only used as expressions and avoid generating bridge
	// code for them.
	functions := make(map[string]bool)

	// Assign mangled names.
	for _, n := range f.Name {
		if n.Kind == "not-type" {
			n.Kind = "var"
		}
		if n.Mangle == "" {
			p.mangleName(n)
		}
		if n.Kind == "func" {
			functions[n.Go] = false
		}
	}

	// Now that we have all the name types filled in,
	// scan through the Refs to identify the ones that
	// are trying to do a ,err call. Also check that
	// functions are only used in calls.
	for _, r := range f.Ref {
		if r.Name.IsConst() && r.Name.Const == "" {
			error_(r.Pos(), "unable to find value of constant C.%s", fixGo(r.Name.Go))
		}
		var expr ast.Expr = ast.NewIdent(r.Name.Mangle) // default
		switch r.Context {
		case "call", "call2":
			if r.Name.Kind != "func" {
				if r.Name.Kind == "type" {
					r.Context = "type"
					if r.Name.Type == nil {
						error_(r.Pos(), "invalid conversion to C.%s: undefined C type '%s'", fixGo(r.Name.Go), r.Name.C)
						break
					}
					expr = r.Name.Type.Go
					break
				}
				error_(r.Pos(), "call of non-function C.%s", fixGo(r.Name.Go))
				break
			}
			functions[r.Name.Go] = true
			if r.Context == "call2" {
				if r.Name.Go == "_CMalloc" {
					error_(r.Pos(), "no two-result form for C.malloc")
					break
				}
				// Invent new Name for the two-result function.
				n := f.Name["2"+r.Name.Go]
				if n == nil {
					n = new(Name)
					*n = *r.Name
					n.AddError = true
					n.Mangle = "_C2func_" + n.Go
					f.Name["2"+r.Name.Go] = n
				}
				expr = ast.NewIdent(n.Mangle)
				r.Name = n
				break
			}
		case "expr":
			if r.Name.Kind == "func" {
				if builtinDefs[r.Name.C] != "" {
					error_(r.Pos(), "use of builtin '%s' not in function call", fixGo(r.Name.C))
				}

				// Function is being used in an expression, to e.g. pass around a C function pointer.
				// Create a new Name for this Ref which causes the variable to be declared in Go land.
				fpName := "fp_" + r.Name.Go
				name := f.Name[fpName]
				if name == nil {
					name = &Name{
						Go:   fpName,
						C:    r.Name.C,
						Kind: "fpvar",
						Type: &Type{Size: p.PtrSize, Align: p.PtrSize, C: c("void*"), Go: ast.NewIdent("unsafe.Pointer")},
					}
					p.mangleName(name)
					f.Name[fpName] = name
				}
				r.Name = name
				// Rewrite into call to _Cgo_ptr to prevent assignments. The _Cgo_ptr
				// function is defined in out.go and simply returns its argument. See
				// issue 7757.
				expr = &ast.CallExpr{
					Fun:  &ast.Ident{NamePos: (*r.Expr).Pos(), Name: "_Cgo_ptr"},
					Args: []ast.Expr{ast.NewIdent(name.Mangle)},
				}
			} else if r.Name.Kind == "type" {
				// Okay - might be new(T)
				if r.Name.Type == nil {
					error_(r.Pos(), "expression C.%s: undefined C type '%s'", fixGo(r.Name.Go), r.Name.C)
					break
				}
				expr = r.Name.Type.Go
			} else if r.Name.Kind == "var" {
				expr = &ast.StarExpr{Star: (*r.Expr).Pos(), X: expr}
			}

		case "selector":
			if r.Name.Kind == "var" {
				expr = &ast.StarExpr{Star: (*r.Expr).Pos(), X: expr}
			} else {
				error_(r.Pos(), "only C variables allowed in selector expression %s", fixGo(r.Name.Go))
			}

		case "type":
			if r.Name.Kind != "type" {
				error_(r.Pos(), "expression C.%s used as type", fixGo(r.Name.Go))
			} else if r.Name.Type == nil {
				// Use of C.enum_x, C.struct_x or C.union_x without C definition.
				// GCC won't raise an error when using pointers to such unknown types.
				error_(r.Pos(), "type C.%s: undefined C type '%s'", fixGo(r.Name.Go), r.Name.C)
			} else {
				expr = r.Name.Type.Go
			}
		default:
			if r.Name.Kind == "func" {
				error_(r.Pos(), "must call C.%s", fixGo(r.Name.Go))
			}
		}
		if *godefs {
			// Substitute definition for mangled type name.
			if id, ok := expr.(*ast.Ident); ok {
				if t := typedef[id.Name]; t != nil {
					expr = t.Go
				}
				if id.Name == r.Name.Mangle && r.Name.Const != "" {
					expr = ast.NewIdent(r.Name.Const)
				}
			}
		}

		// Copy position information from old expr into new expr,
		// in case expression being replaced is first on line.
		// See golang.org/issue/6563.
		pos := (*r.Expr).Pos()
		switch x := expr.(type) {
		case *ast.Ident:
			expr = &ast.Ident{NamePos: pos, Name: x.Name}
		}

		*r.Expr = expr
	}

	// Remove functions only used as expressions, so their respective
	// bridge functions are not generated.
	for name, used := range functions {
		if !used {
			delete(f.Name, name)
		}
	}
}

// gccBaseCmd returns the start of the compiler command line.
// It uses $CC if set, or else $GCC, or else the compiler recorded
// during the initial build as defaultCC.
// defaultCC is defined in zdefaultcc.go, written by cmd/dist.
func (p *Package) gccBaseCmd() []string {
	// Use $CC if set, since that's what the build uses.
	if ret := strings.Fields(os.Getenv("CC")); len(ret) > 0 {
		return ret
	}
	// Try $GCC if set, since that's what we used to use.
	if ret := strings.Fields(os.Getenv("GCC")); len(ret) > 0 {
		return ret
	}
	return strings.Fields(defaultCC)
}

// gccMachine returns the gcc -m flag to use, either "-m32", "-m64" or "-marm".
func (p *Package) gccMachine() []string {
	switch goarch {
	case "amd64":
		return []string{"-m64"}
	case "386":
		return []string{"-m32"}
	case "arm":
		return []string{"-marm"} // not thumb
	case "s390":
		return []string{"-m31"}
	case "s390x":
		return []string{"-m64"}
	case "mips64", "mips64le":
		return []string{"-mabi=64"}
	case "mips", "mipsle":
		return []string{"-mabi=32"}
	}
	return nil
}

func gccTmp() string {
	return *objDir + "_cgo_.o"
}

// gccCmd returns the gcc command line to use for compiling
// the input.
func (p *Package) gccCmd() []string {
	c := append(p.gccBaseCmd(),
		"-w",          // no warnings
		"-Wno-error",  // warnings are not errors
		"-o"+gccTmp(), // write object to tmp
		"-gdwarf-2",   // generate DWARF v2 debugging symbols
		"-c",          // do not link
		"-xc",         // input language is C
	)
	if p.GccIsClang {
		c = append(c,
			"-ferror-limit=0",
			// Apple clang version 1.7 (tags/Apple/clang-77) (based on LLVM 2.9svn)
			// doesn't have -Wno-unneeded-internal-declaration, so we need yet another
			// flag to disable the warning. Yes, really good diagnostics, clang.
			"-Wno-unknown-warning-option",
			"-Wno-unneeded-internal-declaration",
			"-Wno-unused-function",
			"-Qunused-arguments",
			// Clang embeds prototypes for some builtin functions,
			// like malloc and calloc, but all size_t parameters are
			// incorrectly typed unsigned long. We work around that
			// by disabling the builtin functions (this is safe as
			// it won't affect the actual compilation of the C code).
			// See: https://golang.org/issue/6506.
			"-fno-builtin",
		)
	}

	c = append(c, p.GccOptions...)
	c = append(c, p.gccMachine()...)
	c = append(c, "-") //read input from standard input
	return c
}

// gccDebug runs gcc -gdwarf-2 over the C program stdin and
// returns the corresponding DWARF data and, if present, debug data block.
func (p *Package) gccDebug(stdin []byte, nnames int) (d *dwarf.Data, ints []int64, floats []float64, strs []string) {
	runGcc(stdin, p.gccCmd())

	isDebugInts := func(s string) bool {
		// Some systems use leading _ to denote non-assembly symbols.
		return s == "__cgodebug_ints" || s == "___cgodebug_ints"
	}
	isDebugFloats := func(s string) bool {
		// Some systems use leading _ to denote non-assembly symbols.
		return s == "__cgodebug_floats" || s == "___cgodebug_floats"
	}
	indexOfDebugStr := func(s string) int {
		// Some systems use leading _ to denote non-assembly symbols.
		if strings.HasPrefix(s, "___") {
			s = s[1:]
		}
		if strings.HasPrefix(s, "__cgodebug_str__") {
			if n, err := strconv.Atoi(s[len("__cgodebug_str__"):]); err == nil {
				return n
			}
		}
		return -1
	}
	indexOfDebugStrlen := func(s string) int {
		// Some systems use leading _ to denote non-assembly symbols.
		if strings.HasPrefix(s, "___") {
			s = s[1:]
		}
		if strings.HasPrefix(s, "__cgodebug_strlen__") {
			if n, err := strconv.Atoi(s[len("__cgodebug_strlen__"):]); err == nil {
				return n
			}
		}
		return -1
	}

	strs = make([]string, nnames)

	strdata := make(map[int]string, nnames)
	strlens := make(map[int]int, nnames)

	buildStrings := func() {
		for n, strlen := range strlens {
			data := strdata[n]
			if len(data) <= strlen {
				fatalf("invalid string literal")
			}
			strs[n] = string(data[:strlen])
		}
	}

	if f, err := macho.Open(gccTmp()); err == nil {
		defer f.Close()
		d, err := f.DWARF()
		if err != nil {
			fatalf("cannot load DWARF output from %s: %v", gccTmp(), err)
		}
		bo := f.ByteOrder
		if f.Symtab != nil {
			for i := range f.Symtab.Syms {
				s := &f.Symtab.Syms[i]
				switch {
				case isDebugInts(s.Name):
					// Found it. Now find data section.
					if i := int(s.Sect) - 1; 0 <= i && i < len(f.Sections) {
						sect := f.Sections[i]
						if sect.Addr <= s.Value && s.Value < sect.Addr+sect.Size {
							if sdat, err := sect.Data(); err == nil {
								data := sdat[s.Value-sect.Addr:]
								ints = make([]int64, len(data)/8)
								for i := range ints {
									ints[i] = int64(bo.Uint64(data[i*8:]))
								}
							}
						}
					}
				case isDebugFloats(s.Name):
					// Found it. Now find data section.
					if i := int(s.Sect) - 1; 0 <= i && i < len(f.Sections) {
						sect := f.Sections[i]
						if sect.Addr <= s.Value && s.Value < sect.Addr+sect.Size {
							if sdat, err := sect.Data(); err == nil {
								data := sdat[s.Value-sect.Addr:]
								floats = make([]float64, len(data)/8)
								for i := range floats {
									floats[i] = math.Float64frombits(bo.Uint64(data[i*8:]))
								}
							}
						}
					}
				default:
					if n := indexOfDebugStr(s.Name); n != -1 {
						// Found it. Now find data section.
						if i := int(s.Sect) - 1; 0 <= i && i < len(f.Sections) {
							sect := f.Sections[i]
							if sect.Addr <= s.Value && s.Value < sect.Addr+sect.Size {
								if sdat, err := sect.Data(); err == nil {
									data := sdat[s.Value-sect.Addr:]
									strdata[n] = string(data)
								}
							}
						}
						break
					}
					if n := indexOfDebugStrlen(s.Name); n != -1 {
						// Found it. Now find data section.
						if i := int(s.Sect) - 1; 0 <= i && i < len(f.Sections) {
							sect := f.Sections[i]
							if sect.Addr <= s.Value && s.Value < sect.Addr+sect.Size {
								if sdat, err := sect.Data(); err == nil {
									data := sdat[s.Value-sect.Addr:]
									strlen := bo.Uint64(data[:8])
									if strlen > (1<<(uint(p.IntSize*8)-1) - 1) { // greater than MaxInt?
										fatalf("string literal too big")
									}
									strlens[n] = int(strlen)
								}
							}
						}
						break
					}
				}
			}

			buildStrings()
		}
		return d, ints, floats, strs
	}

	if f, err := elf.Open(gccTmp()); err == nil {
		defer f.Close()
		d, err := f.DWARF()
		if err != nil {
			fatalf("cannot load DWARF output from %s: %v", gccTmp(), err)
		}
		bo := f.ByteOrder
		symtab, err := f.Symbols()
		if err == nil {
			for i := range symtab {
				s := &symtab[i]
				switch {
				case isDebugInts(s.Name):
					// Found it. Now find data section.
					if i := int(s.Section); 0 <= i && i < len(f.Sections) {
						sect := f.Sections[i]
						if sect.Addr <= s.Value && s.Value < sect.Addr+sect.Size {
							if sdat, err := sect.Data(); err == nil {
								data := sdat[s.Value-sect.Addr:]
								ints = make([]int64, len(data)/8)
								for i := range ints {
									ints[i] = int64(bo.Uint64(data[i*8:]))
								}
							}
						}
					}
				case isDebugFloats(s.Name):
					// Found it. Now find data section.
					if i := int(s.Section); 0 <= i && i < len(f.Sections) {
						sect := f.Sections[i]
						if sect.Addr <= s.Value && s.Value < sect.Addr+sect.Size {
							if sdat, err := sect.Data(); err == nil {
								data := sdat[s.Value-sect.Addr:]
								floats = make([]float64, len(data)/8)
								for i := range floats {
									floats[i] = math.Float64frombits(bo.Uint64(data[i*8:]))
								}
							}
						}
					}
				default:
					if n := indexOfDebugStr(s.Name); n != -1 {
						// Found it. Now find data section.
						if i := int(s.Section); 0 <= i && i < len(f.Sections) {
							sect := f.Sections[i]
							if sect.Addr <= s.Value && s.Value < sect.Addr+sect.Size {
								if sdat, err := sect.Data(); err == nil {
									data := sdat[s.Value-sect.Addr:]
									strdata[n] = string(data)
								}
							}
						}
						break
					}
					if n := indexOfDebugStrlen(s.Name); n != -1 {
						// Found it. Now find data section.
						if i := int(s.Section); 0 <= i && i < len(f.Sections) {
							sect := f.Sections[i]
							if sect.Addr <= s.Value && s.Value < sect.Addr+sect.Size {
								if sdat, err := sect.Data(); err == nil {
									data := sdat[s.Value-sect.Addr:]
									strlen := bo.Uint64(data[:8])
									if strlen > (1<<(uint(p.IntSize*8)-1) - 1) { // greater than MaxInt?
										fatalf("string literal too big")
									}
									strlens[n] = int(strlen)
								}
							}
						}
						break
					}
				}
			}

			buildStrings()
		}
		return d, ints, floats, strs
	}

	if f, err := pe.Open(gccTmp()); err == nil {
		defer f.Close()
		d, err := f.DWARF()
		if err != nil {
			fatalf("cannot load DWARF output from %s: %v", gccTmp(), err)
		}
		bo := binary.LittleEndian
		for _, s := range f.Symbols {
			switch {
			case isDebugInts(s.Name):
				if i := int(s.SectionNumber) - 1; 0 <= i && i < len(f.Sections) {
					sect := f.Sections[i]
					if s.Value < sect.Size {
						if sdat, err := sect.Data(); err == nil {
							data := sdat[s.Value:]
							ints = make([]int64, len(data)/8)
							for i := range ints {
								ints[i] = int64(bo.Uint64(data[i*8:]))
							}
						}
					}
				}
			case isDebugFloats(s.Name):
				if i := int(s.SectionNumber) - 1; 0 <= i && i < len(f.Sections) {
					sect := f.Sections[i]
					if s.Value < sect.Size {
						if sdat, err := sect.Data(); err == nil {
							data := sdat[s.Value:]
							floats = make([]float64, len(data)/8)
							for i := range floats {
								floats[i] = math.Float64frombits(bo.Uint64(data[i*8:]))
							}
						}
					}
				}
			default:
				if n := indexOfDebugStr(s.Name); n != -1 {
					if i := int(s.SectionNumber) - 1; 0 <= i && i < len(f.Sections) {
						sect := f.Sections[i]
						if s.Value < sect.Size {
							if sdat, err := sect.Data(); err == nil {
								data := sdat[s.Value:]
								strdata[n] = string(data)
							}
						}
					}
					break
				}
				if n := indexOfDebugStrlen(s.Name); n != -1 {
					if i := int(s.SectionNumber) - 1; 0 <= i && i < len(f.Sections) {
						sect := f.Sections[i]
						if s.Value < sect.Size {
							if sdat, err := sect.Data(); err == nil {
								data := sdat[s.Value:]
								strlen := bo.Uint64(data[:8])
								if strlen > (1<<(uint(p.IntSize*8)-1) - 1) { // greater than MaxInt?
									fatalf("string literal too big")
								}
								strlens[n] = int(strlen)
							}
						}
					}
					break
				}
			}
		}

		buildStrings()

		return d, ints, floats, strs
	}

	fatalf("cannot parse gcc output %s as ELF, Mach-O, PE object", gccTmp())
	panic("not reached")
}

// gccDefines runs gcc -E -dM -xc - over the C program stdin
// and returns the corresponding standard output, which is the
// #defines that gcc encountered while processing the input
// and its included files.
func (p *Package) gccDefines(stdin []byte) string {
	base := append(p.gccBaseCmd(), "-E", "-dM", "-xc")
	base = append(base, p.gccMachine()...)
	stdout, _ := runGcc(stdin, append(append(base, p.GccOptions...), "-"))
	return stdout
}

// gccErrors runs gcc over the C program stdin and returns
// the errors that gcc prints. That is, this function expects
// gcc to fail.
func (p *Package) gccErrors(stdin []byte) string {
	// TODO(rsc): require failure
	args := p.gccCmd()

	// Optimization options can confuse the error messages; remove them.
	nargs := make([]string, 0, len(args))
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-O") {
			nargs = append(nargs, arg)
		}
	}

	if *debugGcc {
		fmt.Fprintf(os.Stderr, "$ %s <<EOF\n", strings.Join(nargs, " "))
		os.Stderr.Write(stdin)
		fmt.Fprint(os.Stderr, "EOF\n")
	}
	stdout, stderr, _ := run(stdin, nargs)
	if *debugGcc {
		os.Stderr.Write(stdout)
		os.Stderr.Write(stderr)
	}
	return string(stderr)
}

// runGcc runs the gcc command line args with stdin on standard input.
// If the command exits with a non-zero exit status, runGcc prints
// details about what was run and exits.
// Otherwise runGcc returns the data written to standard output and standard error.
// Note that for some of the uses we expect useful data back
// on standard error, but for those uses gcc must still exit 0.
func runGcc(stdin []byte, args []string) (string, string) {
	if *debugGcc {
		fmt.Fprintf(os.Stderr, "$ %s <<EOF\n", strings.Join(args, " "))
		os.Stderr.Write(stdin)
		fmt.Fprint(os.Stderr, "EOF\n")
	}
	stdout, stderr, ok := run(stdin, args)
	if *debugGcc {
		os.Stderr.Write(stdout)
		os.Stderr.Write(stderr)
	}
	if !ok {
		os.Stderr.Write(stderr)
		os.Exit(2)
	}
	return string(stdout), string(stderr)
}

// A typeConv is a translator from dwarf types to Go types
// with equivalent memory layout.
type typeConv struct {
	// Cache of already-translated or in-progress types.
	m map[dwarf.Type]*Type

	// Map from types to incomplete pointers to those types.
	ptrs map[dwarf.Type][]*Type
	// Keys of ptrs in insertion order (deterministic worklist)
	ptrKeys []dwarf.Type

	// Predeclared types.
	bool                                   ast.Expr
	byte                                   ast.Expr // denotes padding
	int8, int16, int32, int64              ast.Expr
	uint8, uint16, uint32, uint64, uintptr ast.Expr
	float32, float64                       ast.Expr
	complex64, complex128                  ast.Expr
	void                                   ast.Expr
	string                                 ast.Expr
	goVoid                                 ast.Expr // _Ctype_void, denotes C's void
	goVoidPtr                              ast.Expr // unsafe.Pointer or *byte

	ptrSize int64
	intSize int64
}

var tagGen int
var typedef = make(map[string]*Type)
var goIdent = make(map[string]*ast.Ident)

// unionWithPointer is true for a Go type that represents a C union (or class)
// that may contain a pointer. This is used for cgo pointer checking.
var unionWithPointer = make(map[ast.Expr]bool)

func (c *typeConv) Init(ptrSize, intSize int64) {
	c.ptrSize = ptrSize
	c.intSize = intSize
	c.m = make(map[dwarf.Type]*Type)
	c.ptrs = make(map[dwarf.Type][]*Type)
	c.bool = c.Ident("bool")
	c.byte = c.Ident("byte")
	c.int8 = c.Ident("int8")
	c.int16 = c.Ident("int16")
	c.int32 = c.Ident("int32")
	c.int64 = c.Ident("int64")
	c.uint8 = c.Ident("uint8")
	c.uint16 = c.Ident("uint16")
	c.uint32 = c.Ident("uint32")
	c.uint64 = c.Ident("uint64")
	c.uintptr = c.Ident("uintptr")
	c.float32 = c.Ident("float32")
	c.float64 = c.Ident("float64")
	c.complex64 = c.Ident("complex64")
	c.complex128 = c.Ident("complex128")
	c.void = c.Ident("void")
	c.string = c.Ident("string")
	c.goVoid = c.Ident("_Ctype_void")

	// Normally cgo translates void* to unsafe.Pointer,
	// but for historical reasons -godefs uses *byte instead.
	if *godefs {
		c.goVoidPtr = &ast.StarExpr{X: c.byte}
	} else {
		c.goVoidPtr = c.Ident("unsafe.Pointer")
	}
}

// base strips away qualifiers and typedefs to get the underlying type
func base(dt dwarf.Type) dwarf.Type {
	for {
		if d, ok := dt.(*dwarf.QualType); ok {
			dt = d.Type
			continue
		}
		if d, ok := dt.(*dwarf.TypedefType); ok {
			dt = d.Type
			continue
		}
		break
	}
	return dt
}

// unqual strips away qualifiers from a DWARF type.
// In general we don't care about top-level qualifiers.
func unqual(dt dwarf.Type) dwarf.Type {
	for {
		if d, ok := dt.(*dwarf.QualType); ok {
			dt = d.Type
		} else {
			break
		}
	}
	return dt
}

// Map from dwarf text names to aliases we use in package "C".
var dwarfToName = map[string]string{
	"long int":               "long",
	"long unsigned int":      "ulong",
	"unsigned int":           "uint",
	"short unsigned int":     "ushort",
	"unsigned short":         "ushort", // Used by Clang; issue 13129.
	"short int":              "short",
	"long long int":          "longlong",
	"long long unsigned int": "ulonglong",
	"signed char":            "schar",
	"unsigned char":          "uchar",
}

const signedDelta = 64

// String returns the current type representation. Format arguments
// are assembled within this method so that any changes in mutable
// values are taken into account.
func (tr *TypeRepr) String() string {
	if len(tr.Repr) == 0 {
		return ""
	}
	if len(tr.FormatArgs) == 0 {
		return tr.Repr
	}
	return fmt.Sprintf(tr.Repr, tr.FormatArgs...)
}

// Empty reports whether the result of String would be "".
func (tr *TypeRepr) Empty() bool {
	return len(tr.Repr) == 0
}

// Set modifies the type representation.
// If fargs are provided, repr is used as a format for fmt.Sprintf.
// Otherwise, repr is used unprocessed as the type representation.
func (tr *TypeRepr) Set(repr string, fargs ...interface{}) {
	tr.Repr = repr
	tr.FormatArgs = fargs
}

// FinishType completes any outstanding type mapping work.
// In particular, it resolves incomplete pointer types.
func (c *typeConv) FinishType(pos token.Pos) {
	// Completing one pointer type might produce more to complete.
	// Keep looping until they're all done.
	for len(c.ptrKeys) > 0 {
		dtype := c.ptrKeys[0]
		c.ptrKeys = c.ptrKeys[1:]

		// Note Type might invalidate c.ptrs[dtype].
		t := c.Type(dtype, pos)
		for _, ptr := range c.ptrs[dtype] {
			ptr.Go.(*ast.StarExpr).X = t.Go
			ptr.C.Set("%s*", t.C)
		}
		c.ptrs[dtype] = nil // retain the map key
	}
}

// Type returns a *Type with the same memory layout as
// dtype when used as the type of a variable or a struct field.
func (c *typeConv) Type(dtype dwarf.Type, pos token.Pos) *Type {
	if t, ok := c.m[dtype]; ok {
		if t.Go == nil {
			fatalf("%s: type conversion loop at %s", lineno(pos), dtype)
		}
		return t
	}

	t := new(Type)
	t.Size = dtype.Size() // note: wrong for array of pointers, corrected below
	t.Align = -1
	t.C = &TypeRepr{Repr: dtype.Common().Name}
	c.m[dtype] = t

	switch dt := dtype.(type) {
	default:
		fatalf("%s: unexpected type: %s", lineno(pos), dtype)

	case *dwarf.AddrType:
		if t.Size != c.ptrSize {
			fatalf("%s: unexpected: %d-byte address type - %s", lineno(pos), t.Size, dtype)
		}
		t.Go = c.uintptr
		t.Align = t.Size

	case *dwarf.ArrayType:
		if dt.StrideBitSize > 0 {
			// Cannot represent bit-sized elements in Go.
			t.Go = c.Opaque(t.Size)
			break
		}
		count := dt.Count
		if count == -1 {
			// Indicates flexible array member, which Go doesn't support.
			// Translate to zero-length array instead.
			count = 0
		}
		sub := c.Type(dt.Type, pos)
		t.Align = sub.Align
		t.Go = &ast.ArrayType{
			Len: c.intExpr(count),
			Elt: sub.Go,
		}
		// Recalculate t.Size now that we know sub.Size.
		t.Size = count * sub.Size
		t.C.Set("__typeof__(%s[%d])", sub.C, dt.Count)

	case *dwarf.BoolType:
		t.Go = c.bool
		t.Align = 1

	case *dwarf.CharType:
		if t.Size != 1 {
			fatalf("%s: unexpected: %d-byte char type - %s", lineno(pos), t.Size, dtype)
		}
		t.Go = c.int8
		t.Align = 1

	case *dwarf.EnumType:
		if t.Align = t.Size; t.Align >= c.ptrSize {
			t.Align = c.ptrSize
		}
		t.C.Set("enum " + dt.EnumName)
		signed := 0
		t.EnumValues = make(map[string]int64)
		for _, ev := range dt.Val {
			t.EnumValues[ev.Name] = ev.Val
			if ev.Val < 0 {
				signed = signedDelta
			}
		}
		switch t.Size + int64(signed) {
		default:
			fatalf("%s: unexpected: %d-byte enum type - %s", lineno(pos), t.Size, dtype)
		case 1:
			t.Go = c.uint8
		case 2:
			t.Go = c.uint16
		case 4:
			t.Go = c.uint32
		case 8:
			t.Go = c.uint64
		case 1 + signedDelta:
			t.Go = c.int8
		case 2 + signedDelta:
			t.Go = c.int16
		case 4 + signedDelta:
			t.Go = c.int32
		case 8 + signedDelta:
			t.Go = c.int64
		}

	case *dwarf.FloatType:
		switch t.Size {
		default:
			fatalf("%s: unexpected: %d-byte float type - %s", lineno(pos), t.Size, dtype)
		case 4:
			t.Go = c.float32
		case 8:
			t.Go = c.float64
		}
		if t.Align = t.Size; t.Align >= c.ptrSize {
			t.Align = c.ptrSize
		}

	case *dwarf.ComplexType:
		switch t.Size {
		default:
			fatalf("%s: unexpected: %d-byte complex type - %s", lineno(pos), t.Size, dtype)
		case 8:
			t.Go = c.complex64
		case 16:
			t.Go = c.complex128
		}
		if t.Align = t.Size / 2; t.Align >= c.ptrSize {
			t.Align = c.ptrSize
		}

	case *dwarf.FuncType:
		// No attempt at translation: would enable calls
		// directly between worlds, but we need to moderate those.
		t.Go = c.uintptr
		t.Align = c.ptrSize

	case *dwarf.IntType:
		if dt.BitSize > 0 {
			fatalf("%s: unexpected: %d-bit int type - %s", lineno(pos), dt.BitSize, dtype)
		}
		switch t.Size {
		default:
			fatalf("%s: unexpected: %d-byte int type - %s", lineno(pos), t.Size, dtype)
		case 1:
			t.Go = c.int8
		case 2:
			t.Go = c.int16
		case 4:
			t.Go = c.int32
		case 8:
			t.Go = c.int64
		case 16:
			t.Go = &ast.ArrayType{
				Len: c.intExpr(t.Size),
				Elt: c.uint8,
			}
		}
		if t.Align = t.Size; t.Align >= c.ptrSize {
			t.Align = c.ptrSize
		}

	case *dwarf.PtrType:
		// Clang doesn't emit DW_AT_byte_size for pointer types.
		if t.Size != c.ptrSize && t.Size != -1 {
			fatalf("%s: unexpected: %d-byte pointer type - %s", lineno(pos), t.Size, dtype)
		}
		t.Size = c.ptrSize
		t.Align = c.ptrSize

		if _, ok := base(dt.Type).(*dwarf.VoidType); ok {
			t.Go = c.goVoidPtr
			t.C.Set("void*")
			dq := dt.Type
			for {
				if d, ok := dq.(*dwarf.QualType); ok {
					t.C.Set(d.Qual + " " + t.C.String())
					dq = d.Type
				} else {
					break
				}
			}
			break
		}

		// Placeholder initialization; completed in FinishType.
		t.Go = &ast.StarExpr{}
		t.C.Set("<incomplete>*")
		if _, ok := c.ptrs[dt.Type]; !ok {
			c.ptrKeys = append(c.ptrKeys, dt.Type)
		}
		c.ptrs[dt.Type] = append(c.ptrs[dt.Type], t)

	case *dwarf.QualType:
		t1 := c.Type(dt.Type, pos)
		t.Size = t1.Size
		t.Align = t1.Align
		t.Go = t1.Go
		if unionWithPointer[t1.Go] {
			unionWithPointer[t.Go] = true
		}
		t.EnumValues = nil
		t.Typedef = ""
		t.C.Set("%s "+dt.Qual, t1.C)
		return t

	case *dwarf.StructType:
		// Convert to Go struct, being careful about alignment.
		// Have to give it a name to simulate C "struct foo" references.
		tag := dt.StructName
		if dt.ByteSize < 0 && tag == "" { // opaque unnamed struct - should not be possible
			break
		}
		if tag == "" {
			tag = "__" + strconv.Itoa(tagGen)
			tagGen++
		} else if t.C.Empty() {
			t.C.Set(dt.Kind + " " + tag)
		}
		name := c.Ident("_Ctype_" + dt.Kind + "_" + tag)
		t.Go = name // publish before recursive calls
		goIdent[name.Name] = name
		if dt.ByteSize < 0 {
			// Size calculation in c.Struct/c.Opaque will die with size=-1 (unknown),
			// so execute the basic things that the struct case would do
			// other than try to determine a Go representation.
			tt := *t
			tt.C = &TypeRepr{"%s %s", []interface{}{dt.Kind, tag}}
			tt.Go = c.Ident("struct{}")
			typedef[name.Name] = &tt
			break
		}
		switch dt.Kind {
		case "class", "union":
			t.Go = c.Opaque(t.Size)
			if c.dwarfHasPointer(dt, pos) {
				unionWithPointer[t.Go] = true
			}
			if t.C.Empty() {
				t.C.Set("__typeof__(unsigned char[%d])", t.Size)
			}
			t.Align = 1 // TODO: should probably base this on field alignment.
			typedef[name.Name] = t
		case "struct":
			g, csyntax, align := c.Struct(dt, pos)
			if t.C.Empty() {
				t.C.Set(csyntax)
			}
			t.Align = align
			tt := *t
			if tag != "" {
				tt.C = &TypeRepr{"struct %s", []interface{}{tag}}
			}
			tt.Go = g
			typedef[name.Name] = &tt
		}

	case *dwarf.TypedefType:
		// Record typedef for printing.
		if dt.Name == "_GoString_" {
			// Special C name for Go string type.
			// Knows string layout used by compilers: pointer plus length,
			// which rounds up to 2 pointers after alignment.
			t.Go = c.string
			t.Size = c.ptrSize * 2
			t.Align = c.ptrSize
			break
		}
		if dt.Name == "_GoBytes_" {
			// Special C name for Go []byte type.
			// Knows slice layout used by compilers: pointer, length, cap.
			t.Go = c.Ident("[]byte")
			t.Size = c.ptrSize + 4 + 4
			t.Align = c.ptrSize
			break
		}
		name := c.Ident("_Ctype_" + dt.Name)
		goIdent[name.Name] = name
		sub := c.Type(dt.Type, pos)
		t.Go = name
		if unionWithPointer[sub.Go] {
			unionWithPointer[t.Go] = true
		}
		t.Size = sub.Size
		t.Align = sub.Align
		oldType := typedef[name.Name]
		if oldType == nil {
			tt := *t
			tt.Go = sub.Go
			typedef[name.Name] = &tt
		}

		// If sub.Go.Name is "_Ctype_struct_foo" or "_Ctype_union_foo" or "_Ctype_class_foo",
		// use that as the Go form for this typedef too, so that the typedef will be interchangeable
		// with the base type.
		// In -godefs mode, do this for all typedefs.
		if isStructUnionClass(sub.Go) || *godefs {
			t.Go = sub.Go

			if isStructUnionClass(sub.Go) {
				// Use the typedef name for C code.
				typedef[sub.Go.(*ast.Ident).Name].C = t.C
			}

			// If we've seen this typedef before, and it
			// was an anonymous struct/union/class before
			// too, use the old definition.
			// TODO: it would be safer to only do this if
			// we verify that the types are the same.
			if oldType != nil && isStructUnionClass(oldType.Go) {
				t.Go = oldType.Go
			}
		}

	case *dwarf.UcharType:
		if t.Size != 1 {
			fatalf("%s: unexpected: %d-byte uchar type - %s", lineno(pos), t.Size, dtype)
		}
		t.Go = c.uint8
		t.Align = 1

	case *dwarf.UintType:
		if dt.BitSize > 0 {
			fatalf("%s: unexpected: %d-bit uint type - %s", lineno(pos), dt.BitSize, dtype)
		}
		switch t.Size {
		default:
			fatalf("%s: unexpected: %d-byte uint type - %s", lineno(pos), t.Size, dtype)
		case 1:
			t.Go = c.uint8
		case 2:
			t.Go = c.uint16
		case 4:
			t.Go = c.uint32
		case 8:
			t.Go = c.uint64
		case 16:
			t.Go = &ast.ArrayType{
				Len: c.intExpr(t.Size),
				Elt: c.uint8,
			}
		}
		if t.Align = t.Size; t.Align >= c.ptrSize {
			t.Align = c.ptrSize
		}

	case *dwarf.VoidType:
		t.Go = c.goVoid
		t.C.Set("void")
		t.Align = 1
	}

	switch dtype.(type) {
	case *dwarf.AddrType, *dwarf.BoolType, *dwarf.CharType, *dwarf.ComplexType, *dwarf.IntType, *dwarf.FloatType, *dwarf.UcharType, *dwarf.UintType:
		s := dtype.Common().Name
		if s != "" {
			if ss, ok := dwarfToName[s]; ok {
				s = ss
			}
			s = strings.Join(strings.Split(s, " "), "") // strip spaces
			name := c.Ident("_Ctype_" + s)
			tt := *t
			typedef[name.Name] = &tt
			if !*godefs {
				t.Go = name
			}
		}
	}

	if t.Size < 0 {
		// Unsized types are [0]byte, unless they're typedefs of other types
		// or structs with tags.
		// if so, use the name we've already defined.
		t.Size = 0
		switch dt := dtype.(type) {
		case *dwarf.TypedefType:
			// ok
		case *dwarf.StructType:
			if dt.StructName != "" {
				break
			}
			t.Go = c.Opaque(0)
		default:
			t.Go = c.Opaque(0)
		}
		if t.C.Empty() {
			t.C.Set("void")
		}
	}

	if t.C.Empty() {
		fatalf("%s: internal error: did not create C name for %s", lineno(pos), dtype)
	}

	return t
}

// isStructUnionClass reports whether the type described by the Go syntax x
// is a struct, union, or class with a tag.
func isStructUnionClass(x ast.Expr) bool {
	id, ok := x.(*ast.Ident)
	if !ok {
		return false
	}
	name := id.Name
	return strings.HasPrefix(name, "_Ctype_struct_") ||
		strings.HasPrefix(name, "_Ctype_union_") ||
		strings.HasPrefix(name, "_Ctype_class_")
}

// FuncArg returns a Go type with the same memory layout as
// dtype when used as the type of a C function argument.
func (c *typeConv) FuncArg(dtype dwarf.Type, pos token.Pos) *Type {
	t := c.Type(unqual(dtype), pos)
	switch dt := dtype.(type) {
	case *dwarf.ArrayType:
		// Arrays are passed implicitly as pointers in C.
		// In Go, we must be explicit.
		tr := &TypeRepr{}
		tr.Set("%s*", t.C)
		return &Type{
			Size:  c.ptrSize,
			Align: c.ptrSize,
			Go:    &ast.StarExpr{X: t.Go},
			C:     tr,
		}
	case *dwarf.TypedefType:
		// C has much more relaxed rules than Go for
		// implicit type conversions. When the parameter
		// is type T defined as *X, simulate a little of the
		// laxness of C by making the argument *X instead of T.
		if ptr, ok := base(dt.Type).(*dwarf.PtrType); ok {
			// Unless the typedef happens to point to void* since
			// Go has special rules around using unsafe.Pointer.
			if _, void := base(ptr.Type).(*dwarf.VoidType); void {
				break
			}

			t = c.Type(ptr, pos)
			if t == nil {
				return nil
			}

			// For a struct/union/class, remember the C spelling,
			// in case it has __attribute__((unavailable)).
			// See issue 2888.
			if isStructUnionClass(t.Go) {
				t.Typedef = dt.Name
			}
		}
	}
	return t
}

// FuncType returns the Go type analogous to dtype.
// There is no guarantee about matching memory layout.
func (c *typeConv) FuncType(dtype *dwarf.FuncType, pos token.Pos) *FuncType {
	p := make([]*Type, len(dtype.ParamType))
	gp := make([]*ast.Field, len(dtype.ParamType))
	for i, f := range dtype.ParamType {
		// gcc's DWARF generator outputs a single DotDotDotType parameter for
		// function pointers that specify no parameters (e.g. void
		// (*__cgo_0)()).  Treat this special case as void. This case is
		// invalid according to ISO C anyway (i.e. void (*__cgo_1)(...) is not
		// legal).
		if _, ok := f.(*dwarf.DotDotDotType); ok && i == 0 {
			p, gp = nil, nil
			break
		}
		p[i] = c.FuncArg(f, pos)
		gp[i] = &ast.Field{Type: p[i].Go}
	}
	var r *Type
	var gr []*ast.Field
	if _, ok := base(dtype.ReturnType).(*dwarf.VoidType); ok {
		gr = []*ast.Field{{Type: c.goVoid}}
	} else if dtype.ReturnType != nil {
		r = c.Type(unqual(dtype.ReturnType), pos)
		gr = []*ast.Field{{Type: r.Go}}
	}
	return &FuncType{
		Params: p,
		Result: r,
		Go: &ast.FuncType{
			Params:  &ast.FieldList{List: gp},
			Results: &ast.FieldList{List: gr},
		},
	}
}

// Identifier
func (c *typeConv) Ident(s string) *ast.Ident {
	return ast.NewIdent(s)
}

// Opaque type of n bytes.
func (c *typeConv) Opaque(n int64) ast.Expr {
	return &ast.ArrayType{
		Len: c.intExpr(n),
		Elt: c.byte,
	}
}

// Expr for integer n.
func (c *typeConv) intExpr(n int64) ast.Expr {
	return &ast.BasicLit{
		Kind:  token.INT,
		Value: strconv.FormatInt(n, 10),
	}
}

// Add padding of given size to fld.
func (c *typeConv) pad(fld []*ast.Field, sizes []int64, size int64) ([]*ast.Field, []int64) {
	n := len(fld)
	fld = fld[0 : n+1]
	fld[n] = &ast.Field{Names: []*ast.Ident{c.Ident("_")}, Type: c.Opaque(size)}
	sizes = sizes[0 : n+1]
	sizes[n] = size
	return fld, sizes
}

// Struct conversion: return Go and (gc) C syntax for type.
func (c *typeConv) Struct(dt *dwarf.StructType, pos token.Pos) (expr *ast.StructType, csyntax string, align int64) {
	// Minimum alignment for a struct is 1 byte.
	align = 1

	var buf bytes.Buffer
	buf.WriteString("struct {")
	fld := make([]*ast.Field, 0, 2*len(dt.Field)+1) // enough for padding around every field
	sizes := make([]int64, 0, 2*len(dt.Field)+1)
	off := int64(0)

	// Rename struct fields that happen to be named Go keywords into
	// _{keyword}.  Create a map from C ident -> Go ident. The Go ident will
	// be mangled. Any existing identifier that already has the same name on
	// the C-side will cause the Go-mangled version to be prefixed with _.
	// (e.g. in a struct with fields '_type' and 'type', the latter would be
	// rendered as '__type' in Go).
	ident := make(map[string]string)
	used := make(map[string]bool)
	for _, f := range dt.Field {
		ident[f.Name] = f.Name
		used[f.Name] = true
	}

	if !*godefs {
		for cid, goid := range ident {
			if token.Lookup(goid).IsKeyword() {
				// Avoid keyword
				goid = "_" + goid

				// Also avoid existing fields
				for _, exist := used[goid]; exist; _, exist = used[goid] {
					goid = "_" + goid
				}

				used[goid] = true
				ident[cid] = goid
			}
		}
	}

	anon := 0
	for _, f := range dt.Field {
		if f.ByteOffset > off {
			fld, sizes = c.pad(fld, sizes, f.ByteOffset-off)
			off = f.ByteOffset
		}

		name := f.Name
		ft := f.Type

		// In godefs mode, if this field is a C11
		// anonymous union then treat the first field in the
		// union as the field in the struct. This handles
		// cases like the glibc <sys/resource.h> file; see
		// issue 6677.
		if *godefs {
			if st, ok := f.Type.(*dwarf.StructType); ok && name == "" && st.Kind == "union" && len(st.Field) > 0 && !used[st.Field[0].Name] {
				name = st.Field[0].Name
				ident[name] = name
				ft = st.Field[0].Type
			}
		}

		// TODO: Handle fields that are anonymous structs by
		// promoting the fields of the inner struct.

		t := c.Type(ft, pos)
		tgo := t.Go
		size := t.Size
		talign := t.Align
		if f.BitSize > 0 {
			if f.BitSize%8 != 0 {
				continue
			}
			size = f.BitSize / 8
			name := tgo.(*ast.Ident).String()
			if strings.HasPrefix(name, "int") {
				name = "int"
			} else {
				name = "uint"
			}
			tgo = ast.NewIdent(name + fmt.Sprint(f.BitSize))
			talign = size
		}

		if talign > 0 && f.ByteOffset%talign != 0 {
			// Drop misaligned fields, the same way we drop integer bit fields.
			// The goal is to make available what can be made available.
			// Otherwise one bad and unneeded field in an otherwise okay struct
			// makes the whole program not compile. Much of the time these
			// structs are in system headers that cannot be corrected.
			continue
		}
		n := len(fld)
		fld = fld[0 : n+1]
		if name == "" {
			name = fmt.Sprintf("anon%d", anon)
			anon++
			ident[name] = name
		}
		fld[n] = &ast.Field{Names: []*ast.Ident{c.Ident(ident[name])}, Type: tgo}
		sizes = sizes[0 : n+1]
		sizes[n] = size
		off += size
		buf.WriteString(t.C.String())
		buf.WriteString(" ")
		buf.WriteString(name)
		buf.WriteString("; ")
		if talign > align {
			align = talign
		}
	}
	if off < dt.ByteSize {
		fld, sizes = c.pad(fld, sizes, dt.ByteSize-off)
		off = dt.ByteSize
	}

	// If the last field in a non-zero-sized struct is zero-sized
	// the compiler is going to pad it by one (see issue 9401).
	// We can't permit that, because then the size of the Go
	// struct will not be the same as the size of the C struct.
	// Our only option in such a case is to remove the field,
	// which means that it cannot be referenced from Go.
	for off > 0 && sizes[len(sizes)-1] == 0 {
		n := len(sizes)
		fld = fld[0 : n-1]
		sizes = sizes[0 : n-1]
	}

	if off != dt.ByteSize {
		fatalf("%s: struct size calculation error off=%d bytesize=%d", lineno(pos), off, dt.ByteSize)
	}
	buf.WriteString("}")
	csyntax = buf.String()

	if *godefs {
		godefsFields(fld)
	}
	expr = &ast.StructType{Fields: &ast.FieldList{List: fld}}
	return
}

// dwarfHasPointer returns whether the DWARF type dt contains a pointer.
func (c *typeConv) dwarfHasPointer(dt dwarf.Type, pos token.Pos) bool {
	switch dt := dt.(type) {
	default:
		fatalf("%s: unexpected type: %s", lineno(pos), dt)
		return false

	case *dwarf.AddrType, *dwarf.BoolType, *dwarf.CharType, *dwarf.EnumType,
		*dwarf.FloatType, *dwarf.ComplexType, *dwarf.FuncType,
		*dwarf.IntType, *dwarf.UcharType, *dwarf.UintType, *dwarf.VoidType:

		return false

	case *dwarf.ArrayType:
		return c.dwarfHasPointer(dt.Type, pos)

	case *dwarf.PtrType:
		return true

	case *dwarf.QualType:
		return c.dwarfHasPointer(dt.Type, pos)

	case *dwarf.StructType:
		for _, f := range dt.Field {
			if c.dwarfHasPointer(f.Type, pos) {
				return true
			}
		}
		return false

	case *dwarf.TypedefType:
		if dt.Name == "_GoString_" || dt.Name == "_GoBytes_" {
			return true
		}
		return c.dwarfHasPointer(dt.Type, pos)
	}
}

func upper(s string) string {
	if s == "" {
		return ""
	}
	r, size := utf8.DecodeRuneInString(s)
	if r == '_' {
		return "X" + s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

// godefsFields rewrites field names for use in Go or C definitions.
// It strips leading common prefixes (like tv_ in tv_sec, tv_usec)
// converts names to upper case, and rewrites _ into Pad_godefs_n,
// so that all fields are exported.
func godefsFields(fld []*ast.Field) {
	prefix := fieldPrefix(fld)
	npad := 0
	for _, f := range fld {
		for _, n := range f.Names {
			if n.Name != prefix {
				n.Name = strings.TrimPrefix(n.Name, prefix)
			}
			if n.Name == "_" {
				// Use exported name instead.
				n.Name = "Pad_cgo_" + strconv.Itoa(npad)
				npad++
			}
			n.Name = upper(n.Name)
		}
	}
}

// fieldPrefix returns the prefix that should be removed from all the
// field names when generating the C or Go code. For generated
// C, we leave the names as is (tv_sec, tv_usec), since that's what
// people are used to seeing in C.  For generated Go code, such as
// package syscall's data structures, we drop a common prefix
// (so sec, usec, which will get turned into Sec, Usec for exporting).
func fieldPrefix(fld []*ast.Field) string {
	prefix := ""
	for _, f := range fld {
		for _, n := range f.Names {
			// Ignore field names that don't have the prefix we're
			// looking for. It is common in C headers to have fields
			// named, say, _pad in an otherwise prefixed header.
			// If the struct has 3 fields tv_sec, tv_usec, _pad1, then we
			// still want to remove the tv_ prefix.
			// The check for "orig_" here handles orig_eax in the
			// x86 ptrace register sets, which otherwise have all fields
			// with reg_ prefixes.
			if strings.HasPrefix(n.Name, "orig_") || strings.HasPrefix(n.Name, "_") {
				continue
			}
			i := strings.Index(n.Name, "_")
			if i < 0 {
				continue
			}
			if prefix == "" {
				prefix = n.Name[:i+1]
			} else if prefix != n.Name[:i+1] {
				return ""
			}
		}
	}
	return prefix
}

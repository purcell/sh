// Copyright (c) 2017, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package interp

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"mvdan.cc/sh/syntax"
)

// A Runner interprets shell programs. It cannot be reused once a
// program has been interpreted.
//
// Note that writes to Stdout and Stderr may not be sequential. If
// you plan on using an io.Writer implementation that isn't safe for
// concurrent use, consider a workaround like hiding writes behind a
// mutex.
type Runner struct {
	// Env specifies the environment of the interpreter.
	// If Env is nil, Run uses the current process's environment.
	Env []string

	// envMap is just Env as a map, to simplify and speed up its use
	envMap map[string]string

	// Dir specifies the working directory of the command. If Dir is
	// the empty string, Run runs the command in the calling
	// process's current directory.
	Dir string

	// Params are the current parameters, e.g. from running a shell
	// file or calling a function. Accessible via the $@/$* family
	// of vars.
	Params []string

	Exec ModuleExec
	Open ModuleOpen

	filename string // only if Node was a File

	// Separate maps, note that bash allows a name to be both a var
	// and a func simultaneously
	Vars  map[string]Variable
	Funcs map[string]*syntax.Stmt

	// like Vars, but local to a func i.e. "local foo=bar"
	funcVars map[string]Variable

	// like Vars, but local to a cmd i.e. "foo=bar prog args..."
	cmdVars map[string]VarValue

	// >0 to break or continue out of N enclosing loops
	breakEnclosing, contnEnclosing int

	inLoop    bool
	canReturn bool

	err  error // current fatal error
	exit int   // current (last) exit code

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	bgShells sync.WaitGroup

	// Context can be used to cancel the interpreter before it finishes
	Context context.Context

	stopOnCmdErr bool // set -e
	noGlob       bool // set -f
	allExport    bool // set -a
	noUnset      bool // set -u
	noExec       bool // set -n

	dirStack []string

	optState getopts

	ifsJoin string
	ifsRune func(rune) bool

	// KillTimeout holds how much time the interpreter will wait for a
	// program to stop after being sent an interrupt signal, after
	// which a kill signal will be sent. This process will happen when the
	// interpreter's context is cancelled.
	//
	// The zero value will default to 2 seconds.
	//
	// A negative value means that a kill signal will be sent immediately.
	//
	// On Windows, the kill signal is always sent immediately,
	// because Go doesn't currently support sending Interrupt on Windows.
	KillTimeout time.Duration
}

// Reset will set the unexported fields back to zero, fill any exported
// fields with their default values if not set, and prepare the runner
// to interpret a program.
//
// This function should be called once before running any node. It can
// be skipped before any following runs to keep internal state, such as
// declared variables.
func (r *Runner) Reset() error {
	// reset the internal state
	*r = Runner{
		Env:         r.Env,
		Dir:         r.Dir,
		Params:      r.Params,
		Context:     r.Context,
		Stdin:       r.Stdin,
		Stdout:      r.Stdout,
		Stderr:      r.Stderr,
		Exec:        r.Exec,
		Open:        r.Open,
		KillTimeout: r.KillTimeout,
	}
	if r.Context == nil {
		r.Context = context.Background()
	}
	if r.Env == nil {
		r.Env = os.Environ()
	}
	r.envMap = make(map[string]string, len(r.Env))
	for _, kv := range r.Env {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			return fmt.Errorf("env not in the form key=value: %q", kv)
		}
		name, val := kv[:i], kv[i+1:]
		r.envMap[name] = val
	}
	r.Vars = make(map[string]Variable, 4)
	if _, ok := r.envMap["HOME"]; !ok {
		u, _ := user.Current()
		r.Vars["HOME"] = Variable{Value: StringVal(u.HomeDir)}
	}
	if r.Dir == "" {
		dir, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("could not get current dir: %v", err)
		}
		r.Dir = dir
	}
	r.Vars["PWD"] = Variable{Value: StringVal(r.Dir)}
	r.Vars["IFS"] = Variable{Value: StringVal(" \t\n")}
	r.ifsUpdated()
	r.Vars["OPTIND"] = Variable{Value: StringVal("1")}

	// convert $PATH to a unix path list
	path := r.envMap["PATH"]
	path = strings.Join(filepath.SplitList(path), ":")
	r.Vars["PATH"] = Variable{Value: StringVal(path)}

	r.dirStack = []string{r.Dir}
	if r.Exec == nil {
		r.Exec = DefaultExec
	}
	if r.Open == nil {
		r.Open = DefaultOpen
	}
	if r.KillTimeout == 0 {
		r.KillTimeout = 2 * time.Second
	}
	return nil
}

func (r *Runner) ctx() Ctxt {
	c := Ctxt{
		Context:     r.Context,
		Env:         r.Env,
		Dir:         r.Dir,
		Stdin:       r.Stdin,
		Stdout:      r.Stdout,
		Stderr:      r.Stderr,
		KillTimeout: r.KillTimeout,
	}
	for name, vr := range r.Vars {
		if !vr.Exported {
			continue
		}
		c.Env = append(c.Env, name+"="+r.varStr(vr, 0))
	}
	for name, val := range r.cmdVars {
		vr := Variable{Value: val}
		c.Env = append(c.Env, name+"="+r.varStr(vr, 0))
	}
	return c
}

type Variable struct {
	Local    bool
	Exported bool
	ReadOnly bool
	NameRef  bool
	Value    VarValue
}

// VarValue is one of:
//
//     StringVal
//     IndexArray
//     AssocArray
type VarValue interface{}

type StringVal string

type IndexArray []string

type AssocArray map[string]string

// maxNameRefDepth defines the maximum number of times to follow
// references when expanding a variable. Otherwise, simple name
// reference loops could crash the interpreter quite easily.
const maxNameRefDepth = 100

func (r *Runner) varStr(vr Variable, depth int) string {
	if depth > maxNameRefDepth {
		return ""
	}
	switch x := vr.Value.(type) {
	case StringVal:
		if vr.NameRef {
			vr, _ = r.lookupVar(string(x))
			return r.varStr(vr, depth+1)
		}
		return string(x)
	case IndexArray:
		if len(x) > 0 {
			return x[0]
		}
	case AssocArray:
		// nothing to do
	}
	return ""
}

func (r *Runner) varInd(vr Variable, e syntax.ArithmExpr, depth int) string {
	if depth > maxNameRefDepth {
		return ""
	}
	switch x := vr.Value.(type) {
	case StringVal:
		if vr.NameRef {
			vr, _ = r.lookupVar(string(x))
			return r.varInd(vr, e, depth+1)
		}
		if r.arithm(e) == 0 {
			return string(x)
		}
	case IndexArray:
		switch anyOfLit(e, "@", "*") {
		case "@":
			return strings.Join(x, " ")
		case "*":
			return strings.Join(x, r.ifsJoin)
		}
		i := r.arithm(e)
		if len(x) > 0 {
			return x[i]
		}
	case AssocArray:
		if lit := anyOfLit(e, "@", "*"); lit != "" {
			var strs IndexArray
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				strs = append(strs, x[k])
			}
			if lit == "*" {
				return strings.Join(strs, r.ifsJoin)
			}
			return strings.Join(strs, " ")
		}
		return x[r.loneWord(e.(*syntax.Word))]
	}
	return ""
}

type ExitCode uint8

func (e ExitCode) Error() string { return fmt.Sprintf("exit status %d", e) }

type RunError struct {
	Filename string
	syntax.Pos
	Text string
}

func (e RunError) Error() string {
	if e.Filename == "" {
		return fmt.Sprintf("%s: %s", e.Pos.String(), e.Text)
	}
	return fmt.Sprintf("%s:%s: %s", e.Filename, e.Pos.String(), e.Text)
}

func (r *Runner) setErr(err error) {
	if r.err == nil {
		r.err = err
	}
}

func (r *Runner) runErr(pos syntax.Pos, format string, a ...interface{}) {
	r.setErr(RunError{
		Filename: r.filename,
		Pos:      pos,
		Text:     fmt.Sprintf(format, a...),
	})
}

func (r *Runner) lastExit() {
	if r.err == nil {
		r.err = ExitCode(r.exit)
	}
}

func (r *Runner) setVarString(name, val string) {
	r.setVar(name, nil, Variable{Value: StringVal(val)})
}

func (r *Runner) setVarInternal(name string, vr Variable) {
	if _, ok := vr.Value.(StringVal); ok {
		if r.allExport {
			vr.Exported = true
		}
	} else {
		vr.Exported = false
	}
	if vr.Local {
		if r.funcVars == nil {
			r.funcVars = make(map[string]Variable)
		}
		r.funcVars[name] = vr
	} else {
		r.Vars[name] = vr
	}
	if name == "IFS" {
		r.ifsUpdated()
	}
}

func (r *Runner) setVar(name string, index syntax.ArithmExpr, vr Variable) {
	cur, _ := r.lookupVar(name)
	if cur.ReadOnly {
		r.errf("%s: readonly variable\n", name)
		r.exit = 1
		r.lastExit()
		return
	}
	_, isIndexArray := cur.Value.(IndexArray)
	_, isAssocArray := cur.Value.(AssocArray)

	if _, ok := vr.Value.(StringVal); ok && index == nil {
		// When assigning a string to an array, fall back to the
		// zero value for the index.
		if isIndexArray {
			index = &syntax.Word{Parts: []syntax.WordPart{
				&syntax.Lit{Value: "0"},
			}}
		} else if isAssocArray {
			index = &syntax.Word{Parts: []syntax.WordPart{
				&syntax.DblQuoted{},
			}}
		}
	}
	if index == nil {
		r.setVarInternal(name, vr)
		return
	}

	// from the syntax package, we know that val must be a string if
	// index is non-nil; nested arrays are forbidden.
	valStr := string(vr.Value.(StringVal))

	// if the existing variable is already an AssocArray, try our best
	// to convert the key to a string
	if stringIndex(index) || isAssocArray {
		var amap AssocArray
		switch x := cur.Value.(type) {
		case StringVal, IndexArray:
			return // TODO
		case AssocArray:
			amap = x
		}
		w, ok := index.(*syntax.Word)
		if !ok {
			return
		}
		k := r.loneWord(w)
		amap[k] = valStr
		cur.Value = amap
		r.setVarInternal(name, cur)
		return
	}
	var list IndexArray
	switch x := cur.Value.(type) {
	case StringVal:
		list = append(list, string(x))
	case IndexArray:
		list = x
	case AssocArray: // done above
	}
	k := r.arithm(index)
	for len(list) < k+1 {
		list = append(list, "")
	}
	list[k] = valStr
	cur.Value = list
	r.setVarInternal(name, cur)
}

func (r *Runner) lookupVar(name string) (Variable, bool) {
	if val, e := r.cmdVars[name]; e {
		return Variable{Value: val}, true
	}
	if vr, e := r.funcVars[name]; e {
		return vr, true
	}
	if vr, e := r.Vars[name]; e {
		return vr, true
	}
	if str, e := r.envMap[name]; e {
		return Variable{Value: StringVal(str)}, true
	}
	if r.noUnset {
		r.errf("%s: unbound variable\n", name)
		r.exit = 1
		r.lastExit()
	}
	return Variable{}, false
}

func (r *Runner) getVar(name string) string {
	val, _ := r.lookupVar(name)
	return r.varStr(val, 0)
}

func (r *Runner) delVar(name string) {
	delete(r.Vars, name)
	delete(r.envMap, name)
}

func (r *Runner) setFunc(name string, body *syntax.Stmt) {
	if r.Funcs == nil {
		r.Funcs = make(map[string]*syntax.Stmt, 4)
	}
	r.Funcs[name] = body
}

// FromArgs populates the shell options and returns the remaining
// arguments. For example, running FromArgs("-e", "--", "foo") will set
// the "-e" option and return []string{"foo"}.
//
// This is similar to what the interpreter's "set" builtin does.
func (r *Runner) FromArgs(args ...string) ([]string, error) {
opts:
	for len(args) > 0 {
		opt := args[0]
		if opt == "" || (opt[0] != '-' && opt[0] != '+') {
			break
		}
		enable := opt[0] == '-'
		switch opt[1:] {
		case "-":
			args = args[1:]
			break opts
		case "e":
			r.stopOnCmdErr = enable
		case "f":
			r.noGlob = enable
		case "a":
			r.allExport = enable
		case "u":
			r.noUnset = enable
		case "n":
			r.noExec = enable
		default:
			return nil, fmt.Errorf("invalid option: %q", opt)
		}
		args = args[1:]
	}
	return args, nil
}

// Run starts the interpreter and returns any error.
func (r *Runner) Run(node syntax.Node) error {
	r.filename = ""
	switch x := node.(type) {
	case *syntax.File:
		r.filename = x.Name
		r.stmts(x.StmtList)
	case *syntax.Stmt:
		r.stmt(x)
	case syntax.Command:
		r.cmd(x)
	default:
		return fmt.Errorf("Node can only be File, Stmt, or Command: %T", x)
	}
	r.lastExit()
	if r.err == ExitCode(0) {
		r.err = nil
	}
	return r.err
}

func (r *Runner) Stmt(stmt *syntax.Stmt) error {
	r.stmt(stmt)
	return r.err
}

func (r *Runner) outf(format string, a ...interface{}) {
	fmt.Fprintf(r.Stdout, format, a...)
}

func (r *Runner) errf(format string, a ...interface{}) {
	fmt.Fprintf(r.Stderr, format, a...)
}

func (r *Runner) stop() bool {
	if r.err != nil {
		return true
	}
	if err := r.Context.Err(); err != nil {
		r.err = err
		return true
	}
	if r.noExec {
		return true
	}
	return false
}

func (r *Runner) stmt(st *syntax.Stmt) {
	if r.stop() {
		return
	}
	if st.Background {
		r.bgShells.Add(1)
		r2 := r.sub()
		go func() {
			r2.stmtSync(st)
			r.bgShells.Done()
		}()
	} else {
		r.stmtSync(st)
	}
}

func stringIndex(index syntax.ArithmExpr) bool {
	w, ok := index.(*syntax.Word)
	if !ok || len(w.Parts) != 1 {
		return false
	}
	_, ok = w.Parts[0].(*syntax.DblQuoted)
	return ok
}

func (r *Runner) assignVal(as *syntax.Assign, valType string) VarValue {
	prev, prevOk := r.lookupVar(as.Name.Value)
	if as.Naked {
		return prev.Value
	}
	if as.Value != nil {
		s := r.loneWord(as.Value)
		if !as.Append || !prevOk {
			return StringVal(s)
		}
		switch x := prev.Value.(type) {
		case StringVal:
			return x + StringVal(s)
		case IndexArray:
			if len(x) == 0 {
				x = append(x, "")
			}
			x[0] += s
			return x
		case AssocArray:
			// TODO
		}
		return StringVal(s)
	}
	if as.Array == nil {
		return nil
	}
	elems := as.Array.Elems
	if valType == "" {
		if len(elems) == 0 || !stringIndex(elems[0].Index) {
			valType = "-a" // indexed
		} else {
			valType = "-A" // associative
		}
	}
	if valType == "-A" {
		// associative array
		amap := AssocArray(make(map[string]string, len(elems)))
		for _, elem := range elems {
			k := r.loneWord(elem.Index.(*syntax.Word))
			amap[k] = r.loneWord(elem.Value)
		}
		if !as.Append || !prevOk {
			return amap
		}
		// TODO
		return amap
	}
	// indexed array
	maxIndex := len(elems) - 1
	indexes := make([]int, len(elems))
	for i, elem := range elems {
		if elem.Index == nil {
			indexes[i] = i
			continue
		}
		k := r.arithm(elem.Index)
		indexes[i] = k
		if k > maxIndex {
			maxIndex = k
		}
	}
	strs := make([]string, maxIndex+1)
	for i, elem := range elems {
		strs[indexes[i]] = r.loneWord(elem.Value)
	}
	if !as.Append || !prevOk {
		return IndexArray(strs)
	}
	switch x := prev.Value.(type) {
	case StringVal:
		prevList := IndexArray([]string{string(x)})
		return append(prevList, strs...)
	case IndexArray:
		return append(x, strs...)
	case AssocArray:
		// TODO
	}
	return IndexArray(strs)
}

func (r *Runner) stmtSync(st *syntax.Stmt) {
	oldIn, oldOut, oldErr := r.Stdin, r.Stdout, r.Stderr
	for _, rd := range st.Redirs {
		cls, err := r.redir(rd)
		if err != nil {
			r.exit = 1
			return
		}
		if cls != nil {
			defer cls.Close()
		}
	}
	if st.Cmd == nil {
		r.exit = 0
	} else {
		r.cmd(st.Cmd)
	}
	if st.Negated {
		r.exit = oneIf(r.exit == 0)
	}
	r.Stdin, r.Stdout, r.Stderr = oldIn, oldOut, oldErr
}

func (r *Runner) sub() *Runner {
	r2 := *r
	r2.bgShells = sync.WaitGroup{}
	// TODO: perhaps we could do a lazy copy here, or some sort of
	// overlay to avoid copying all the time
	r2.Vars = make(map[string]Variable, len(r.Vars))
	for k, v := range r.Vars {
		r2.Vars[k] = v
	}
	return &r2
}

func (r *Runner) cmd(cm syntax.Command) {
	if r.stop() {
		return
	}
	switch x := cm.(type) {
	case *syntax.Block:
		r.stmts(x.StmtList)
	case *syntax.Subshell:
		r2 := r.sub()
		r2.stmts(x.StmtList)
		r.exit = r2.exit
		r.setErr(r2.err)
	case *syntax.CallExpr:
		fields := r.Fields(x.Args...)
		if len(fields) == 0 {
			for _, as := range x.Assigns {
				vr, _ := r.lookupVar(as.Name.Value)
				vr.Value = r.assignVal(as, "")
				r.setVar(as.Name.Value, as.Index, vr)
			}
			break
		}
		oldVars := r.cmdVars
		if r.cmdVars == nil {
			r.cmdVars = make(map[string]VarValue, len(x.Assigns))
		}
		for _, as := range x.Assigns {
			val := r.assignVal(as, "")
			r.cmdVars[as.Name.Value] = val
			if as.Name.Value == "IFS" {
				r.ifsUpdated()
				defer r.ifsUpdated()
			}
		}
		r.call(x.Args[0].Pos(), fields[0], fields[1:])
		r.cmdVars = oldVars
	case *syntax.BinaryCmd:
		switch x.Op {
		case syntax.AndStmt:
			r.stmt(x.X)
			if r.exit == 0 {
				r.stmt(x.Y)
			}
		case syntax.OrStmt:
			r.stmt(x.X)
			if r.exit != 0 {
				r.stmt(x.Y)
			}
		case syntax.Pipe, syntax.PipeAll:
			pr, pw := io.Pipe()
			r2 := r.sub()
			r2.Stdout = pw
			if x.Op == syntax.PipeAll {
				r2.Stderr = pw
			} else {
				r2.Stderr = r.Stderr
			}
			r.Stdin = pr
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				r2.stmt(x.X)
				pw.Close()
				wg.Done()
			}()
			r.stmt(x.Y)
			pr.Close()
			wg.Wait()
			r.setErr(r2.err)
		}
	case *syntax.IfClause:
		r.stmts(x.Cond)
		if r.exit == 0 {
			r.stmts(x.Then)
			return
		}
		r.exit = 0
		r.stmts(x.Else)
	case *syntax.WhileClause:
		for r.err == nil {
			r.stmts(x.Cond)
			stop := (r.exit == 0) == x.Until
			r.exit = 0
			if stop || r.loopStmtsBroken(x.Do) {
				break
			}
		}
	case *syntax.ForClause:
		switch y := x.Loop.(type) {
		case *syntax.WordIter:
			name := y.Name.Value
			for _, field := range r.Fields(y.Items...) {
				r.setVarString(name, field)
				if r.loopStmtsBroken(x.Do) {
					break
				}
			}
		case *syntax.CStyleLoop:
			r.arithm(y.Init)
			for r.arithm(y.Cond) != 0 {
				if r.loopStmtsBroken(x.Do) {
					break
				}
				r.arithm(y.Post)
			}
		}
	case *syntax.FuncDecl:
		r.setFunc(x.Name.Value, x.Body)
	case *syntax.ArithmCmd:
		if r.arithm(x.X) == 0 {
			r.exit = 1
		}
	case *syntax.LetClause:
		var val int
		for _, expr := range x.Exprs {
			val = r.arithm(expr)
		}
		if val == 0 {
			r.exit = 1
		}
	case *syntax.CaseClause:
		str := r.loneWord(x.Word)
		for _, ci := range x.Items {
			for _, word := range ci.Patterns {
				pat := r.lonePattern(word)
				if match(pat, str) {
					r.stmts(ci.StmtList)
					return
				}
			}
		}
	case *syntax.TestClause:
		if r.bashTest(x.X) == "" {
			if r.exit == 0 {
				// to preserve exit code 2 for regex
				// errors, etc
				r.exit = 1
			}
		} else {
			r.exit = 0
		}
	case *syntax.DeclClause:
		var modes []string
		valType := ""
		switch x.Variant.Value {
		case "local":
			modes = append(modes, "l")
		case "export":
			modes = append(modes, "-x")
		case "readonly":
			modes = append(modes, "-r")
		case "nameref":
			modes = append(modes, "-n")
		}
		for _, opt := range x.Opts {
			_ = opt
			switch s := r.loneWord(opt); s {
			case "l", "-x", "-r", "-n":
				modes = append(modes, s)
			case "-a", "-A":
				valType = s
			default:
				r.runErr(cm.Pos(), "unhandled declare opts")
			}
		}
		for _, as := range x.Assigns {
			for _, as := range r.expandAssigns(as) {
				name := as.Name.Value
				vr, _ := r.lookupVar(as.Name.Value)
				vr.Value = r.assignVal(as, valType)
				for _, mode := range modes {
					switch mode {
					case "l":
						vr.Local = true
					case "-x":
						vr.Exported = true
					case "-r":
						vr.ReadOnly = true
					case "-n":
						vr.NameRef = true
					}
				}
				r.setVar(name, as.Index, vr)
			}
		}
	case *syntax.TimeClause:
		start := time.Now()
		if x.Stmt != nil {
			r.stmt(x.Stmt)
		}
		real := time.Since(start)
		r.outf("\n")
		r.outf("real\t%s\n", elapsedString(real))
		// TODO: can we do these?
		r.outf("user\t0m0.000s\n")
		r.outf("sys\t0m0.000s\n")
	default:
		r.runErr(cm.Pos(), "unhandled command node: %T", x)
	}
	if r.exit != 0 && r.stopOnCmdErr {
		r.lastExit()
	}
}

func elapsedString(d time.Duration) string {
	min := int(d.Minutes())
	sec := math.Remainder(d.Seconds(), 60.0)
	return fmt.Sprintf("%dm%.3fs", min, sec)
}

func (r *Runner) stmts(sl syntax.StmtList) {
	for _, stmt := range sl.Stmts {
		r.stmt(stmt)
	}
}

func (r *Runner) redir(rd *syntax.Redirect) (io.Closer, error) {
	if rd.Hdoc != nil {
		hdoc := r.loneWord(rd.Hdoc)
		r.Stdin = strings.NewReader(hdoc)
		return nil, nil
	}
	orig := &r.Stdout
	if rd.N != nil {
		switch rd.N.Value {
		case "1":
		case "2":
			orig = &r.Stderr
		}
	}
	arg := r.loneWord(rd.Word)
	switch rd.Op {
	case syntax.WordHdoc:
		r.Stdin = strings.NewReader(arg + "\n")
		return nil, nil
	case syntax.DplOut:
		switch arg {
		case "1":
			*orig = r.Stdout
		case "2":
			*orig = r.Stderr
		}
		return nil, nil
	case syntax.RdrIn, syntax.RdrOut, syntax.AppOut,
		syntax.RdrAll, syntax.AppAll:
		// done further below
	// case syntax.DplIn:
	default:
		r.runErr(rd.Pos(), "unhandled redirect op: %v", rd.Op)
	}
	mode := os.O_RDONLY
	switch rd.Op {
	case syntax.AppOut, syntax.AppAll:
		mode = os.O_RDWR | os.O_CREATE | os.O_APPEND
	case syntax.RdrOut, syntax.RdrAll:
		mode = os.O_RDWR | os.O_CREATE | os.O_TRUNC
	}
	f, err := r.open(r.relPath(arg), mode, 0644, true)
	if err != nil {
		return nil, err
	}
	switch rd.Op {
	case syntax.RdrIn:
		r.Stdin = f
	case syntax.RdrOut, syntax.AppOut:
		*orig = f
	case syntax.RdrAll, syntax.AppAll:
		r.Stdout = f
		r.Stderr = f
	default:
		r.runErr(rd.Pos(), "unhandled redirect op: %v", rd.Op)
	}
	return f, nil
}

func (r *Runner) loopStmtsBroken(sl syntax.StmtList) bool {
	r.inLoop = true
	defer func() { r.inLoop = false }()
	for _, stmt := range sl.Stmts {
		r.stmt(stmt)
		if r.contnEnclosing > 0 {
			r.contnEnclosing--
			return r.contnEnclosing > 0
		}
		if r.breakEnclosing > 0 {
			r.breakEnclosing--
			return true
		}
	}
	return false
}

func (r *Runner) ifsUpdated() {
	runes := r.getVar("IFS")
	r.ifsJoin = ""
	if len(runes) > 0 {
		r.ifsJoin = runes[:1]
	}
	r.ifsRune = func(r rune) bool {
		for _, r2 := range runes {
			if r == r2 {
				return true
			}
		}
		return false
	}
}

type returnCode uint8

func (returnCode) Error() string { return "returned" }

func (r *Runner) call(pos syntax.Pos, name string, args []string) {
	if r.stop() {
		return
	}
	if body := r.Funcs[name]; body != nil {
		// stack them to support nested func calls
		oldParams := r.Params
		r.Params = args
		oldCanReturn := r.canReturn
		oldFuncVars := r.funcVars
		r.funcVars = nil
		r.canReturn = true

		r.stmt(body)

		r.Params = oldParams
		r.funcVars = oldFuncVars
		r.canReturn = oldCanReturn
		if code, ok := r.err.(returnCode); ok {
			r.err = nil
			r.exit = int(code)
		}
		return
	}
	if isBuiltin(name) {
		r.exit = r.builtinCode(pos, name, args)
		return
	}
	r.exec(name, args)
}

func (r *Runner) exec(name string, args []string) {
	path := r.lookPath(name)
	err := r.Exec(r.ctx(), name, path, args)
	switch x := err.(type) {
	case nil:
		r.exit = 0
	case ExitCode:
		r.exit = int(x)
	default:
		r.setErr(err)
	}
}

func (r *Runner) open(path string, flags int, mode os.FileMode, print bool) (io.ReadWriteCloser, error) {
	f, err := r.Open(r.ctx(), path, flags, mode)
	switch err.(type) {
	case nil:
	case *os.PathError:
		if print {
			r.errf("%v\n", err)
		}
	default:
		r.setErr(err)
	}
	return f, err
}

func (r *Runner) stat(name string) (os.FileInfo, error) {
	return os.Stat(r.relPath(name))
}

func (r *Runner) findExecutable(file string) error {
	d, err := r.stat(file)
	if err != nil {
		return err
	}
	if m := d.Mode(); !m.IsDir() && m&0111 != 0 {
		return nil
	}
	return os.ErrPermission
}

// splitList is like filepath.SplitList, but always using the unix path
// list separator ':'.
func splitList(path string) []string {
	if path == "" {
		return []string{""}
	}
	return strings.Split(path, ":")
}

func (r *Runner) lookPath(file string) string {
	if strings.Contains(file, "/") {
		if err := r.findExecutable(file); err == nil {
			return file
		}
		return ""
	}
	path := r.getVar("PATH")
	for _, dir := range splitList(path) {
		var path string
		switch dir {
		case "", ".":
			// otherwise "foo" won't be "./foo"
			path = "." + string(filepath.Separator) + file
		default:
			path = filepath.Join(dir, file)
		}
		if err := r.findExecutable(path); err == nil {
			return path
		}
	}
	return ""
}

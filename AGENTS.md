# AGENTS.md

Notes for anyone, human or agent, changing apogee. The README describes the
project's goals and API; this file describes how the implementation works
today, the rules it depends on, and where performance work should go next.

## Contributing

Issues and pull requests from agents are welcome, but please do not flood
the project with them: send a few that matter rather than many.

- **Search first.** Check open issues and PRs before filing; add to an
  existing one rather than opening a duplicate.
- **Issues:** one per distinct problem, with the smallest Lua script or Go
  program that shows it, what apogee does, and what C Lua 5.5 does. Put
  related findings in one issue rather than one each.
- **Pull requests:** one focused change each, verified with the checks in
  Working on the repository below, with a conventional-commit title (see
  Releases) and a description of what changed and how it was verified. A
  behaviour change comes with a test that fails without it; a performance
  change comes with its measurements. Leave out unrelated cleanups and
  sweeping reformatting.

## Working on the repository

- Go 1.27.1, `CGO_ENABLED=0`. Workflows pin every action to a full commit
  SHA with its version in a comment; Dependabot keeps them current.
- **Releases.** PRs are squash-merged, and the PR title becomes the commit
  release-please reads, so it is a conventional commit (`fix(jit): …`,
  `perf(lua): …`, `feat(stdlib): …`, `docs: …`); the pr-title workflow
  checks it. `feat`, `fix` and `perf` go in the changelog and bring a
  release; `docs`, `test`, `ci`, `chore` and `refactor` do not.
  release-please keeps a release PR open, and merging it tags the version
  (`vX.Y.Z`) and publishes the GitHub release. Only the root module is
  released; `bench` and `cmd/apogee` are separate modules, left out.
- **apogee is v1.** Its Go API is stable: a breaking change would need a
  `/v2` module path, so fix and extend without breaking it. Mark a
  deliberate break with `!` (`feat!: …`).
- Go has generics: generic types, and since Go 1.27 generic methods, on
  generic types too. Reach for them before writing one function per
  numeric type (`PushInteger[T Integer]`, `Arg[T]`, `RawGetInt[T]`).
- Run everything CI runs before pushing:
  - `gofmt -l .` must print nothing.
  - `go generate ./...` must leave the tree unchanged; it rewrites
    `lua/vm_jit.go`.
  - `go vet ./...`.
  - `go test ./...`, which runs with the JIT at its normal threshold;
    again with `APOGEE_JIT_TEST=1`, which compiles every function on first
    use; again with `APOGEE_JIT=off`, which only interprets; and
    `-race -run JIT`. CI also runs the JIT tests with `GOAMD64=v3`, where
    `sin` and `cos` are left to Go (`trigInline` is false).
  - `go vet ./...` with `GOOS`/`GOARCH` set for each of windows/amd64,
    windows/arm64, linux/386, darwin/amd64, darwin/arm64 and linux/arm64,
    which compiles the tests where the JIT does not exist.
  - `cd bench && APOGEE_JIT_TEST=1 go test -run 'TestSuiteAgrees|TestStandardAgrees' .`,
    and with `-tags clua55` or `-tags luajit` where those are installed.
  - For `cmd/apogee`, its own `go vet ./...` and `go test -race ./...`.
- On an Apple silicon Mac, `GOARCH=amd64 go test ./...` runs the amd64 JIT
  under Rosetta. A `GOAMD64=v3` binary cannot run there; CI covers it on
  linux/amd64.
- On linux/amd64 with qemu-user's binfmt handler installed,
  `GOARCH=arm64 go test ./...` runs the arm64 JIT under emulation.
- apogee targets Lua 5.5. The official Lua 5.5.1 suite, unmodified, is
  `lua-5.5-tests/`, run by
  `TestLua55` (lua/lua55_test.go). Its pending list says what each file
  still needs; `APOGEE_SUITE_PROGRESS=1` runs pending files and logs where
  they stop. Take a file off the list once it passes. C Lua 5.5 (`brew install lua`) is the reference
  for any behaviour the suite does not pin down: diff a script's output
  against it.
- Benchmarks: always pass `-ldflags=-funcalign=64`. Without it, unrelated
  changes move the interpreter loop's alignment and its timings by 5–10%.
  Compare back to back on an idle machine; on a CPU with more than one
  core complex, pin A/B runs to one (`taskset -c 0-5` on the 9900X3D).
  Every performance PR includes a full benchmark run (bench/README.md,
  Reproducing), saved as the results file for its machine
  (`bench/suite-results-amd64.txt`, or `bench/suite-results.txt` for
  Apple M1). The C interpreters' rows may be carried over from the last
  run when nothing changed them; say so in bench/README.md.
  `bench/chart` redraws that
  machine's charts and rewrites its tables in bench/README.md, and
  `-summary` rewrites the root README's table of geometric means from
  both files (bench/README.md, Reproducing).

## Packages

- The root holds no Go package. Users import `lua` and `stdlib`.
- `lua` is the State, its API (api*.go, auxiliary.go, debug_api.go), the
  VM, the object types and the JIT. They read each other's unexported
  fields, and the JIT hard-codes their layout. File names elsewhere in
  this document are in `lua/` unless a path says otherwise. Its tests
  open `fixtures/` and `libs/` (which the Lua suite's attrib.lua uses)
  relative to `lua/`, and the suite as `../lua-5.5-tests`.
- `internal/bytecode` is the instruction format, the opcodes, the limits
  the compiler and VM share, `Number` and its arithmetic (`Arith`, so
  constant folding and the VM compute alike), numeral parsing
  (`ParseNumber`, `Numeral`) and float formatting (`FormatFloat`), and
  `Proto`, a compiled function with Go constants (nil, bool, int64,
  float64, string).
- `internal/compiler` compiles source to a `bytecode.Proto`; it knows
  nothing of the VM. `Parse` returns syntax errors as Go errors. The core
  builds its
  `prototype` from a Proto (`prototypeOf`, compile.go), keeping runtime
  state beside it: the specialised code, field caches and JIT state.
- `internal/chunk` reads and writes binary chunks as Protos (`Load`,
  `Dump`), in apogee's own format, which C Lua's luac cannot read. `Load` returns malformed chunks as errors and bounds each
  allocation, but, like Lua, trusts the code of a well-formed chunk.
  `protoOf` (compile.go) converts a prototype back for `Dump`.
- `stdlib` holds the standard libraries and uses only `lua`'s public API.
  A library that needs something the API cannot do fast gets a public
  method in `lua`, as table.sort got `State.SortArray`.
- Tests that need only the public API are external (`package lua_test`)
  and call `stdlib.Open`. Tests that reach into internals stay in `lua`,
  which cannot import stdlib (it imports lua); they open the libraries
  with `openLibraries` from export_test.go, which libs_test.go, an
  external test file in the same binary, sets to `stdlib.Open`.

## The apogee command

- `cmd/apogee` is its own module, so the library's go.mod stays free of
  its dependencies (Bubble Tea v2, Bubbles, Lip Gloss, Chroma). It
  requires a released apogee version, not a replace directive, which
  `go install ...@latest` refuses; so CI builds it against that release,
  not this checkout. After a release, bump it with
  `cd cmd/apogee && go get github.com/matjam/apogee@vX.Y.Z`.
- For local development against this checkout, make an untracked
  workspace: `go work init . ./cmd/apogee` (go.work is gitignored; a
  committed one would put the root's `go test ./...` and bench/ in
  workspace mode).
- `standalone.go` ports lua.c: options, `LUA_INIT`, `arg`, `docall` with
  a traceback handler and SIGINT calling `State.Interrupt`, and the plain
  REPL. `tui.go` is the terminal REPL: an inline Bubble Tea program whose
  transcript goes to the scrollback through `Program.Println`.
- The TUI rules: Lua runs in a goroutine, and the state is touched only by
  the event loop while idle or by the evaluation while running. Every
  transcript line is printed from a goroutine (`print` blocks until the
  event loop takes it, which keeps order), never from `Update`.
  `capture` puts os.Stdout and os.Stderr through a pipe from startup, so
  print, io.write and child processes land in the transcript; each
  evaluation ends with `capture.Sync`, so no output is in flight when the
  program exits.
- Tests run the test binary as the command (`APOGEE_CLI_MAIN=1`) for lua.c
  behaviour, and drive the TUI model with key messages.

## Interpreter

- `value` (types.go) is 16 bytes: `p unsafe.Pointer` and `n float64`.
  Floats, integers, booleans and none have sentinel pointers in `p`; an
  integer keeps its int64's bits in `n`. The float and integer sentinels
  are adjacent bytes (`numberSentinels`), so `isNumber` is one subtract
  and compare, which compiled code repeats (`branchNumber`). Other kinds
  keep a kind tag in `n`'s top byte and the object in `p`; since an
  integer's bits can be any tag, a kind test must rule out both number
  sentinels first. Numbers and booleans never allocate. `value` is not
  comparable; use `rawEqual`, `hashKey` and `identical`. Table keys are
  normalised: a float with an integer value is stored as the integer.
- Numbers follow Lua 5.5, as 5.4 defined them: integers wrap around, `/`
  and `^` give floats, `//` and `%` floor, comparisons between integers
  and floats are exact (numbers.go), and a numeric for loop whose start
  and step are integers counts on integers (`forPrep`), never wrapping.
- `executeSwitch` (vm.go) is the interpreter loop. Keep rare work out of
  it, behind a call: one extra branch and a load in VARARG, or the
  to-be-closed code in RETURN, measured 10–20% slower on fib and closures,
  which never ran them. A/B any change to it on fib and closures. `prototype.exec`
  (specialise.go) holds a specialised copy of the bytecode with the same pc
  for every instruction: RR, RK and KR arithmetic, field instructions with
  constant string keys, and `opMulAddRKR`.
- Tables (tables.go, shape.go, field_cache.go) keep string keys in slots
  described by shared shapes (hidden classes). Each field instruction has a
  `fieldCache`, which can also cache a hit through a metatable's `__index`
  table. NEWTABLE sites remember the shape their last table reached.
- Number functions (number_function.go, `PushNumberFunction[F]`) are Go
  functions of float64s that CALL invokes without a Go frame. Any number
  argument converts to a float, and the result is a float, so only
  functions that always return floats (`math.sqrt`, `math.sin`) may be
  number functions.

## Coroutines

Coroutines work as C Lua's do (coroutine.go ports ldo.c's
`lua_resume`, `lua_yieldk`, `unroll` and `recover`, and lvm.c's
`luaV_finishOp`).

- **How a yield works.** A yield panics with `errYield`, unwinding the Go
  stack back to `Resume`'s `protect`. The coroutine's Lua frames stay on
  its own stack.
- **How a resume works.** `unroll` runs the frames on. A Go function
  continues through the continuation it gave `CallWithContinuation` or
  `ProtectedCallWithContinuation`. A Lua frame first finishes its
  interrupted instruction (`finishOp`), then `execute` carries on.
- **Errors inside a yieldable pcall** reach `Resume` like any error.
  `recover` then finds the pcall's frame and runs its continuation with the
  error status.
- **Why not goroutines.** A goroutine per coroutine would be simpler, but a
  switch would cost a scheduler hand-off, and a suspended coroutine that
  became garbage would leak its goroutine. This way a coroutine is only a
  stack, a switch is a panic and a recover, and the semantics are C Lua's:
  yields across pcall, metamethods and iterators, and "attempt to yield
  across a C-call boundary" for a Go function without a continuation, such
  as table.sort's comparator.

Rules any instruction that can call Lua must keep, in the interpreter and
in `jitStep`:

- `ci.savedPC` is past the instruction when it makes the call.
- A metamethod's result is on the top of the stack when it returns, as
  `callTagMethod` leaves it. `finishOp` moves it to its register.
- `finishOp` reads `prototype.Code`, the original bytecode. The specialised
  and patched copies must keep every instruction's position.
- SELF stores self in RA+1 after the lookup, not before as C does.
  `finishOp` stores it for a lookup a yield interrupted.

## To-be-closed variables

tbc.go follows lfunc.c and ldo.c. TBC puts a variable's stack index on its
thread's list (`State.tbc`); a generic for's closing value is one too, as
TBC with B set swaps it below the control variable (5.5's TFORPREP).

- **Closing.** A closing JMP (`closeFromJump`) and RETURN
  (`closeForReturn`) close upvalues, then call each `__close` innermost
  first, popping it from the list before the call. A yield in one unwinds
  as any yield does; `finishOp` then runs the JMP or RETURN again, which
  closes the rest. Both paths are out of line: code for them in
  `executeSwitch` measured 10–20% slower on fib and closures. The compiler
  emits no tail call inside a to-be-closed scope, as lparser.c does.
- **Errors.** `protectedCall` closes with `closeProtected`, each `__close`
  protected and unable to yield, an error in one replacing the error for
  the rest. A yieldable pcall in a coroutine closes in `finishGoCall`
  instead, able to yield, as finishpcallk does.
- **Coroutines.** One that dies by error keeps its variables pending;
  `CloseThread` (coroutine.close, and coroutine.wrap on an error) closes
  them. `CloseRunning` is 5.5's `coroutine.close()` of itself.
- **JIT.** Compiled code runs TBC for a nil closing value, which closes
  nothing (a generic for over `pairs` has one), and exits for any other.
  RETURN in a prototype with TBC checks `jitContext.tbc` and exits while
  a variable is pending, so compiled code never returns past one.

## Buffers

buffer.go, after LuaJIT's FFI arrays: `PushBuffer[T]` gives Lua a Go
slice of float64, float32, int32 or uint8 as a userdata whose `buf`
points at a `buffer` (address, length, kind), with no metatable. Indexing
counts from 0; reads outside, or with keys that are not integers, give
nil; writes there raise an error; stores convert to the element type as
Go does, integers wrapping, and fractions refused for integer buffers.
`tableAt`, `setTableAt` and `objectLength` handle buffers before the
userdata metatable path; compiled code handles them inline. Elements hold
no pointers, so no write barrier is involved. What transfers from
LuaJIT's FFI is typed memory, not direct calls: generated code never
calls Go.

## Named vararg tables

Lua 5.5's `function f(...t)` (vararg.go). The compiler tracks the name:
used only as `t[k]` or `t.k`, the proto is `VarArgView` and the register
holds `varArgView`, a sentinel that `tableAt` answers from the frame's
extra arguments, so nothing allocates; used any other way (passed,
returned, captured, assigned into, `_ENV`), it is `VarArgTable`, entry
builds `{n = #extras, ...}`, and `...` reads the table, checking `n`. The
opcode space is full, which is why the view is a value, not C Lua's
GETVARG instruction.

## Allocation limit

memory.go. `SetAllocationLimit(n)` bounds what a state and its threads
allocate from then on; `charge(n)` counts n bytes before an allocation
and raises `ErrMemory` (the preallocated "not enough memory", no message
handler) when they do not fit. The count only rises, garbage included,
so it is deterministic whatever Go's collector does. Without a limit,
`allocRemaining` still counts down from `math.MaxInt`, and `outOfMemory`
restarts it.

- **Every allocation Lua can repeat charges first:** new tables
  (`newTableAt`, `CreateTable`, vararg tables), a table's new string key,
  hash entry or array extension (the table methods that grow take the
  state), closures and new upvalues, CONCAT and number-to-string
  conversions, `PushString`, userdata, Go closures, threads, stack growth,
  and `next`'s key snapshots. A new allocation site must charge too.
- **Estimates:** 16 bytes a value slot, a string's length, struct sizes
  from `unsafe.Sizeof`. A substring counts its length, though Go shares
  the bytes, as C Lua copies them.
- **Builders check before they grow:** a stdlib function that builds a
  string larger than its inputs (`rep`, `gsub`, `format`,
  `table.concat`, `pack`, `upper`/`lower`/`reverse`, `io.read`) calls
  `CheckAllocation` with the size so far; `PushString` then counts it.
- **Compiled code never allocates:** NEWTABLE, CLOSURE, CONCAT, new keys
  and array growth exit to Go, which charges.
- Not counted: the compiler, `string.dump`, and what Go functions allocate
  for themselves unless they call `Charge`.

## Garbage collection

Go's collector frees apogee's memory. A Lua collection (gc.go) adds what
Lua defines beyond freeing memory: weak tables and `__gc` finalizers. It
follows lgc.c's atomic phase.

- **What a collection does.** It marks from Lua's roots: the registry,
  the basic types' metatables, the main thread, the running thread and
  finalizers still due. Ephemeron tables are marked to convergence. It
  then clears weak values, moves unreached finalizable objects to
  `toFinalize` (resurrecting them), clears weak keys and weak values
  again, and runs the finalizers newest first. An error in one is a
  warning, "error in __gc (...)", as in 5.4 on. It also clears each
  thread's stack above its top, and "buries" the keys of dictionary
  shapes' nil slots (`shape.buried`), so that Go frees dead keys as C Lua
  does; `next` resumes from a buried key by its hash. Shared shapes take
  no key longer than 40 bytes, since they outlive their tables.
- **Closing a state.** `State.Close`, as lua_close, closes the main
  thread's to-be-closed variables and runs every pending finalizer,
  newest first; `os.exit(code, true)` calls it. Files a script opens are
  fully buffered, as in C, and os.exit flushes them, as C's exit does, so
  a host that ends the process otherwise should close its states.
- **When it runs.** On `collectgarbage("collect")` and `"step"`, and
  automatically once a metatable with `__mode` or `__gc` is set, since a
  state without either has nothing for it to do. The automatic pacing is
  C Lua's: collect once the state has allocated (pause − 100)% of the live
  heap the last collection kept. `checkGC` looks only after a Go
  collection, which a re-arming finalizer sentinel counts, so the check
  where C Lua steps its collector (NEWTABLE, CONCAT, CLOSURE, and calls
  and resumes from Go) is one load and compare. A collection that frees
  nothing doubles the wait, up to 64 times: every state's standard files
  have `__gc`, which would otherwise make every state collect.
- **Finalizers run on their own thread** (`finalizerThread`), not on the
  running one. A collection therefore never moves the running thread's
  stack, and the interpreter and `jitStep` keep their frame across
  NEWTABLE and CLOSURE without reloading it. A finalizer sees that thread
  as `coroutine.running()`.
- **Why not Go finalizers.** `runtime.SetFinalizer` runs on another
  goroutine at an unknown time, can't resurrect in Lua's order, and can't
  tell a weak table's entries from strong ones. A Lua-level mark over
  Lua's own roots gives C Lua's semantics: 5.5's gc.lua passes.
- **Differences.** The collection is not incremental: it marks the whole
  Lua heap at once. An object only Go memory refers to, outside the
  registry and the stacks, counts as unreachable, so a Go embedder must
  keep such objects in the registry. `__mode` and `__gc` are noticed when
  the metatable is set, as C Lua reads `__gc`. Of `collectgarbage`'s
  "param" parameters only "pause" paces collections; the modes are
  remembered and reported, and "step" in generational mode collects.
  The count is Go's heap, so Go's own warm-up (starting GC worker
  threads) can grow it by a few kilobytes over the first collections.

## JIT

On by default; `NewState(WithoutJIT())` or `APOGEE_JIT=off` turns it off.
It runs on linux and darwin, arm64 and amd64. Elsewhere `jit_none.go`
makes it a no-op. JIT tests call `skipWithoutJIT`, which skips them where
nothing compiles.

- **Hand-over:**
  - A JIT state runs `executeSwitchJIT` (vm_jit.go), which
    internal/jitvm generates from `executeSwitch`. It adds one check for
    the patched opcodes `opJITCount` and `opJITEnter`, so states without
    the JIT run the original loop.
  - `jitOrig` keeps the unpatched instructions. The counter sits at pc 0,
    each FORLOOP, and the head of every other loop (the target of a
    backward JMP or of TFORLOOP), so a function run once with a hot while
    loop compiles; after `jitThreshold` (1000) counts `compileJIT` runs,
    and `opJITEnter` goes at the entries `jitEntries` picks.
  - Never patch an instruction its predecessor consumes: the JMP after a
    test, TFORLOOP after TFORCALL, or an EXTRAARG word.
- **Driver (`runJIT`, jit.go):**
  - It enters compiled code through `internal/jit/call` and handles exits
    in Go.
  - Compiled code exits with `jitExitCallGo` at a CALL whose callee is a
    Go closure, or a Go function that is neither an inline intrinsic nor a
    number function, and with `jitExitCallNumber` for a number function. The exit stub leaves the callee's object in
    `jitContext.callee` and the frame register in `jitContext.frame`
    (native calls move the frame without updating the context), and the
    driver calls it (`jitCallGoFunction`) and re-enters after it. The
    check sits out of line, after the Lua closure check, so Lua-to-Lua
    calls neither run it nor have it in their code path.
  - Other exits at CALL and at RETURN are run by the driver, as are
    CLOSURE, NEWTABLE, LEN of a table with a hash part or `__len`,
    generic table access, SETLIST into a short
    array and upvalue-closing JMPs with something to close (`jitStep`).
    Compiled code then carries on after the instruction.
  - `runJIT` reloads the closure and prototype only when `l.callInfo`
    changes. The chain of loads from a callInfo to its prototype is most
    of a crossing's cost otherwise.
  - Anything else goes back to the interpreter. The interpreter reloads
    its frame from `l.callInfo` after every hand-over.
  - `jitEntries` leaves out entries from which compiled code would run
    fewer than `jitMinRun` instructions before an exit (`worthEntering`).
    A RETURN does not end that count: into a compiled caller it is a
    native return. Tests set `jitMinRun` to zero so that compiled code runs
    wherever it can.
  - When Go calls a compiled Lua function (`l.call`), `callJIT` runs it
    with `runJIT` straight from `preCall`, without the interpreter. A
    function that returns within `jitMinRun` instructions of pc 0
    (`returnsSoon`), such as a sort comparator, is interpreted instead,
    and pc 0 is not patched: its RETURN would go back to Go or to
    interpreted code, and on the M1 the round trip into compiled code and
    back costs about 10 ns, more than interpreting it. Compiled callers
    still call it natively. That
    frame is `runJIT`'s `bottom`: at its RETURN, `jitReturnToGo` does the
    interpreter's general return and `runJIT` reports that the call is
    done. If compiled code stops anywhere else, `execute` carries on from
    `l.callInfo`.
- **Compilers:**
  - `jit_arm64*.go` and `jit_amd64*.go` are separate, with the same
    structure.
  - Shared pieces: struct offsets (jit_layout.go), the numeric-loop
    analysis (jit_kernel.go) and the trig constants (jit_trig.go).
  - The encoders in internal/jit/arm64 and internal/jit/amd64 are checked
    against clang's output.
  - arm64's TBZ and TBNZ reach ±32 KB, which a function of a hundred
    instructions can outgrow (cd's `RedBlackTree:put`, havlak's
    `find_loops`). On `ErrTestRange` the function compiles again with
    `LongTests`: the opposite test over a B.
- **Coverage:**
  - Moves, constants, arithmetic, comparisons, branches and numeric for
    loops, on integers and floats. Two integers take the integer path (the
    integer sentinel is `rInteger` on arm64 and `p - rNumber == 1` on
    amd64, which has no register to spare); a float operand converts the
    other (`loadFloat`). Integer `%`, `//` and the bitwise operators
    compile (jit_*_integer.go). `%` and `//` by an integer constant
    that fits 32 bits divide without a divide instruction, in kernels
    too (jit_divide.go): a shift and mask for a power of two, otherwise
    a multiply by Hacker's Delight's magic number. `divRef` mirrors the
    emitted code for `TestDivideByConstant`. Float `%` runs `math.Mod`'s
    own steps on the floats' bits (`floatModAny`), or a few instructions
    for a power-of-two constant, and gives Go's NaN where `math.Mod` does.
    A zero divisor, a subnormal float divisor, a bitwise float operand,
    and `<` between a float and an integer beyond 2^53 exit to Go.
    Integer for loops count down in the limit's register,
    as `forPrep` sets them up; a float limit with an integer start exits
    at FORPREP. `<` and `<=` compare numbers; `==` compares any values, a
    float and an integer exactly (`floatEqualsInt`), and
    exits only for two userdata, two tables whose first metatable is not
    known to lack `__eq`, or equal-length strings longer than
    `maxInlineCompare`.
  - Upvalues.
  - Fields through the field caches, including `__index` tables two
    deep, own fields holding nil, and string methods through the string
    metatable (whose cache `jitStep` fills), and array elements,
    including appends within capacity, of tables in registers or
    upvalues.
  - `#` of strings, buffers and tables without `__len` or a hash part:
    the array's length, or a border by binary search (`tableLength`).
  - Native calls, tail calls and returns between compiled Lua functions,
    with arguments up to l.top (B 0) too. A vararg callee's frame starts
    above its arguments, where `adjustVarArgs` moves its fixed parameters,
    as preCall does; one whose named vararg table must be made exits.
    VARARG copies the extra arguments from below the frame (`varArg`),
    growing the frame for all of them within the stack. A tail call
    replaces the frame as the interpreter's does (`tailCallLua`): the
    callee moves to the caller's function slot, below a vararg caller's
    extra arguments, and its frame starts after it. A function with
    nested functions returns and tail calls natively too, unless an
    upvalue over its frame is still open (`exitIfUpValuesOpen`); then it
    exits, for Go to close it.
  - SETLIST of a fixed count into the array NEWTABLE sized
    (`setList`), and nil for an integer key past the array of a table
    with no hash part or metatable.
  - Buffers' elements (see Buffers): GETTABLE and SETTABLE branch out of
    line (`outOfLine`, emitted with the stubs, as kernels' side exits are)
    when the object is not a table, so table code pays one untaken
    branch. A float's bits go to and from memory through general
    registers, or straight from memory into an SSE register: moving them
    between the register files just before the store measured three
    times slower on amd64, fed by sin.
  - `math.sqrt`, `sin` and `cos` inline, as number functions. `floor`,
    `ceil`, `abs`, `min` and `max` keep integers integers, so are Go
    functions, not number functions: `lua` defines them (`MathFloor` and
    the rest, math_functions.go), stdlib registers them, and compiled
    code tells them by their code pointer (jit_math.go) in `goCallee`'s
    path for Go functions, where Lua-to-Lua calls never go.
  - **Generic for** the same way: `pairs`, `ipairs`, `next` and ipairs'
    iterator are `BasePairs` and the rest (base_functions.go). A call of
    `pairs` or `ipairs` stores its results inline (`pairsCall`), and
    TFORCALL steps through an array part itself (`tforCall`), exiting
    where string keys or a hash part follow, or for `__index`. The JMP
    that closes the loop jumps when nothing at or above it is open
    (`closeJump`).
  - **`setmetatable`** too (`BaseSetMetatable`), called or tail called,
    of a table that has no metatable yet, to one whose shape has never
    held `__gc` or `__mode` (`shape.collects`, set as keys are added), so
    the collector needs to hear of nothing (`setMetaTable`); that includes
    the fresh `{__index = C}` many constructors make. A tail call then
    returns the table (`tailSetMeta`).
- **Kernels:** an innermost numeric for loop whose body is only moves,
  number constants, arithmetic (`%` and `//` by a nonzero integer
  constant), number comparisons, and the intrinsic calls and buffer
  accesses below keeps every Lua register it uses in a machine register
  (`emitKernel`): integers in general-purpose registers, floats in FP
  registers. A loop gets an
  integer and a float kernel where both type; each checks its live-in
  types on entry and falls through to the next, then to ordinary code,
  which comes back to the check every iteration, so a kernel whose check
  keeps failing slows the ordinary loop. `jitContext.kernels` counts
  entries by kind, for `TestJITKernels`.
  - **Types per pc.** `planKernel` types each register before each body
    pc (`at`), because Lua reuses a temporary for an integer and then a
    float; a register gets a machine register for each type it takes
    (`slots`, `allocate`). The loop's registers, live-in ones and locals
    below the loop keep theirs throughout; the body's temporaries share by
    liveness, so a result may take the register of an operand it reads
    last, which every kernel emitter must allow by reading its operands
    before writing its result. amd64 has 7 integer registers, and an
    integer loop takes 3 of them: the loop variable shares the index's,
    as the body cannot write it. A live-in register the body writes starts with the type it
    ends with; one nothing decides takes `guess`'s type, a float only
    when it meets floats more than integers (a time parameter), since a
    wrong guess costs the kernel.
  - **Intrinsic calls.** `GETUPVAL f; …arithmetic, buffer reads and
    other intrinsic calls…; CALL f B C` (a side exit in between stores `f`
    first, `calleesAt`), where the closure being compiled holds
    `math.sqrt`, `sin`, `cos`, `floor`, `ceil`, `abs`, `min` or `max` in
    that upvalue, compiles inline; the entry check confirms the upvalue
    still holds it. A nested call, `min(255, floor(x))`, is `CALL … C 0`
    then `CALL … B 0`: an intrinsic has one result, so `kernelCall.args`
    fixes the count, and a side exit at a `B 0` call sets `l.top`. The kernel saves the integer registers trig uses around it
    (`intrinsicSaved`, spilled to `jitContext.spill`).
  - **Buffers.** `GETTABLE`/`SETTABLE` on a register the body never
    writes, with an integer key, reads or writes a buffer's element; the
    entry check confirms a buffer (of floats, if read). A register the
    function puts a new table in is never taken for one.
  - **Upvalues.** Numbers and buffers in upvalues (GETUPVAL, GETTABUP,
    SETTABUP) are checked on entry and kept in `jitContext.hoist`
    (`hoisted`, at most `maxHoisted`). A register holding such a buffer
    is typed as an alias of it and written back only on the way out.
  - **Side exits.** A key out of range, a float for an integer buffer or
    an argument trig leaves to Go leaves the kernel mid-body
    (`kernelSideExit`): it writes back the registers defined at that pc
    and jumps to the ordinary code for the instruction. Locals of the
    enclosing function the body writes are live-in when a kernel can side
    exit, so they hold the last iteration's values there: an error may
    close upvalues over them.
  - **Integer to float on amd64** goes through `toFloat`, which zeroes the
    destination first: CVTSI2SD keeps the rest of the register, so waits
    for its last writer, and in plasma that chained each sin to the one
    before, taking the kernel from 510 µs to 611 rather than 249.

### Rules compiled code depends on

- **Check before write:** each instruction checks everything before it
  writes anything, so any failed check can exit at that instruction and
  let Go run it from the start.
- **Write barrier:** `runtime.writeBarrier` is read once on entry. It only
  changes while the world is stopped, and a goroutine in generated code
  cannot be stopped. While it is on, compiled code stores only nil,
  numbers and booleans over scalar values. Native calls, returns and
  kernels exit instead.
- **Budget:** loops and native calls spend `jitBudget` (65536). When it
  runs out, compiled code returns to Go, which calls `runtime.Gosched`, so
  the GC can stop the world. Any new backward branch or call path must
  spend budget.
- **Interrupt:** `State.Interrupt` sets an atomic flag from any goroutine.
  The interpreter checks it every `interruptPoll` (1024) loop iterations
  and tail calls (`pollInterrupt`: backward jumps, FORLOOP, TFORLOOP,
  TAILCALL); compiled code is checked at the budget exit and on every
  1024th entry (`enterJIT`), since a loop that leaves compiled code every
  iteration re-enters with a full budget. A new kind of loop must reach
  one of these. An atomic load on every iteration measured 6% slower on
  an interpreted numeric loop, and any check inside `runJIT`'s loop 12–18%
  slower on calls into Go; the paced checks cost 1–5% on interpreted loops
  and nothing measurable with the JIT.
- **No calls into Go:** generated code never calls Go and never uses the
  Go stack; Go's unwinder cannot see its frames. Anything needing Go
  exits.
- **Registers:** on arm64 generated code keeps R18, R26–R30 and SP; on
  amd64 SP, BP, R14 and X15 (internal/jit/call).
- **Bit-exact intrinsics:** they must match Go exactly. `sin` and `cos`
  copy the code Go 1.27 compiles for math/sin.go, including which
  multiply-adds it fuses on arm64. On amd64 that means no fusion below
  GOAMD64=v3; at v3 they are left to Go. `TestJITTrigMatchesGo` fails if a
  Go release changes this. Their constants live in `trigTable`
  (jit_trig.go), which compiled code reads as memory operands on amd64
  and with one LDR each on arm64; where a constant comes from does not
  change the rounding.

### Testing the JIT

- `runBoth` in jit_test.go runs a script with and without the JIT and
  compares the results; add cases there for new instructions.
- `TestJITKernels` and `TestJITKernelCallsAndBuffers` assert which kinds
  of kernel ran (`jitContext.kernels`), so a loop that silently stops
  being a kernel fails. Where `trigInline` is false, loops calling `sin`
  or `cos` are expected not to be kernels.
- To prove a test exercises generated code, break the generated code on
  purpose (for example, emit a subtract for ADD) and check the tests fail.
  A break that hangs rather than fails counts as caught; kill the test
  binary afterwards.
- `TestJITLargeFunction` compiles past arm64's 32 KB test-branch reach;
  run it with `GOARCH=arm64` after changing branches or code size there.

## Gaps with C Lua 5.5 and LuaJIT

What apogee does not do, and why, as of September 2026. Keep this list
current: remove an entry when a change closes it, and add one when a
change opens a difference.

### C Lua 5.5

The language and standard library are complete: the 5.5.1 suite passes
apart from the files that need C Lua's internal test library (T). What
remains follows from running on Go.

- **Binary chunks are apogee's own format** (internal/chunk). `luac`
  output does not load, and `string.dump` output does not load in C Lua.
  `calls.lua` is pending for this alone: it checks C's chunk header.
- **No C modules.** There is no C API, so `package.loadlib` and binary
  `require` searchers cannot work; Lua and Go modules can.
- **Only the C locale.** `os.setlocale` accepts `"C"`, `"POSIX"` and
  `""`, and fails otherwise.
- **The collector** (see Garbage collection):
  - Go frees memory; a Lua collection marks the whole Lua heap at once,
    for weak tables and `__gc`.
  - "incremental" and "generational" are remembered and reported;
    "step" in generational mode collects.
  - Of the "param" parameters, only "pause" paces collections.
  - `collectgarbage("count")` is Go's heap.
  - An object only Go memory refers to, outside the registry and the
    stacks, counts as garbage.
  - `__mode` and `__gc` are noticed when the metatable is set.
- **Out of memory aborts the process** unless the host set an allocation
  limit: Go aborts rather than raising "not enough memory", so
  `table.create` caps what it preallocates. The limit counts allocation,
  not memory in use (see Allocation limit), so heavy.lua and memerr.lua
  stay skipped.
- **Smaller visible differences:**
  - Debug information calls Go functions "Go" unless
    `APOGEE_GO_AS_C=1` is set.
  - A numeric `for` has one more hidden variable than 5.5's, which
    `debug.getlocal` shows.
  - `^` uses Go's `math.Pow`, which can differ from C's `pow` in the
    last bit.
- **The JIT compiles only on linux and darwin, arm64 and amd64.**
  Elsewhere, Windows included, states interpret.
- **Speed.** With the JIT, apogee takes 0.69 times C Lua 5.5's time on
  the standard benchmarks on the 9900X3D; without it, 1.8 times. It is
  faster on all 17, least on CD (0.86 times) and Json (0.90). The M1's results are
  still against 5.4; re-measure there with `-tags clua55`
  (bench/README.md, "Reproducing").

### LuaJIT

**Speed is the main gap.** On the standard benchmarks on the 9900X3D,
LuaJIT takes 0.18 times C Lua 5.5's time, against apogee's 0.69: roughly
four times faster.
- Numeric loops: 5–16 times faster (spectral-norm, Permute, NBody,
  Towers).
- Object-heavy code: 1.2–2.6 times faster (Havlak, DeltaBlue, Richards).
- On the embedding workloads LuaJIT takes 2.3 times native Go's time,
  apogee 5.8; with a buffer, apogee's plasma beats both.
- On the M1, Homebrew's LuaJIT is slower than apogee on CD (329 ms
  against 58) and Json (19 ms against 6); on amd64 it is faster on both.

The gap is architectural:
- LuaJIT is a tracing JIT. It keeps values unboxed in registers across
  a trace, hoists loop invariants, sinks allocations, and calls within
  compiled code.
- apogee's JIT compiles a function at a time on the interpreter's own
  frames. Every value is a 16-byte stack slot. Kernels (simple numeric
  loops) are the only code that keeps values in registers.
- apogee's compiled code exits to Go for:
  - calls into Go, including a tail call of one, other than the math
    functions, `pairs`, `ipairs` and `setmetatable` of a new table;
  - NEWTABLE and CLOSURE;
  - CONCAT, and LEN of a table with a hash part or `__len`;
  - GETTABLE and SETTABLE with keys other than constant strings and
    array indices;
  - the generic for's TFORCALL, except `ipairs` and `pairs` over an
    array part.

Closing most of the speed gap means a tracing or register-allocating
JIT: a project, not tuning. The cheaper steps are in Next below (exits,
more in kernels, registers across ordinary code).

**Language and libraries favour apogee.** LuaJIT is Lua 5.1 with a few
5.2 extensions. apogee has what it lacks:
- an integer subtype, bitwise operators and `//`;
- `<const>`, `<close>` and `global`;
- `utf8`, `string.pack`, `table.move` and `table.create`;
- 5.5's metamethod rules.

LuaJIT has what apogee lacks:
- the FFI (C types and calls); apogee's buffers cover its typed arrays
  of numbers, not structs or calls;
- the `bit` and `jit.*` modules;
- `string.buffer` and `table.new`.

**Embedding favours apogee.** It is pure Go with no cgo, it
cross-compiles freely, and its Go API is typed, with generics. LuaJIT
needs cgo, which `CGO_ENABLED=0` builds such as EncomPlayer's rule out.

**Platforms favour LuaJIT.** Its JIT covers more CPU architectures, and
Windows. apogee's JIT covers linux and darwin on arm64 and amd64.

## Performance today

bench/README.md has the current tables and charts, generated from the raw
results: AMD Ryzen 9 9900X3D (linux/amd64) and Apple M1 Pro (arm64). On
the standard benchmarks (Are We Fast Yet and three from the Benchmarks
Game) apogee with the JIT takes 0.69 times as long as C Lua 5.5 on amd64
(pinned to one CCD), and 1.8 times without it. The M1's results
(0.75 and 1.5 times) are still against C Lua 5.4, from before the port to
5.5, and before the arm64 fix that compiles functions past 32 KB of code.

The slowdowns after the port to 5.5 are explained. The numeric loop's
`(i*i) % 7` became an integer division each iteration, now a multiply
(#109: 1.53 to 0.79 ms). Closures' 7.9 ms was an unpinned run: pinned,
the same commit takes 4.7. Interpreted, the numeric loop is still 11.6 ms
against 9.2 before the port; the same integer division is the likely
cause, not yet confirmed.

To find where a workload leaves compiled code, count exits: log
`p.jitOrig[ip]` and `l.jitCtx.reason` after each `enterJIT` in `runJIT`
for one call of the workload. For field instructions, log the state of
`p.fields[ip]` too (own slot, `__index` table, chain length), which says
why the cache missed. An exit every iteration costs 10 ns or more; that
is how `GETTABUP` with an array index was found to exit on each of
particles' 2,000 iterations, and how every Are We Fast Yet class method
was found to miss the cache.

An interpreted result that moves by 10–25% after a change the
interpreter does not run is almost always `executeSwitch` moving: Json
and the string build swing that much with its address alone. Rebuild
both binaries with `-ldflags=-funcalign=128`, which moves it again, before
believing it.

The JIT gains least where a script crosses between compiled code and Go
every few instructions. A bare `call.Call` round trip costs about 2 ns
(`BenchmarkCallRet`). On the 9900X3D a call into Go from compiled code
now costs about 12.5 ns in all, against 17.3 ns interpreted; about 4.5
ns of that is compiled code around the call.

On amd64 most of plasma's JIT time is in compiled code, not in calls to
`set`: every Lua register lives in memory, and each store re-reads the
barrier flag from the context. Given the canvas as a buffer instead, its
inner loop runs as a kernel, in registers with `sin` inline, and takes
0.25 ms to native Go's 0.30.

## Recommended next steps

### Where the time goes now

Measured on the 9900X3D with the JIT on (bench/README.md):

- Exits and the allocation behind many of them are most of what is left.
  A call into Go costs about 12.5 ns, of
  which about 4.5 ns is compiled code around it, and the rest is the
  API's Go frame and the round trip. The other exits left on every
  iteration are allocations (`NEWTABLE`, `CLOSURE`), tail calls of Go
  functions other than `setmetatable`,
  GETTABLE and SETTABLE with keys that are not constant
  strings or array indices, CONCAT, and the sort comparator's return to
  Go.
- CD, the standard benchmark closest to C Lua 5.5 (0.86 times), spent
  about a fifth of its time in `jitStep` before constructors' setmetatable
  compiled, most of that creating tables
  (`newTableAt`) and storing into their hash parts (`setTableAt`): the
  exit itself is the smaller part.
  binary-trees and Havlak are likewise allocation-bound.
- array-fill-sum takes about twice C Lua's time, mostly growing the array:
  Go's collector scanning and its write barriers while copying, not
  compiled code (about 13% of its time).
- Plasma calling `set` takes about 38 ns a pixel against Go's 15, mostly
  the call to `set` and ordinary compiled code, which keeps every Lua
  register in memory. With a buffer it is a kernel at 12.5 ns.
- fib, records and particles are limited by ordinary compiled code:
  values in memory, a type check on every read and a barrier check on
  every store.
- Closures and records are limited by allocation, one object per
  iteration, as Lua requires.

### Done in the last rounds

Measured against the standard benchmarks, which found most of them.
Newest first, the rounds after the 5.5 port (#109–#116), each found by
counting exits or profiling:

- `%` and `//` by a constant without a divide instruction (#109; numeric
  loop −47%).
- A backward JMP after TEST or TESTSET, as repeat-until compiles, branches
  in compiled code instead of exiting (#110; fannkuch −40%).
- SETLIST of a fixed count, and nil past the array of a table without a
  hash part (#111; binary-trees −27%).
- Tail calls between compiled Lua functions replace the frame in compiled
  code (#112; Richards −22%, CD −12%).
- Buffers (#113; plasma writing into one instead of calling `set`, −43%),
  and functions with nested functions returning natively unless an
  upvalue is open (#114; CD and Richards −2%).
- Kernels with intrinsic calls, buffers, side exits and per-pc types, and
  `toFloat` (#115; plasma into a buffer −51%, to below native Go; plasma
  calling `set` −13%; numeric loop −5%).
- arm64 functions past 32 KB of code compile (#116): four of CD's and
  Havlak's hottest functions were interpreted there. Unmeasured on
  hardware.

Earlier:

- Loop heads count toward compiling (#70): a function run once with a
  hot `while` loop never compiled before (Mandelbrot −53%).
- `__index` chains in the field cache (#71), own fields holding nil and
  a second chain level in compiled reads (#74): class hierarchies built
  from metatables, as Are We Fast Yet's are, missed the cache at almost
  every method call (Richards −35%, List −38%, DeltaBlue −17%).
- `==` for every kind of value in compiled code (#72): comparisons with
  nil, strings and objects exited (Json −25%, Richards −15%).
- Field stores of nil, and into nil slots of tables whose metatable lacks
  `__newindex` (#73; Towers −28%).
- String length and string methods in compiled code (#76; Json −22%), and
  the API's argument fast paths (#77; string scan −14%).
- After the 5.5 port (#101): `+ - * /` of an integer and a float
  without `FloatArith`'s dispatch; a raw array read first in `FieldInt`,
  which the ported ltablib uses; and a generic for's TBC with a nil
  closing value and RETURN in functions that have one, in compiled code
  (`jitContext.tbc`). Before, such functions exited at every RETURN.
  Bounce −8%, CD and binary-trees −5% with the JIT.
- Before these: calls into Go through `jitExitCallGo` and
  `jitExitCallNumber` (16.6 to 12.5 ns), Go calling compiled Lua
  (`callJIT`, `jitReturnToGo`; sort 5.6 to 3.6 ms), `sin` and `cos`
  constants from memory, and array indexing of tables in upvalues.

### Next

In order of expected payoff for real-time scripts such as visualisers:

1. **Fewer, cheaper exits.** Tail calls between compiled Lua functions
   no longer exit (#112), nor do constructors' `return
   setmetatable(obj, mt)` (`tailSetMeta`) or CALL with a variable number
   of results (#143).
   Allocation itself stays in Go, so NEWTABLE and CLOSURE exits can only
   get cheaper, and in CD most of their cost is the allocation.
2. **More in kernels.** Kernels call intrinsics and index buffers; a Go
   or number function could be called by writing the kernel's registers
   back, exiting, and re-entering after the call, and table arrays read
   with a type check per element (array-fill-sum takes about twice C
   Lua's time, though most of it is growing the array, in Go) and an
   adaptive switch-off when that check keeps failing.
3. **Registers across ordinary code.** Keep numbers in FP registers
   across straight-line code between exits, not only in kernels, with
   type checks at the first use. This is the lever for fib, records and
   particles.
4. **Short functions called from Go.** A comparator is now interpreted
   (above), which on the M1 made sort 13% faster with the JIT but only
   level with the interpreter. Compiled code could run the RETURN of the frame
   `callJIT` entered itself, with a call status bit on that frame cleared
   when the interpreter takes over, to make the crossing cheap enough to
   enter again. Replacing `jitReturnToGo`'s `postCall` with an inline
   copy measured no gain; the rest of the crossing is spread across
   `call`, `preCall`, `pushLuaFrame` and `enterJIT`.
5. **Closures and GC.** Shrink `luaClosure`, or create closures in
   compiled code from a pre-allocated pool.
6. **More native instructions:** `pairs` over string keys (slots);
   CONCAT into a reusable buffer; indexing a vararg view; GETTABLE and
   SETTABLE with string keys in registers.

7. **The barrier and budget in registers on amd64**, where they live in
   the context: unmeasured.
8. **The interpreter's placement.** Interpreted Json and string building
   vary by 20% with the address of `executeSwitch` alone; a layout that
   is good wherever the linker puts it would help every build without the
   JIT, Windows included.

Each of these is worth a few percent on one or two benchmarks, not more:
after #116, profiling found no single cause left as large as those the
rounds above removed.

Measured and not worth it for now: loop-invariant global loads (plasma
runs the same with `set` global or local), putting `sin` and `cos`
first in the intrinsic dispatch (0.9% on plasma, at a cost to every other
intrinsic), and running TAILCALL in `runJIT` rather than the interpreter
(no change: the exit is the cost). Closing JMPs in compiled code once
measured closures +20%; as compiled since #149 (`closeJump`) they measure
no change (4.84 against 4.92 ms, p=0.44).

- **Inline integer-times-float-constant branches in `executeSwitch`'s
  RK opcodes:** plasma −7% interpreted, but others +1–2.5% from layout;
  −0.1% net.

The go-calls workload's JIT time swings 20–30% with whether its argument
and result are integers or floats, in no consistent direction. It looks
micro-architectural (store forwarding, placement), not a code path in
apogee.

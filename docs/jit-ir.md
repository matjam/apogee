# An IR tier for the JIT

A design for the optimizing tier #140 asked for: instead of adding a case
to the kernel planner for each shape of loop, compile hot loops through a
typed intermediate representation, with one lowering per architecture.
Each phase below is a PR, measured; **Progress** says what each did.

## Where the JIT is

Two compilers share each function today (AGENTS.md, JIT):

- **Templates** (jit_amd64.go, jit_arm64.go and their `_calls`,
  `_tables`, `_integer` files): one sequence of machine code per bytecode
  instruction, on the interpreter's own frame. Every Lua register lives in
  its 16-byte stack slot; each read checks the type, each store checks the
  write barrier. Anything they cannot do exits, and Go runs the
  instruction (`jitStep`) or the interpreter takes over.
- **Kernels** (jit_kernel.go, jit_*_kernel.go): an innermost numeric for
  loop whose body fits a fixed list of instructions runs with its numbers
  in machine registers. The planner types each register per pc, allocates
  registers by liveness, and emits each instruction directly. A body with
  anything else is not a kernel at all, and a loop that needs one more
  register than the machine has is rejected.

Kernels are fast where they apply (plasma into a buffer beats native Go)
but brittle: #140 was three planner cases in a row, each found because a
common idiom fell off the list. The fuzzer (TestJITFuzz) now finds such
cliffs, but each fix is still a case.

## The design

Keep the templates as the **baseline and exit tier**: they run any
instruction, and every pc has code. Add an **IR tier** for hot loops that
replaces kernels:

1. **Region.** A loop nest, from its FORLOOP or back-edge target, as the
   kernel planner finds innermost loops today. Later phases grow it.
2. **IR.** SSA over typed values, built from the bytecode in one pass
   (Braun et al.'s on-the-fly SSA; loop bodies have only forward jumps
   and the back edge). Types: int, float, bool, and boxed value; table,
   buffer and function references once guarded. Each operation is
   explicit: `load slot`, `guard int`, `add.int`, `array load`, `call
   intrinsic`, `store slot`, and so on.
3. **Types from the frame, checked by guards.** Instead of the planner's
   inference and guesses, the region's entry reads the registers it uses
   and guards their types, as kernels' entry checks do; types inside
   follow from the operations. A guard that fails leaves the region.
4. **Leaving is a snapshot.** Every guard and every instruction the IR
   does not lower carries the Lua registers live there, as SSA values.
   Leaving writes them back to their stack slots (boxing numbers) and
   jumps to that pc's template code, which is the kernels' side exit made
   general. An unsupported instruction therefore costs one exit where it
   runs, not the whole loop: the next iteration re-enters the region at
   the loop head, as kernels are re-entered now. This is option 3 from
   #140, for free.
5. **Optimisation,** each small and separately testable: constant
   folding, removing redundant guards (a register's type is known after
   its first guard or its defining operation), hoisting loop-invariant
   loads and guards (upvalues, a buffer's pointer and length, a table's
   shape), dead code, and unboxing across the loop (a float stays in an
   FP register, as in kernels).
6. **Register allocation.** Linear scan over the region, the kernels'
   liveness allocation made general. A value that does not get a
   register spills to its Lua register's own stack slot, which is where
   a snapshot writes it anyway: running out of registers slows a loop
   instead of rejecting it.
7. **Lowering** per architecture, reusing the encoders, the constant
   division plans, the bit-exact intrinsics (floor, min, sin, fmod...) and
   the table and buffer access sequences the templates already have.

The fuzzer is the safety net: every phase runs it on both architectures,
with the kernel and IR paths compared against the interpreter, and adds
the idioms it makes fast to `jitMustNotExit`.

## Phases

1. **IR, allocation and lowering for what kernels do today**: numbers,
   buffers, intrinsic calls and upvalues, with snapshots for exits.
   Kernels' tests pass unchanged against the IR; the kernel planner and
   emitters are deleted. Measured on plasma, particles, the numeric loop
   and the standard benchmarks: no regression.
2. **Unsupported instructions exit locally** instead of rejecting the
   region, and values spill instead of failing allocation. Loops with a
   Go call, a CONCAT or a table allocation in them keep the rest in
   registers.
3. **Tables in the region**: array reads and writes with a type guard per
   element, and field reads and writes through the field caches with the
   shape guarded once, hoisted when the table is loop-invariant. This is
   what array-fill-sum, records and particles need.
4. **Lua calls in the region**: calls to compiled functions leave and
   re-enter at the next pc at first; inlining small leaf functions later.
5. **Wider regions**: loop nests, then whole functions between calls,
   which is what fib-shaped code needs ("registers across ordinary code",
   AGENTS.md, Next, item 3).

Each phase ships with a full benchmark run and no regression on the
geometric mean; a phase that loses somewhere says where and why.

## Progress

1. **Phase 1** (jit_ir.go, jit_*_kernel.go). Kernels compile through the
   IR: `buildIR` makes typed operations on virtual registers from the
   planner's typed body, with a snapshot for each pc an operation can
   leave at; `allocate` colours the virtual registers by liveness over
   the operations; `kernelInstruction` lowers each operation on each
   architecture. The per-bytecode emitters and the planner's own
   allocation are gone. Two things differ from the design above, for
   now: the virtual registers are one per Lua register and type, not
   SSA values, which is what the allocation over a loop body needs and
   keeps a snapshot's registers one-to-one with virtual ones; and types
   still come from the planner's inference (`planKernel`), which later
   phases replace with guards where they add instructions it cannot type.
   Every kernel compiles to the same size of code as before on both
   architectures, over the JIT tests and the benchmark suites.
2. **Phase 2.** An instruction the kernel cannot run leaves it where it
   runs (`irExit`, and a branch whose jump leaves the body), for the
   ordinary code to run it and the rest of the iteration, instead of
   rejecting the loop; one on every path still does. A kernel whose exits
   turn out common switches itself off (`kernelRuns`). Values that do not
   fit the registers spill to their stack slots. Loops with a call or a
   `break` on a rare path run 2.7 to 3.6 times as fast; the benchmark
   suites do not change, since their loops' tables and calls are on every
   path: that is phases 3 and 4.

## What this does not change

The interpreter, the templates and the driver (`runJIT`, `jitStep`)
stay as they are; the IR tier only adds entries at loop heads, as kernels
do. Buffers, the write-barrier rule, the budget and interrupts, and the
rule that generated code never calls Go all hold unchanged.

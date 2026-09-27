# Benchmarks

Two benchmarks run the same Lua source in every interpreter: apogee without
the JIT (`WithoutJIT`), apogee with it (the default), Shopify/go-lua, and C
Lua 5.5 or LuaJIT when built with their tag (see
[C interpreters](#c-interpreters)).

- **`BenchmarkSuite`** ([`suite_test.go`](suite_test.go)) runs eleven
  workloads chosen for what an embedded Lua does: calls, loops, tables,
  closures, sorting, building and scanning strings, and calls into Go.
  Each is also written in native Go, which the interpreters are compared
  with.
- **`BenchmarkStandard`** ([`standard_test.go`](standard_test.go)) runs the
  14 benchmarks of Are We Fast Yet and three from the Computer Language
  Benchmarks Game, which other language implementations are measured with
  ([`standard/`](standard)). The interpreters are compared with C Lua 5.5,
  the language apogee speaks.

`TestSuiteAgrees` and `TestStandardAgrees` check that every interpreter
computes the same results. The tables and charts come from the raw output
by [`chart`](chart) (see [Reproducing](#reproducing)).

## amd64

AMD Ryzen 9 9900X3D, linux, Go 1.27.1, `-count 6`,
`-ldflags=-funcalign=64`, medians, pinned to the six cores of one CCD
(`taskset -c 0-5`), 2026-09-27, at commit cbfab53. Rows move by up to
10% from one day's run to the next with nothing changed; compare
commits back to back, not across these files. Lua 5.5.1 and LuaJIT
2.1.1788460057 from Arch Linux's packages, measured on 2026-09-25 at commit
3d3b49e (C code the later commits do not touch). Raw output:
[`suite-results-amd64.txt`](suite-results-amd64.txt).

### The suite

![Each interpreter's time on each workload divided by native Go's, on amd64](suite-amd64.svg)

Each cell is the median time, and in brackets that time divided by native
Go's.

<!-- suite-table amd64 -->
| Workload | Native Go | Apogee (JIT) | Apogee (no JIT) | go-lua | Lua 5.5 | LuaJIT |
|---|---:|---:|---:|---:|---:|---:|
| fib(25), recursive calls | 0.22 ms | 1.79 ms (8.2×) | 6.05 ms (28×) | 9.45 ms (43×) | 2.36 ms (11×) | 0.27 ms (1.3×) |
| numeric loop, 1M iterations | 0.80 ms | 0.81 ms (1.0×) | 12.5 ms (16×) | 188 ms (236×) | 4.29 ms (5.4×) | 0.76 ms (0.96×) |
| array fill and sum, 100k | 0.52 ms | 1.08 ms (2.1×) | 2.88 ms (5.6×) | 6.28 ms (12×) | 0.58 ms (1.1×) | 0.22 ms (0.42×) |
| records, 10k tables | 0.10 ms | 0.66 ms (6.9×) | 0.95 ms (10×) | 2.80 ms (29×) | 0.77 ms (8.1×) | 0.28 ms (2.9×) |
| closures, 100k | 0.23 ms | 5.05 ms (22×) | 5.94 ms (26×) | 10.1 ms (45×) | 7.03 ms (31×) | 3.45 ms (15×) |
| sort 10k with comparator | 1.31 ms | 4.02 ms (3.1×) | 3.99 ms (3.1×) | 10.9 ms (8.3×) | 3.26 ms (2.5×) | 3.33 ms (2.5×) |
| string build, 10k pieces | 0.40 ms | 0.61 ms (1.5×) | 0.75 ms (1.9×) | 79.4 ms (200×) | 0.77 ms (1.9×) | 0.25 ms (0.63×) |
| string scan, 11k characters | 0.007 ms | 0.61 ms (90×) | 1.55 ms (229×) | 2.70 ms (398×) | 0.71 ms (105×) | 0.06 ms (8.7×) |
| calls into Go, 100k | 0.23 ms | 1.42 ms (6.3×) | 2.02 ms (9.0×) | 5.51 ms (24×) | 1.07 ms (4.7×) | 0.69 ms (3.1×) |
| plasma frame | 0.30 ms | 0.79 ms (2.6×) | 1.80 ms (5.9×) | 4.36 ms (14×) | 1.42 ms (4.7×) | 0.53 ms (1.8×) |
| particles frame | 0.005 ms | 0.09 ms (18×) | 0.31 ms (59×) | 1.24 ms (236×) | 0.15 ms (29×) | 0.03 ms (5.5×) |
| **geometric mean** |  | **5.9×** | **13×** | **53×** | **7.7×** | **2.3×** |
<!-- /suite-table -->

### The standard benchmarks

![Each interpreter's time on each standard benchmark divided by C Lua 5.5's, on amd64](standard-amd64.svg)

Each cell is the median time, and in brackets that time divided by C Lua
5.5's.

<!-- suite-table standard-amd64 -->
| Benchmark | Lua 5.5 | Apogee (JIT) | Apogee (no JIT) | go-lua | LuaJIT |
|---|---:|---:|---:|---:|---:|
| Bounce | 0.29 ms | 0.14 ms (0.47×) | 0.56 ms (1.9×) | 2.23 ms (7.6×) | 0.03 ms (0.10×) |
| CD | 35.1 ms | 29.1 ms (0.83×) | 45.9 ms (1.3×) | 155 ms (4.4×) | 14.3 ms (0.41×) |
| DeltaBlue | 21.1 ms | 19.3 ms (0.91×) | 32.6 ms (1.5×) | 1585 ms (75×) | 9.46 ms (0.45×) |
| Havlak | 2218 ms | 1524 ms (0.69×) | 2131 ms (0.96×) | – | 1458 ms (0.66×) |
| Json | 4.34 ms | 3.93 ms (0.90×) | 7.30 ms (1.7×) | 20.9 ms (4.8×) | 0.86 ms (0.20×) |
| List | 0.25 ms | 0.16 ms (0.64×) | 0.45 ms (1.8×) | 1.07 ms (4.4×) | 0.06 ms (0.25×) |
| Mandelbrot | 126 ms | 112 ms (0.89×) | 242 ms (1.9×) | 687 ms (5.4×) | 23.2 ms (0.18×) |
| NBody | 1.23 ms | 0.67 ms (0.54×) | 2.24 ms (1.8×) | 11.2 ms (9.0×) | 0.08 ms (0.06×) |
| Permute | 0.40 ms | 0.24 ms (0.60×) | 1.00 ms (2.5×) | 2.47 ms (6.2×) | 0.02 ms (0.04×) |
| Queens | 0.29 ms | 0.17 ms (0.59×) | 0.65 ms (2.3×) | 1.38 ms (4.8×) | 0.03 ms (0.12×) |
| Richards | 16.7 ms | 14.1 ms (0.85×) | 27.5 ms (1.6×) | 103 ms (6.2×) | 5.42 ms (0.32×) |
| Sieve | 0.10 ms | 0.08 ms (0.84×) | 0.32 ms (3.2×) | 0.62 ms (6.2×) | 0.02 ms (0.17×) |
| Storage | 0.76 ms | 0.56 ms (0.74×) | 0.89 ms (1.2×) | 3.15 ms (4.1×) | 0.33 ms (0.43×) |
| Towers | 0.82 ms | 0.52 ms (0.64×) | 1.57 ms (1.9×) | 4.27 ms (5.2×) | 0.10 ms (0.12×) |
| binary-trees | 115 ms | 103 ms (0.89×) | 174 ms (1.5×) | 226 ms (2.0×) | 34.5 ms (0.30×) |
| fannkuch-redux | 65.0 ms | 36.3 ms (0.56×) | 193 ms (3.0×) | 334 ms (5.1×) | 17.9 ms (0.28×) |
| spectral-norm | 33.6 ms | 20.6 ms (0.61×) | 80.4 ms (2.4×) | 175 ms (5.2×) | 1.27 ms (0.04×) |
| **geometric mean** |  | **0.70×** | **1.8×** | **6.1×** | **0.18×** |
<!-- /suite-table -->

- apogee with the JIT takes 0.70 times as long as C Lua 5.5 on the
  geometric mean, and is faster on all 17: most on numeric code and
  method calls (Bounce, NBody, Queens, fannkuch-redux, Permute and
  spectral-norm), least on CD, which allocates most (0.83 times), and
  Json (0.90).
- Without the JIT, apogee takes 1.8 times as long as C Lua 5.5.
- LuaJIT is about 5.5 times faster than C Lua 5.5 on the geometric mean.
- go-lua takes six times as long as C Lua 5.5, and 75 times on
  DeltaBlue. It does not finish Havlak in ten minutes, so it has no result
  there, and its geometric mean is of the other 16.

## arm64

Apple M1 Pro, macOS, Go 1.27.1, `-count 6`, `-ldflags=-funcalign=64`,
medians, 2026-09-25, at commit d5dba73. Lua 5.4.9 and LuaJIT 2.1.1788856981
from Homebrew (`lua@5.4`, `luajit`). These results predate the port to
Lua 5.5 and are still against C Lua 5.4. The machine was not idle, so
single results vary more than on amd64. Raw output:
[`suite-results.txt`](suite-results.txt).

### The suite

![Each interpreter's time on each workload divided by native Go's, on Apple M1 Pro](suite-arm64-m1.svg)

Each cell is the median time, and in brackets that time divided by native
Go's.

<!-- suite-table arm64-m1 -->
| Workload | Native Go | Apogee (JIT) | Apogee (no JIT) | go-lua | Lua 5.4 | LuaJIT |
|---|---:|---:|---:|---:|---:|---:|
| fib(25), recursive calls | 0.26 ms | 2.80 ms (11×) | 8.34 ms (32×) | 14.2 ms (55×) | 4.13 ms (16×) | 0.51 ms (2.0×) |
| numeric loop, 1M iterations | 1.18 ms | 1.79 ms (1.5×) | 14.6 ms (12×) | 291 ms (247×) | 11.5 ms (9.7×) | 1.06 ms (0.89×) |
| array fill and sum, 100k | 0.60 ms | 1.63 ms (2.7×) | 4.39 ms (7.3×) | 8.89 ms (15×) | 1.22 ms (2.0×) | 0.39 ms (0.65×) |
| records, 10k tables | 0.13 ms | 1.08 ms (8.4×) | 1.39 ms (11×) | 4.27 ms (33×) | 1.33 ms (10×) | 0.42 ms (3.2×) |
| closures, 100k | 0.35 ms | 9.19 ms (26×) | 8.24 ms (23×) | 13.8 ms (39×) | 9.51 ms (27×) | 5.41 ms (15×) |
| sort 10k with comparator | 1.94 ms | 5.77 ms (3.0×) | 5.65 ms (2.9×) | 14.0 ms (7.2×) | 5.03 ms (2.6×) | 4.09 ms (2.1×) |
| string build, 10k pieces | 0.57 ms | 0.85 ms (1.5×) | 1.06 ms (1.8×) | 99.6 ms (174×) | 1.75 ms (3.1×) | 0.50 ms (0.88×) |
| string scan, 11k characters | 0.02 ms | 0.85 ms (42×) | 2.28 ms (113×) | 3.40 ms (168×) | 1.14 ms (57×) | 0.10 ms (5.1×) |
| calls into Go, 100k | 0.35 ms | 2.12 ms (6.1×) | 2.83 ms (8.1×) | 7.83 ms (23×) | 1.82 ms (5.2×) | 1.17 ms (3.4×) |
| plasma frame | 0.30 ms | 1.46 ms (4.8×) | 2.12 ms (7.1×) | 5.87 ms (19×) | 2.08 ms (6.9×) | 0.54 ms (1.8×) |
| particles frame | 0.006 ms | 0.18 ms (30×) | 0.38 ms (61×) | 1.63 ms (265×) | 0.26 ms (43×) | 0.06 ms (9.7×) |
| **geometric mean** |  | **6.9×** | **13×** | **51×** | **9.6×** | **2.6×** |
<!-- /suite-table -->

- Sort with a comparator runs about as fast with the JIT as without it.
  Go calls the comparator, which returns after two instructions, so it
  is interpreted: entering compiled code and returning to Go costs about
  10 ns here, more than the instructions. Entered anyway, it took 6.90
  ms against 5.76 ms.

### The standard benchmarks

![Each interpreter's time on each standard benchmark divided by C Lua 5.4's, on Apple M1 Pro](standard-arm64-m1.svg)

Each cell is the median time, and in brackets that time divided by C Lua
5.4's.

<!-- suite-table standard-arm64-m1 -->
| Benchmark | Lua 5.4 | Apogee (JIT) | Apogee (no JIT) | go-lua | LuaJIT |
|---|---:|---:|---:|---:|---:|
| Bounce | 0.51 ms | 0.35 ms (0.69×) | 0.88 ms (1.7×) | 3.00 ms (5.9×) | 0.09 ms (0.18×) |
| CD | 57.6 ms | 58.5 ms (1.0×) | 65.7 ms (1.1×) | 236 ms (4.1×) | 329 ms (5.7×) |
| DeltaBlue | 34.9 ms | 32.1 ms (0.92×) | 48.0 ms (1.4×) | 2505 ms (72×) | 15.3 ms (0.44×) |
| Havlak | 3042 ms | 2768 ms (0.91×) | 3195 ms (1.1×) | – | 1812 ms (0.60×) |
| Json | 7.26 ms | 6.33 ms (0.87×) | 10.6 ms (1.5×) | 27.7 ms (3.8×) | 19.3 ms (2.7×) |
| List | 0.41 ms | 0.29 ms (0.70×) | 0.67 ms (1.6×) | 1.53 ms (3.7×) | 0.08 ms (0.19×) |
| Mandelbrot | 291 ms | 182 ms (0.62×) | 369 ms (1.3×) | 1190 ms (4.1×) | 41.2 ms (0.14×) |
| NBody | 2.57 ms | 1.51 ms (0.59×) | 3.51 ms (1.4×) | 15.4 ms (6.0×) | 0.13 ms (0.05×) |
| Permute | 0.78 ms | 0.48 ms (0.62×) | 1.50 ms (1.9×) | 3.54 ms (4.6×) | 0.03 ms (0.04×) |
| Queens | 0.54 ms | 0.29 ms (0.54×) | 0.92 ms (1.7×) | 2.05 ms (3.8×) | 0.05 ms (0.10×) |
| Richards | 29.4 ms | 23.5 ms (0.80×) | 41.1 ms (1.4×) | 137 ms (4.6×) | 6.64 ms (0.23×) |
| Sieve | 0.18 ms | 0.16 ms (0.87×) | 0.47 ms (2.6×) | 0.96 ms (5.3×) | 0.02 ms (0.11×) |
| Storage | 1.48 ms | 1.07 ms (0.72×) | 1.36 ms (0.92×) | 4.46 ms (3.0×) | 0.49 ms (0.33×) |
| Towers | 1.31 ms | 1.05 ms (0.80×) | 2.20 ms (1.7×) | 6.04 ms (4.6×) | 0.08 ms (0.06×) |
| binary-trees | 191 ms | 223 ms (1.2×) | 245 ms (1.3×) | 327 ms (1.7×) | 51.3 ms (0.27×) |
| fannkuch-redux | 135 ms | 97.6 ms (0.72×) | 272 ms (2.0×) | 471 ms (3.5×) | 16.9 ms (0.12×) |
| spectral-norm | 63.7 ms | 33.7 ms (0.53×) | 97.5 ms (1.5×) | 257 ms (4.0×) | 2.00 ms (0.03×) |
| **geometric mean** |  | **0.75×** | **1.5×** | **4.8×** | **0.20×** |
<!-- /suite-table -->

- apogee with the JIT takes 0.75 times as long as C Lua 5.4 on the
  geometric mean, and is faster on 15 of the 17; CD takes as long, and
  binary-trees, which allocates most, 1.1 times as long.
- Without the JIT, apogee takes 1.5 times as long as C Lua 5.4.
- Relative to C Lua 5.4, both apogee and LuaJIT do better here than on
  amd64. The two machines' C Lua 5.4 are different builds (Homebrew and
  Arch Linux), so compare their ratios with care.

## Allocations

The JIT allocates exactly what the interpreter does. Allocations are per
run of each suite workload.

| Workload | apogee allocations | go-lua allocations |
|---|---|---|
| fib(25) | 0 | 318,000 |
| numeric loop | 0 | 4,857,000 |
| array fill and sum | 22 | 500,000 |
| records | 10,000 | 120,000 |
| closures | 100,000 | 900,000 |
| sort | 17 | 180,000 |
| string build | 10,000 | 80,000 |
| string scan | 0 | 71,000 |
| calls into Go | 0 | 400,000 |
| plasma frame | 0 | 280,000 |
| particles frame | 0 | 25,000 |

## Notes

- Plasma is a 200×100 per-pixel effect with three `math.sin` calls and one
  call into Go per pixel. Particles moves 2,000 particle tables by a method
  and draws them.
- `TestNumericFrameDoesNotAllocate` keeps numeric code and calls into Go
  allocation-free.
- Numbers, booleans and calls allocate nothing in apogee. The remaining
  allocations are the Lua objects the script creates, one per table,
  closure or string.
- go-lua's numeric loop is slow because its `%` fast path calls
  `math.Mod`, which is far slower than Lua's `a - floor(a/b)*b`.
- The string scan walks 11,000 characters with `s:byte(i)` and
  `s:sub(i, i)`: two calls into Go a character, which the native Go
  version replaces with indexing its compiler folds into a tight loop. It
  shows what calls into Go cost; apogee with the JIT runs it faster than C
  Lua 5.5. It is the suite's newest workload, so its geometric means are
  not comparable with those of earlier runs.
- go-lua's string build is quadratic: its `table.concat` appends with
  `s += str`. apogee's uses a `strings.Builder`.
- The JIT gains least where scripts cross between compiled code and Go
  every few instructions: creating closures, Go calling Lua (the sort
  comparator), and calls into Go. It is fastest on numeric loops and Lua
  calls.
- On amd64 most of plasma's time with the JIT is in compiled code, not in
  its calls to `set`: every Lua register lives in memory, and each store
  checks the write barrier flag in memory.
- `-ldflags=-funcalign=64` keeps the interpreter loop's alignment fixed;
  without it, unrelated changes move timings by 5–10%.
- The native Go versions keep Go's advantages, such as inlined method calls
  in particles, so the gap to Go is widest where Go inlines most.
- Plasma here registers `set` as an ordinary Go function. Registered as a
  number function, which compiled code calls without a Go frame, it takes
  0.74 ms with the JIT (`BenchmarkApogeeNumberFunction`), against 0.77 ms
  (`BenchmarkApogee`). Given the canvas as a buffer, the inner loop runs
  as a kernel, with the `sin` calls inline and its numbers in machine
  registers, and takes 0.25 ms (`BenchmarkApogeeBuffer`), faster than the
  0.30 ms of native Go (amd64, 2026-09-26).
- The C interpreters' calls into Go are calls to C functions, and their
  plasma draws into a C array; the suite's other workloads are the same
  Lua in every interpreter. Their results use integers where Lua 5.4 does,
  which the agreement tests compare as numbers.

## C interpreters

C Lua 5.5, C Lua 5.4 and LuaJIT are linked with cgo, so they need a C
compiler and their development packages, found with pkg-config (`lua5.5`,
`lua5.4` and `luajit`). All define the Lua C API, so a test binary can
link only one. Each has a build tag: `clua55`, `clua54` or `luajit`.
Without one, the benchmarks build with `CGO_ENABLED=0` and run apogee and
go-lua only. C Lua 5.5, the language apogee speaks, is the reference: the
charts compare with it when a results file has it, and with 5.4
otherwise. On macOS, Homebrew's `lua` is 5.5; for 5.4 install `lua@5.4`,
which is keg-only, and set
`PKG_CONFIG_PATH=$(brew --prefix lua@5.4)/lib/pkgconfig`.

## Reproducing

From this directory, on an idle machine, run apogee, go-lua and native Go,
then each C interpreter, into one file:

```sh
CGO_ENABLED=0 go test -run x -bench . -benchmem -count 6 -timeout 3h -ldflags=-funcalign=64 > suite-results-amd64.txt
go test -tags clua55 -run x -bench '.*/.*/lua55$' -benchmem -count 6 -timeout 3h >> suite-results-amd64.txt
go test -tags luajit -run x -bench '.*/.*/luajit$' -benchmem -count 6 -timeout 3h >> suite-results-amd64.txt
go run ./chart -svg suite-amd64.svg -readme README.md -name amd64 suite-results-amd64.txt
go run ./chart -suite standard -svg standard-amd64.svg -readme README.md -name standard-amd64 suite-results-amd64.txt
```

From the median of each benchmark in the file, `-svg` redraws the chart
and `-readme` rewrites the table between `<!-- suite-table NAME -->` and
`<!-- /suite-table -->` in each file; `-table` prints it instead. Name the
file, charts and tables for the machine. On Apple M1 the file is
`suite-results.txt`, and the charts and tables are only in this README:

```sh
go run ./chart -svg suite-arm64-m1.svg -readme README.md -name arm64-m1 suite-results.txt
go run ./chart -suite standard -svg standard-arm64-m1.svg -readme README.md -name standard-arm64-m1 suite-results.txt
```

After either machine's run, `-summary` rewrites the root README's table
of geometric means from both files, a row per benchmark and machine:

```sh
go run ./chart -summary -readme ../README.md -name summary suite-results-amd64.txt suite-results.txt
```

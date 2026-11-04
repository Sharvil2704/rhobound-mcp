# Building and testing

rhobound is one Go program with no dependency outside the Go standard library. This page shows how to build it, how
to run its tests, and what to do when something goes wrong. The commands and outputs below were run on Linux
(arm64) with Go 1.27.2, from the repository root.

## Requirements

- **Go 1.23 or later** (`go.mod` says `go 1.23`). Get it from <https://go.dev/dl/>.
- Nothing else to build: no C compiler, no other module, no network access during the build.
- Any operating system and architecture Go supports. It has been built and run on linux/arm64, and cross-built for
  linux/amd64, darwin/arm64, darwin/amd64 and windows/amd64 (those were built, not run).
- The tests have been run only with Go 1.27.2 on linux/arm64. `go vet` (whose `stdversion` check reports standard
  library calls newer than the `go 1.23` line) passes, but no Go 1.23 toolchain and no amd64 machine has run them;
  the CI below is the first to do so.
- For the race detector only: cgo, which means a C compiler (`gcc` or `clang`) and `CGO_ENABLED=1`.
- For the Kubernetes end-to-end test only: `docker`, `kind` and `kubectl`.

Check your Go:

```
$ go version
go version go1.27.2 linux/arm64
```

## Get the source

```
git clone https://github.com/Sharvil2704/rhobound-mcp.git
cd rhobound-mcp
```

(Not run here: this machine had no network access when this page was written.)

## Build

```
$ go build -o bin/rhobound ./cmd/rhobound
$ bin/rhobound version
rhobound 0.1.0-dev
```

The binary is `bin/rhobound` (`bin/` is ignored by git). It does not need cgo, so it is a static binary:

```
$ file bin/rhobound
bin/rhobound: ELF 64-bit LSB executable, ARM aarch64, version 1 (SYSV), statically linked, Go BuildID=YKRsD4ejlNC18DW2GKIz/hHePPiyAbelv929FPHD-/ua7Xmiwc06LxTRZIIKvU/VP9gUijI5zTguE7XElZX, BuildID[sha1]=d4a0e98c935c914e43999416f1666e5054f42ff5, with debug_info, not stripped
```

### With version information

The version string is a variable set at link time:

```
$ go build -ldflags "-X main.Version=0.1.0" -o bin/rhobound ./cmd/rhobound
$ bin/rhobound version
rhobound 0.1.0
```

Fit and A/B records hold the version, so it follows their rule: 1 to 64 letters, digits and `. _ + ~ -`, the first a
letter or a digit (`0.1.0`, `1.2.3+build.7`, a 40- or 64-digit commit id). Every build refuses to read a record whose
version breaks the rule (`fit.version`), so a build stamped with such a version (`ship:deadbeef`, a URL, a 65-character
id) refuses to run `fit` and `diff-fit`, before it reads the capture, and says the rule.

`rhobound version --json` adds what the go command recorded in the binary: the Go release, the architecture and,
when it was built inside a git checkout, the source it was built from (`vcs`, absent otherwise; `modified` is true when
the checkout had uncommitted changes). Its schema is `docs/mcp/schemas/version.v1.json`:

```
$ bin/rhobound version --json
{"go":"go1.27.2","goarch":"arm64","name":"rhobound","vcs":{"modified":false,"revision":"<40 hex digits>","time":"<commit time, RFC 3339>"},"version":"0.1.0"}
```

For a release, build with the latest patch release of a supported Go (the `go` lines of `go.mod` and `mcp/go.mod` are
minimums, and later patch releases carry the standard library's security fixes, which the network-facing MCP server
needs), with `CGO_ENABLED=0`. `-trimpath` removes local paths from the binary, and `-s -w` drops the symbol table and
debug information (here 3.3 MB instead of about 5 MB). Two builds from one tree on one host, with `-trimpath`, gave the
same bytes; builds on other hosts were not compared:

```
go build -trimpath -ldflags "-s -w -X main.Version=0.1.0" -o bin/rhobound ./cmd/rhobound
```

### Cross-compiling

Set `GOOS` and `GOARCH`. `CGO_ENABLED=0` makes sure the binary is static:

```
$ CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.Version=0.1.0" -o dist/rhobound-linux-amd64 ./cmd/rhobound
$ CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.Version=0.1.0" -o dist/rhobound-linux-arm64 ./cmd/rhobound
$ CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.Version=0.1.0" -o dist/rhobound-darwin-arm64 ./cmd/rhobound
$ CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.Version=0.1.0" -o dist/rhobound-darwin-amd64 ./cmd/rhobound
$ CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.Version=0.1.0" -o dist/rhobound-windows-amd64.exe ./cmd/rhobound
$ file dist/*
dist/rhobound-darwin-amd64:      Mach-O 64-bit x86_64 executable, flags:<|DYLDLINK|PIE>
dist/rhobound-darwin-arm64:      Mach-O 64-bit arm64 executable, flags:<|DYLDLINK|PIE>
dist/rhobound-linux-amd64:       ELF 64-bit LSB executable, x86-64, version 1 (SYSV), statically linked, Go BuildID=l70P39FvX0JLL_Eism0x/cjOyFMZJPSKrwBxw0Wmf/6dn4AdFYf4HJDHyanJM-/MzZDwlPCQwRSU1H2eOOp, BuildID[sha1]=d3becd1b4e9aa3bab0bb52408a7d8defd3131173, stripped
dist/rhobound-linux-arm64:       ELF 64-bit LSB executable, ARM aarch64, version 1 (SYSV), statically linked, Go BuildID=o1Y08pfokQh0eN2-zuER/wzKTBYAyd6NqPS3UkvfO/bChK14g4MnncA52maCVJ/O0xnaqWah0r4p4jgOPFs, BuildID[sha1]=3da4fb286b9229768b3177b54f8a64e8e71de61a, stripped
dist/rhobound-windows-amd64.exe: PE32+ executable for MS Windows 10.00 (console), x86-64, 8 sections
$ dist/rhobound-linux-arm64 version
rhobound 0.1.0
```

(`dist/` is ignored by git. On macOS a binary always links the system library, which is what `DYLDLINK` says; it
needs nothing else.)

### Install with `go install`

From a clone, `go install ./cmd/rhobound` puts the binary in `$(go env GOPATH)/bin`, or in `GOBIN` when that is set
(here a folder of the repository, to show it):

```
$ GOBIN=$PWD/dist go install ./cmd/rhobound
$ dist/rhobound version
rhobound 0.1.0-dev
```

Without a clone, once the repository is public:

```
go install github.com/Sharvil2704/rhobound-cli/cmd/rhobound@latest
```

(Not run here: it needs network access. It builds without `-ldflags`, so `rhobound version` prints `0.1.0-dev`.)

## The Makefile

The Makefile is a shortcut for the `go` commands. `make` is not needed; every target has a plain equivalent:

| target | the go command | what it does |
|---|---|---|
| `make build` | `go build -o bin/rhobound ./cmd/rhobound` | builds the binary |
| `make test` | `go test ./...` | unit, golden, simulator and CLI tests (about 35 s on three cores) |
| `make race` | `go test -race -timeout 30m ./...` | the same with the race detector (about 5-6 minutes on three cores) |
| `make vet` | `go vet ./... && go vet -tags e2e ./... && go vet -tags accuracy ./...` | static checks, with each build tag |
| `make cover` | `go test -cover ./...` | the tests with coverage per package |
| `make fmt` | `gofmt -l -w .` | formats the code in place |
| `make accuracy` | `go test -tags accuracy -v -count=1 -timeout 60m ./internal/infer` | every accuracy study (prints tables, does not assert) |
| `make e2e` | `RHOBOUND_E2E=1 go test -tags e2e -v -count=1 -timeout 40m ./e2e` | the Kubernetes end-to-end test (needs docker, kind, kubectl) |
| `make mcp-build` | `cd mcp && go build -o ../bin/rhobound-mcp ./cmd/rhobound-mcp` | builds the MCP server (the nested module `mcp/`, Go 1.25 or later) |
| `make mcp-test` | `cd mcp && go vet ./... && go test ./...` | the server's tests; they build the CLI from this repository and run it |
| `make contract` | `go run ./tools/gencontract docs` | regenerates the code tables of `docs/mcp/DATA.md` and `docs/mcp/data-contract.json` from the registry |

## Running the tests

The times below are from an 8-core arm64 machine with the tests limited to 3 cores (`GOMAXPROCS=3`) and a 2.5 GB
memory cap. On more cores they are shorter.

### Unit tests

```
$ go test -count=1 ./...
ok  	github.com/Sharvil2704/rhobound-cli/cmd/rhobound	0.965s
?   	github.com/Sharvil2704/rhobound-cli/e2e/demoapp	[no test files]
ok  	github.com/Sharvil2704/rhobound-cli/e2e/imagetar	0.002s
?   	github.com/Sharvil2704/rhobound-cli/e2e/loadgen	[no test files]
ok  	github.com/Sharvil2704/rhobound-cli/internal/infer	31.671s
ok  	github.com/Sharvil2704/rhobound-cli/internal/mathx	6.642s
ok  	github.com/Sharvil2704/rhobound-cli/internal/model	0.074s
ok  	github.com/Sharvil2704/rhobound-cli/internal/otlp	0.005s
ok  	github.com/Sharvil2704/rhobound-cli/internal/report	0.002s
ok  	github.com/Sharvil2704/rhobound-cli/validation/cluster/otel/otelcap	0.029s
```

`-count=1` makes Go run them again instead of printing a cached result. They cover: the CLI on the example capture
and on hand-made captures; the inference against a queue simulator whose truth is known; the model against closed
forms (M/M/1, M/G/1, M/M/2, Burke) and against golden files; the numerical kernels against high-precision reference
values; and the OTLP reader.

- `testdata/golden/golden.json` holds results of the reference implementation (an unpublished Python prototype)
  that the basic model must match: 7 hand-written and 60 random layered topologies at utilizations 0.3, 0.6 and
  0.85, each with a random service-time change (the basic model, Poisson arrivals). The script that made them needs
  the reference implementation and is not published.
- `testdata/numerics/` holds reference values for the numerical kernels (`internal/mathx` and Erlang C), from decimal
  arithmetic, scipy, adaptive quadrature and Monte Carlo. `gen_reference.py` (needs numpy and scipy) shows how.
- `internal/infer/testdata/` holds the reference implementation's outputs for the queue rebuild (on inputs that
  `TestDumpFIFOInputs` writes from simulated captures) and for the stage template. The scripts that made them are
  not published.
- `internal/infer/testdata/nested/` holds whole traces of the OpenTelemetry Demo (release 2.2.0) from a real cluster, cut down to
  the fields the reader uses: four patterns of nested spans (Next.js and flagd SERVER spans inside the HTTP or RPC
  instrumentation's, .NET's gRPC client CLIENT span around the HTTP client's) and a message consumer's receive and
  process spans, which are not merged. `nested_real_test.go` reads them.
- `internal/infer/testdata/counts/shipped-20s/` is 478 whole traces (6,049 spans) of the same Demo under its shipped load mix,
  the first 20 s of a 420 s window, cut down the same way (service and pod; ids, kind, times, name; the peer, port and route
  attributes; this set carries no instrumentation scope names, see its [README](../internal/infer/testdata/README.md)): the cart's cache calls 1, 2 or 3 per visit, checkout's one call per cart line, a front end of 11 call shapes.
  `counts_real_test.go` and the CLI test `TestVariableCallsInTheReport` read it. `counts_sim_test.go` is an
  event-driven simulator (one first-come, first-served queue per callee, variable call counts and call shapes) for the tests
  of calls per visit that vary; it is test code, and the rest of the simulator is in `sim_test.go`.
- `lost_test.go` (in `internal/infer`) tests the naming of lost spans on hand-made spans from the simulator, and on
  `internal/infer/testdata/counts/shipped-20s` read with the SERVER spans of product-catalog taken out of some traces in memory;
  `TestLostSpansAreMarkedInTheReport` in `cmd/rhobound` tests the report's marks.
- `longlived_test.go` tests the refusal of server spans that are streams, not visits (hand-made spans for the three conditions
  and their edges, and `internal/infer/testdata/counts/shipped-20s` with three streams of 600 s added to flagd in memory);
  `structure_internal_test.go` tests that the stage template's search finishes on a service that calls one callee dozens of
  times in a row per visit (it took minutes before it remembered the stage pairs that failed), with a time limit;
  `quantized_internal_test.go` and `quantized_run_test.go` test the reading of calls from a caller whose spans start on whole
  milliseconds (the pod found from its spans, the callee-based stages, the allowance at the visit's end, and a Node.js-like
  caller in the simulator's span writer, which can write a start exactly); `warnings_internal_test.go` the batch and
  mean-count warnings and the list of services that are not modelled.
- `validation/cluster/otel/otelcap` has tests of its own (the time parsing, the cut by the time a trace started, the rewrite of nested
  spans and the dropping of a service, the description of a capture), and `e2e/imagetar` has one that reads back the image tar it writes
  as an importer does (manifest, config and layer digests, the two programs in the layer).
- Comments in the regression tests, the headers of the logs in `docs/accuracy-logs/`, and the numbers quoted in
  [ACCURACY.md](ACCURACY.md), [VALIDATION.md](VALIDATION.md) and [DEMO.md](DEMO.md) say how earlier code behaved, by
  short commit hash ("each failed at 29a7ed9"). Those hashes name commits of the development history, which is not
  published; the commits of this repository are squashed releases, so those hashes do not name commits that can be
  checked out here.

With coverage:

```
$ go test -count=1 -cover ./...
ok  	github.com/Sharvil2704/rhobound-cli/cmd/rhobound	0.966s	coverage: 90.3% of statements
	github.com/Sharvil2704/rhobound-cli/e2e/demoapp		coverage: 0.0% of statements
ok  	github.com/Sharvil2704/rhobound-cli/e2e/imagetar	0.003s	coverage: 35.4% of statements
	github.com/Sharvil2704/rhobound-cli/e2e/loadgen		coverage: 0.0% of statements
ok  	github.com/Sharvil2704/rhobound-cli/internal/infer	32.772s	coverage: 96.7% of statements
ok  	github.com/Sharvil2704/rhobound-cli/internal/mathx	6.728s	coverage: 99.0% of statements
ok  	github.com/Sharvil2704/rhobound-cli/internal/model	0.074s	coverage: 92.1% of statements
ok  	github.com/Sharvil2704/rhobound-cli/internal/otlp	0.006s	coverage: 91.3% of statements
ok  	github.com/Sharvil2704/rhobound-cli/internal/report	0.002s	coverage: 60.6% of statements
ok  	github.com/Sharvil2704/rhobound-cli/validation/cluster/otel/otelcap	0.031s	coverage: 79.9% of statements
```

(`internal/report` is exercised mostly by the CLI tests in `cmd/rhobound`; `report_test.go` covers the "hop from the mean" line. The demo services
are tested by the end-to-end tests below. `e2e/imagetar`'s test covers the archive writer (`writeArchive`); `main` and `run`, which cross-build the demo's
programs, are run by `go run ./e2e/imagetar` and by the cluster scripts.)

### The race detector

```
$ go test -race -count=1 -timeout 30m ./...
ok  	github.com/Sharvil2704/rhobound-cli/cmd/rhobound	11.893s
?   	github.com/Sharvil2704/rhobound-cli/e2e/demoapp	[no test files]
ok  	github.com/Sharvil2704/rhobound-cli/e2e/imagetar	1.007s
?   	github.com/Sharvil2704/rhobound-cli/e2e/loadgen	[no test files]
ok  	github.com/Sharvil2704/rhobound-cli/internal/infer	326.919s
ok  	github.com/Sharvil2704/rhobound-cli/internal/mathx	17.127s
ok  	github.com/Sharvil2704/rhobound-cli/internal/model	1.353s
ok  	github.com/Sharvil2704/rhobound-cli/internal/otlp	1.016s
ok  	github.com/Sharvil2704/rhobound-cli/internal/report	1.008s
ok  	github.com/Sharvil2704/rhobound-cli/validation/cluster/otel/otelcap	1.357s
```

It took 5 minutes 27 seconds in all; `internal/infer` is about ten times slower under the race detector than without it. Go's default limit
for one package's tests is 10 minutes, which is why the command gives `-timeout 30m`.

### Formatting and vet

```
$ gofmt -l .
$ go vet ./... && go vet -tags e2e ./... && go vet -tags accuracy ./...
```

Both print nothing when all is well.

### The accuracy studies

The accuracy studies are behind the `accuracy` build tag because they take seconds to minutes each and print tables
instead of asserting (except `TestHarnessPlumbing`, which checks the harness's own arithmetic). Run one at a time
with `-run`:

```
$ go test -tags accuracy -count=1 -run TestHarnessPlumbing -v ./internal/infer
=== RUN   TestHarnessPlumbing
--- PASS: TestHarnessPlumbing (0.47s)
PASS
ok  	github.com/Sharvil2704/rhobound-cli/internal/infer	0.469s
```

```
$ go test -tags accuracy -count=1 -run TestGlueEndToEnd -v ./internal/infer
=== RUN   TestGlueEndToEnd
GLUE post 0 chg@a truth L 103.06 dL 1.84 (se 0.01) | true parameters L 102.12 (-0.9%) dL 1.81 (-1.5%)
GLUE   spans include the wait false a S 12.01 glue 0.00 cap 1 | L 106.13 (+3.0%) dL 1.78 (-3.1% of dL, -0.06% of L)
GLUE   spans include the wait true  a S 12.01 glue 0.00 cap 1 | L 106.13 (+3.0%) dL 1.78 (-3.1% of dL, -0.06% of L)
GLUE post 0 chg@b truth L 102.35 dL 27.23 (se 0.46) | true parameters L 102.12 (-0.2%) dL 27.12 (-0.4%)
GLUE   spans include the wait false a S 11.99 glue 0.00 cap 1 | L 105.42 (+3.0%) dL 29.17 (+7.1% of dL, +1.89% of L)
GLUE   spans include the wait true  a S 11.99 glue 0.00 cap 1 | L 105.44 (+3.0%) dL 29.19 (+7.2% of dL, +1.91% of L)
GLUE post 3 chg@a truth L 104.98 dL 1.83 (se 0.01) | true parameters L 105.12 (+0.1%) dL 1.81 (-0.9%)
GLUE   spans include the wait false a S 11.84 glue 3.00 cap 1 | L 110.92 (+5.7%) dL 1.72 (-5.8% of dL, -0.10% of L)
GLUE   spans include the wait true  a S 11.84 glue 3.00 cap 1 | L 110.92 (+5.7%) dL 1.72 (-5.8% of dL, -0.10% of L)
GLUE post 3 chg@b truth L 105.11 dL 26.92 (se 0.56) | true parameters L 105.12 (+0.0%) dL 27.12 (+0.7%)
GLUE   spans include the wait false a S 11.91 glue 3.00 cap 1 | L 106.25 (+1.1%) dL 27.49 (+2.1% of dL, +0.55% of L)
GLUE   spans include the wait true  a S 11.91 glue 3.00 cap 1 | L 106.25 (+1.1%) dL 27.49 (+2.1% of dL, +0.55% of L)
--- PASS: TestGlueEndToEnd (1.55s)
PASS
ok  	github.com/Sharvil2704/rhobound-cli/internal/infer	1.552s
```

Each `GLUE` row compares the truth of a simulation with the model, first given the true parameters, then given what
it read from one simulated capture. `TestAccuracy` (32 scenarios against 64 simulated replications each) took 18.3 s
here. [ACCURACY.md section 11](ACCURACY.md#11-re-running) lists every study, its run time (from about 1 s to about
2 minutes) and the switches that pick seeds and modes. `make accuracy` runs them all in one go, with a 60-minute
timeout; it was not run as a whole for this page.

### The demo without a cluster (loopback smoke test)

`TestLoopbackSmoke` runs the Kubernetes demo's services and load generator as local processes on loopback ports, then
runs the CLI on their traces. It needs no docker and no cluster (about 35 s):

```
$ go test -tags e2e -count=1 -run TestLoopbackSmoke -v ./e2e
=== RUN   TestLoopbackSmoke
=== RUN   TestLoopbackSmoke/keepalive=false
    smoke_test.go:119: load generator: sent 433, 297 measured, 0 errors, mean 44.91 ms, lag 1.1 ms, 5 windows; traces: entry 44.72 ms over 433 spans; api api-a 51.0% wait 7.62 ms, api-b 49.0% wait 5.26 ms
    smoke_test.go:150: rhobound: own ms frontend 3.41 (3), api 20.27 (20), db 8.14 (8), cache 5.51 (5); entry model 43.68 ms, load generator 44.91 ms; change api +8ms: +14.07 ms
    smoke_test.go:160: db down: sent 65, errors 65, measured 0; {"errors":65,"max_lag_ms":1.04427,"mean_ms":null,"requests":0,"rps":30,"sent":65,"windows":[{"mean_ms":0,"n":0,"start_s":0}]}
=== RUN   TestLoopbackSmoke/keepalive=true
    smoke_test.go:119: load generator: sent 447, 288 measured, 0 errors, mean 38.06 ms, lag 1.1 ms, 5 windows; traces: entry 37.88 ms over 447 spans; api api-a 60.2% wait 7.91 ms, api-b 39.8% wait 3.37 ms
--- PASS: TestLoopbackSmoke (33.74s)
    --- PASS: TestLoopbackSmoke/keepalive=false (17.47s)
    --- PASS: TestLoopbackSmoke/keepalive=true (15.48s)
PASS
ok  	github.com/Sharvil2704/rhobound-cli/e2e	33.739s
```

It is a plumbing check, with only about 15 s of load: its numbers are noisy and it is not an accuracy test. The
numbers in brackets are the true service times.

### The Kubernetes end-to-end test (kind)

`TestClusterEndToEnd` builds the demo services into an image, creates a kind cluster named `rb-oss-e2e` with its own
kubeconfig file (it never touches your default one), deploys four services, drives a Poisson load, rolls out a change
with `kubectl set env`, and compares the CLI's prediction with the latency the load generator measured. It needs
`docker`, `kind` and `kubectl` on the PATH, and takes about 13 minutes with the default two rounds. It deletes the
cluster at the end.

```
RHOBOUND_E2E=1 go test -tags e2e -v -count=1 -timeout 40m ./e2e
```

It was not run for this page; [DEMO.md](DEMO.md#demo-2-kubernetes-kind) shows the result of its last passing run.
Without `RHOBOUND_E2E=1` it skips:

```
$ go test -tags e2e -count=1 -run TestClusterEndToEnd -v ./e2e
=== RUN   TestClusterEndToEnd
    e2e_test.go:1291: set RHOBOUND_E2E=1 (needs docker, kind and kubectl; or kubectl and RHOBOUND_E2E_KUBECONFIG)
--- SKIP: TestClusterEndToEnd (0.00s)
PASS
ok  	github.com/Sharvil2704/rhobound-cli/e2e	0.002s
```

Its settings are environment variables, described at the top of `e2e/e2e_test.go`:

| variable | effect |
|---|---|
| `RHOBOUND_E2E_ROUNDS=N` | before/after pairs (default 2) |
| `RHOBOUND_E2E_STRICT=1` | fail instead of skip when the measurement was not steady |
| `RHOBOUND_E2E_KEEPALIVE=1` | the services reuse connections (a robustness run; expect the share gate to mark it unsteady) |
| `RHOBOUND_E2E_WORK=DIR` | the work folder (default: a new folder under `rb-e2e-work` in the user cache folder, Go's `os.UserCacheDir`; kept when the test fails or skips) |
| `RHOBOUND_E2E_KEEP=1` | keep the cluster afterwards; a later run reuses it |
| `RHOBOUND_E2E_CAPTURE=DIR` | read one kept capture offline (`TestAnalyzeCapture`) |
| `RHOBOUND_E2E_RPS=N` | the load generator's rate, requests/s (default 30) |
| `RHOBOUND_E2E_EXTRA_MS=X` | the change: a constant extra time in api, ms (default 8) |
| `RHOBOUND_E2E_MAX_RHO=F` | passed to `rhobound predict` as `--max-rho` (default: the CLI's own, 0.85) |
| `RHOBOUND_E2E_KUBECONFIG=FILE` | use the cluster of this kubeconfig file instead of kind (no kind, no docker); give its kubelets `container-log-max-size=100Mi` |
| `RHOBOUND_E2E_IMAGE_LOADED=1` | the image `rhobound-demo:e2e` is on every node already (required with `RHOBOUND_E2E_KUBECONFIG`; `go run ./e2e/imagetar -arch amd64 -o FILE.tar` writes it for another architecture) |
| `RHOBOUND_E2E_APP_SELECTOR=k=v`, `RHOBOUND_E2E_LOAD_SELECTOR=k=v` | node selectors for the demo services and for the load generator (which tolerates taints with the selector's keys) |
| `RHOBOUND_E2E_SPREAD=1` | spread the demo's pods over the selected nodes, so calls cross nodes |

[validation/cluster/](../validation/cluster/README.md) runs this test on a real cluster (k3s, or any Kubernetes cluster) in its
external-cluster mode, at three loads (`phase-a.sh`); [REAL_CLUSTER.md](REAL_CLUSTER.md#2-how-to-reproduce-it) says how to set the
cluster up and gives the commands.

## Continuous integration

`.github/workflows/ci.yml` runs on every push and pull request, on `ubuntu-latest`, twice: with the Go version that
`go.mod` names, and with the current stable release:

1. `gofmt -l .` must print nothing (under `bash`, with `pipefail`, so a file gofmt cannot parse fails the step);
2. `go vet ./... && go vet -tags e2e ./...` (the `accuracy` tag is not vetted in CI; `make vet` does it);
3. `go test -race -count=1 -timeout 30m ./...` (the race detector makes `internal/infer` about ten times slower, and Go's default limit is 10 minutes per package).

A second job, `mcp`, runs the same three steps in the nested module `mcp/` (the MCP server), with the Go version that
`mcp/go.mod` names (1.25) and with the current stable release, after `go mod tidy -diff` (its `go.mod` and `go.sum`
must be tidy): the root module stays
on Go 1.23 and the standard library, and `./...` in the root never descends into `mcp/`. The root job's tests include
the dependency check (`tools/depcheck`: the CLI is built from the standard library and this module only, and `go.mod`
says `go 1.23`) and the contract-doc check (`tools/gencontract`: the code tables in `docs/mcp/DATA.md` match the
registry).

A third job, `golden-arm64`, runs on GitHub's `ubuntu-24.04-arm` runner, because the goldens of the previous release
(`testdata/golden-1a2df09/arm64`) and the judge packet that `testdata/mcp/loop/judge-answer.json` cites were recorded
on linux/arm64, and on the amd64 runners the golden sweep skips: it runs the byte-for-byte golden sweep, fit parity,
the documented pod-identity change and the captures past the reader's caps, whose comparisons with 1a2df09's bytes
run only where those goldens are (`./cmd/rhobound`; amd64 checks their numbers only, and logs that; under `-race` the
captures of 65 MiB and 1 GiB are not read), and the loop path (`./tools/gencontract`). Each must pass by name. A fourth
job, `golden-live`, checks the same byte rule on amd64, where no goldens are stored: `tools/golden-live.sh` builds
rhobound from commit 1a2df09 out of the history (the checkout fetches it all) and compares it with this tree's CLI, byte
for byte, on every case of the manifest.

A fifth job builds and tests `rust/` (rbfast) on Rust 1.99.1, pinned so that a new stable release's lints cannot turn
it red, with `cargo fmt --check`, `cargo clippy -D warnings` and `cargo test --release`; see
[rust/README.md](../rust/README.md).

It does not run the accuracy studies or either end-to-end test.

## Troubleshooting

**`make: command not found`.** `make` is optional. Without it, bash prints a message like this one (the wording
depends on your shell and system). Use the go command from the table above instead:

```
$ make build
bash: make: command not found
$ go build -o bin/rhobound ./cmd/rhobound
```

**Your Go is too old.** With Go 1.21 or later and the default `GOTOOLCHAIN=auto`, an older `go` downloads a newer
toolchain by itself (that needs network access). Offline, or with `GOTOOLCHAIN=local`, it stops with a message like
this one (reproduced by raising the `go` line of `go.mod` in a scratch copy, since this machine has Go 1.27.2):

```
$ GOTOOLCHAIN=local go build ./cmd/rhobound
go: go.mod requires go >= 1.99 (running go 1.27.2; GOTOOLCHAIN=local)
```

Install Go 1.23 or later from <https://go.dev/dl/>.

**Commands run outside the repository.** `go build` and `go test` must run inside the repository (the folder with
`go.mod`, or below it):

```
$ go test ./...
# ./...
pattern ./...: directory prefix . does not contain main module or its selected dependencies
FAIL	./... [setup failed]
FAIL
```

**`-race requires cgo`.** The race detector needs cgo and a C compiler:

```
$ CGO_ENABLED=0 go test -race ./internal/otlp
go: -race requires cgo; enable cgo by setting CGO_ENABLED=1
```

Install `gcc` (or `clang`) and set `CGO_ENABLED=1`, or run the tests without `-race`.

**`build constraints exclude all Go files` in `e2e`.** The end-to-end tests need the `e2e` build tag (the path is
shortened here):

```
$ go test ./e2e
# github.com/Sharvil2704/rhobound-cli/e2e
package github.com/Sharvil2704/rhobound-cli/e2e: build constraints exclude all Go files in .../e2e
FAIL	github.com/Sharvil2704/rhobound-cli/e2e [setup failed]
FAIL
```

Add `-tags e2e`.

**`no tests to run` for an accuracy study.** The studies need the `accuracy` build tag:

```
$ go test -count=1 -run 'TestAccuracy$' -v ./internal/infer
testing: warning: no tests to run
PASS
ok  	github.com/Sharvil2704/rhobound-cli/internal/infer	0.001s [no tests to run]
```

Add `-tags accuracy`. For several studies at once also add `-timeout 60m`: by default `go test` stops a test binary
after 10 minutes.

**The kind test skips with `UNSTABLE measurement`.** The test checks that each measured window was steady (even load
split, no stalls, no lost spans, windows of the same kind that agree). If one was not, it logs why and skips instead
of judging the model, and keeps its work folder. Look at the `UNSTABLE:` lines and `result.json` in that folder.
`RHOBOUND_E2E_STRICT=1` makes it fail instead.

**The CLI refuses a capture.** That is not a build problem: [USAGE.md](USAGE.md#9-exit-codes-and-refusals) lists the
refusals and what to do about each.

package modelchecker

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	ast "fizz/proto"

	"google.golang.org/protobuf/proto"
)

// The benchmark specs live in testdata/bench, each next to its parsed AST.
// To regenerate an AST after editing a spec:
//
//	bazel-bin/parser/parser_bin modelchecker/testdata/bench/Name.fizz
//
// Lock:       roles, atomic actions, role function calls, three assertions.
// TwoPhase:   non-atomic actions, thread interleaving, oneof, crash on yield.
// Workers:    symmetric roles, so every dedup lookup permutes the state.
// Containers: no roles, nested containers, nondeterministic choice.
var benchSpecs = []string{"Lock", "TwoPhase", "Workers", "Containers"}

// benchMode is one way of running the exploration engine.
type benchMode struct {
	name      string
	configure func(p *Processor)
}

var benchModes = []benchMode{
	// What `fizz spec.fizz` runs: BFS with the processed queue.
	{"default", func(p *Processor) { p.SetExperimentalProcessedQueue(true) }},
	{"classic", func(p *Processor) {}},
	{"nograph", func(p *Processor) {
		p.SetExperimentalProcessedQueue(true)
		p.SetExperimentalNoGraph(true)
	}},
	{"lazyfork", func(p *Processor) {
		p.SetExperimentalProcessedQueue(true)
		p.SetExperimentalLazyFork(true)
	}},
}

func benchDataDir() string {
	base := "."
	if dir := os.Getenv("RUNFILES_DIR"); dir != "" {
		base = filepath.Join(dir, "_main", "modelchecker")
	}
	return filepath.Join(base, "testdata", "bench")
}

// loadBenchSpec reads a benchmark spec's AST and its options: the
// frontmatter over the defaults `fizz` applies.
func loadBenchSpec(tb testing.TB, name string) (*ast.File, *ast.StateSpaceOptions) {
	tb.Helper()
	file, err := readAstFromFile(filepath.Join(benchDataDir(), name+".json"))
	if err != nil {
		tb.Fatalf("reading %s: %v", name, err)
	}
	config := &ast.StateSpaceOptions{Options: &ast.Options{}}
	if yaml := file.GetFrontMatter().GetYaml(); yaml != "" {
		fm, err := ReadOptionsFromYamlString(yaml)
		if err != nil {
			tb.Fatalf("frontmatter of %s: %v", name, err)
		}
		proto.Merge(config, fm)
	}
	if config.Options.MaxActions == 0 {
		config.Options.MaxActions = 100
	}
	if config.Options.MaxConcurrentActions == 0 {
		config.Options.MaxConcurrentActions = min(2, config.Options.MaxActions)
	}
	if config.DeadlockDetection == nil {
		config.DeadlockDetection = proto.Bool(true)
	}
	if config.Options.CrashOnYield == nil {
		config.Options.CrashOnYield = proto.Bool(true)
	}
	return file, config
}

// silenceStdout sends the engine's progress lines to /dev/null until the
// returned function is called.
func silenceStdout(tb testing.TB) func() {
	tb.Helper()
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		tb.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = devNull
	return func() {
		os.Stdout = stdout
		devNull.Close()
	}
}

func runBenchSpec(tb testing.TB, file *ast.File, config *ast.StateSpaceOptions, mode benchMode) *Processor {
	tb.Helper()
	p := NewProcessor([]*ast.File{file}, config, false, 0, benchDataDir(), "bfs", true, nil, nil, "")
	mode.configure(p)
	_, failed, err := p.Start()
	if err != nil {
		tb.Fatalf("model checking failed: %v", err)
	}
	if failed != nil {
		tb.Fatalf("unexpected failing node %s", failed.GetName())
	}
	return p
}

// BenchmarkModelCheck explores each benchmark spec's whole state space. The
// states metric is the number of stored nodes: a change that is meant to be
// performance-only must leave it as it was.
func BenchmarkModelCheck(b *testing.B) {
	defer silenceStdout(b)()
	for _, name := range benchSpecs {
		file, config := loadBenchSpec(b, name)
		for _, mode := range benchModes {
			b.Run(name+"/"+mode.name, func(b *testing.B) {
				b.ReportAllocs()
				states := 0
				for i := 0; i < b.N; i++ {
					states = runBenchSpec(b, file, config, mode).GetVisitedNodesCount()
				}
				b.ReportMetric(float64(states), "states")
			})
		}
	}
}

// BenchmarkRetainedHeap reports the heap a finished run keeps alive per
// stored node, the number that sets the peak memory of a large run.
func BenchmarkRetainedHeap(b *testing.B) {
	defer silenceStdout(b)()
	for _, name := range benchSpecs {
		file, config := loadBenchSpec(b, name)
		for _, mode := range benchModes[:2] {
			b.Run(name+"/"+mode.name, func(b *testing.B) {
				var perState float64
				for i := 0; i < b.N; i++ {
					var before, after runtime.MemStats
					runtime.GC()
					runtime.ReadMemStats(&before)
					p := runBenchSpec(b, file, config, mode)
					runtime.GC()
					runtime.ReadMemStats(&after)
					perState = (float64(after.HeapAlloc) - float64(before.HeapAlloc)) / float64(p.GetVisitedNodesCount())
					runtime.KeepAlive(p)
				}
				b.ReportMetric(perState, "retained-B/state")
			})
		}
	}
}

// benchState returns one of the deepest stored states of a spec, picked by
// hash so that every run measures the same one.
func benchState(tb testing.TB, name string) *Process {
	tb.Helper()
	file, config := loadBenchSpec(tb, name)
	p := runBenchSpec(tb, file, config, benchModes[1])
	hashes := make([]string, 0, len(p.visited))
	for hash := range p.visited {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)
	var deepest *Node
	for _, hash := range hashes {
		node := p.visited[hash]
		if node == nil || node.Process == nil {
			continue
		}
		if deepest == nil || node.actionDepth > deepest.actionDepth {
			deepest = node
		}
	}
	if deepest == nil {
		tb.Fatalf("%s: no stored state", name)
	}
	return deepest.Process
}

// BenchmarkFork measures the deep copy made for every scheduled action.
func BenchmarkFork(b *testing.B) {
	restore := silenceStdout(b)
	for _, name := range benchSpecs {
		process := benchState(b, name)
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				process.Fork()
			}
		})
	}
	restore()
}

// BenchmarkCloneForAssert measures the copy each assertion is checked on.
func BenchmarkCloneForAssert(b *testing.B) {
	restore := silenceStdout(b)
	for _, name := range benchSpecs {
		process := benchState(b, name)
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				process.CloneForAssert(nil, 0)
			}
		})
	}
	restore()
}

// BenchmarkHashCode measures hashing a state from scratch, as dedup does
// for every new node.
func BenchmarkHashCode(b *testing.B) {
	restore := silenceStdout(b)
	for _, name := range benchSpecs {
		process := benchState(b, name)
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				process.CachedHashCode = ""
				process.Heap.CachedHashCode = ""
				process.HashCode()
			}
		})
	}
	restore()
}

// BenchmarkCheckInvariants measures checking every assertion of a spec on
// one state.
func BenchmarkCheckInvariants(b *testing.B) {
	restore := silenceStdout(b)
	for _, name := range benchSpecs {
		process := benchState(b, name)
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				CheckInvariants(process)
			}
		})
	}
	restore()
}

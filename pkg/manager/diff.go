// Copyright 2024 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/syzkaller/pkg/corpus"
	"github.com/google/syzkaller/pkg/flatrpc"
	"github.com/google/syzkaller/pkg/fuzzer"
	"github.com/google/syzkaller/pkg/fuzzer/queue"
	"github.com/google/syzkaller/pkg/instance"
	"github.com/google/syzkaller/pkg/log"
	"github.com/google/syzkaller/pkg/mgrconfig"
	"github.com/google/syzkaller/pkg/osutil"
	"github.com/google/syzkaller/pkg/report"
	"github.com/google/syzkaller/pkg/repro"
	"github.com/google/syzkaller/pkg/rpcserver"
	"github.com/google/syzkaller/pkg/signal"
	"github.com/google/syzkaller/pkg/stat"
	"github.com/google/syzkaller/pkg/vcs"
	"github.com/google/syzkaller/pkg/vminfo"
	"github.com/google/syzkaller/prog"
	"github.com/google/syzkaller/vm"
	"github.com/google/syzkaller/vm/dispatcher"
	"golang.org/x/sync/errgroup"
)

type DiffFuzzerConfig struct {
	Debug        bool
	PatchedOnly  chan *UniqueBug
	Store        *DiffFuzzerStore
	ArtifactsDir string // Where to store the artifacts that supplement the logs.
	// The fuzzer waits no more than MaxTriageTime time until it starts taking VMs away
	// for bug reproduction.
	// The option may help find a balance between spending too much time triaging
	// the corpus and not reaching a proper kernel coverage.
	MaxTriageTime time.Duration
	// If non-empty, the fuzzer will spend no more than this amount of time
	// trying to reach the modified code. The time is counted since the moment
	// 99% of the corpus is triaged.
	FuzzToReachPatched time.Duration
}

func (cfg *DiffFuzzerConfig) TriageDeadline() <-chan time.Time {
	if cfg.MaxTriageTime == 0 {
		return nil
	}
	return time.After(cfg.MaxTriageTime)
}

type UniqueBug struct {
	// The report from the patched kernel.
	Report *report.Report
	Repro  *repro.Result
}

func RunDiffFuzzer(ctx context.Context, baseCfg, newCfg *mgrconfig.Config, cfg DiffFuzzerConfig) error {
	if cfg.PatchedOnly == nil {
		return fmt.Errorf("you must set up a patched only channel")
	}
	base, err := setup("base", baseCfg, cfg.Debug)
	if err != nil {
		return err
	}
	new, err := setup("new", newCfg, cfg.Debug)
	if err != nil {
		return err
	}
	eg, ctx := errgroup.WithContext(ctx)
	eg.Go(func() error {
		info, err := LoadSeeds(newCfg, true)
		if err != nil {
			return err
		}
		select {
		case new.candidates <- info.Candidates:
		case <-ctx.Done():
		}
		return nil
	})

	stream := queue.NewRandomQueue(4096, rand.New(rand.NewSource(time.Now().UnixNano())))
	base.source = stream
	new.duplicateInto = stream

	diffCtx := &diffContext{
		cfg:           cfg,
		doneRepro:     make(chan *ReproResult),
		base:          base,
		new:           new,
		store:         cfg.Store,
		reproAttempts: map[string]int{},
		patchedOnly:   cfg.PatchedOnly,
	}
	if newCfg.HTTP != "" {
		diffCtx.http = &HTTPServer{
			Cfg:       newCfg,
			StartTime: time.Now(),
			DiffStore: cfg.Store,
			Pools: map[string]*vm.Dispatcher{
				new.name:  new.pool,
				base.name: base.pool,
			},
		}
		new.http = diffCtx.http
	}
	eg.Go(func() error {
		return diffCtx.Loop(ctx)
	})
	return eg.Wait()
}

type diffContext struct {
	cfg   DiffFuzzerConfig
	store *DiffFuzzerStore
	http  *HTTPServer

	doneRepro   chan *ReproResult
	base        *kernelContext
	new         *kernelContext
	patchedOnly chan *UniqueBug

	mu            sync.Mutex
	reproAttempts map[string]int
}

const (
	// Don't start reproductions until 90% of the corpus has been triaged.
	corpusTriageToRepro = 0.9
	// Start to monitor whether we reached the modified files only after triaging 99%.
	corpusTriageToMonitor = 0.99
)

func (dc *diffContext) Loop(baseCtx context.Context) error {
	g, ctx := errgroup.WithContext(baseCtx)
	reproLoop := NewReproLoop(dc, dc.new.pool.Total()-dc.new.cfg.FuzzingVMs, false)
	if dc.http != nil {
		dc.http.ReproLoop = reproLoop
		g.Go(func() error {
			return dc.http.Serve(ctx)
		})
	}

	g.Go(func() error {
		select {
		case <-ctx.Done():
			return nil
		case <-dc.waitCorpusTriage(ctx, corpusTriageToRepro):
		case <-dc.cfg.TriageDeadline():
			log.Logf(0, "timed out waiting for coprus triage")
		}
		log.Logf(0, "starting bug reproductions")
		reproLoop.Loop(ctx)
		return nil
	})

	g.Go(func() error { return dc.monitorPatchedCoverage(ctx) })
	g.Go(func() error { return dc.base.Loop(ctx) })
	g.Go(func() error { return dc.new.Loop(ctx) })

	runner := &reproRunner{done: make(chan reproRunnerResult, 2), kernel: dc.base}
	statTimer := time.NewTicker(5 * time.Minute)
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-statTimer.C:
			vals := make(map[string]int)
			for _, stat := range stat.Collect(stat.All) {
				vals[stat.Name] = stat.V
			}
			data, _ := json.MarshalIndent(vals, "", "  ")
			log.Logf(0, "STAT %s", data)
		case rep := <-dc.base.crashes:
			log.Logf(1, "base crash: %v", rep.Title)
			dc.store.BaseCrashed(rep.Title, rep.Report)
		case ret := <-runner.done:
			// We have run the reproducer on the base instance.

			// A sanity check: the base kernel might have crashed with the same title
			// since the moment we have stared the reproduction / running on the repro base.
			crashesOnBase := dc.store.EverCrashedBase(ret.origReport.Title)
			if ret.crashReport == nil && crashesOnBase {
				// Report it as error so that we could at least find it in the logs.
				log.Errorf("repro didn't crash base, but base itself crashed: %s", ret.origReport.Title)
			} else if ret.crashReport == nil {
				dc.store.BaseNotCrashed(ret.origReport.Title)
				select {
				case <-ctx.Done():
				case dc.patchedOnly <- &UniqueBug{
					Report: ret.origReport,
					Repro:  ret.repro,
				}:
				}
				log.Logf(0, "patched-only: %s", ret.origReport.Title)
				// Now that we know this bug only affects the patch kernel, we can spend more time
				// generating a minimalistic repro and a C repro.
				if !ret.fullRepro {
					reproLoop.Enqueue(&Crash{
						Report: &report.Report{
							Title:  ret.origReport.Title,
							Output: ret.repro.Prog.Serialize(),
						},
						FullRepro: true,
					})
				}
			} else {
				dc.store.BaseCrashed(ret.origReport.Title, ret.origReport.Report)
				log.Logf(0, "crashes both: %s / %s", ret.origReport.Title, ret.crashReport.Title)
			}
		case ret := <-dc.doneRepro:
			// We have finished reproducing a crash from the patched instance.
			if ret.Repro != nil && ret.Repro.Report != nil {
				origTitle := ret.Crash.Report.Title
				if ret.Repro.Report.Title == origTitle {
					origTitle = "-SAME-"
				}
				log.Logf(1, "found repro for %q (orig title: %q, reliability: %2.f), took %.2f minutes",
					ret.Repro.Report.Title, origTitle, ret.Repro.Reliability, ret.Stats.TotalTime.Minutes())
				g.Go(func() error {
					runner.Run(ctx, ret.Repro, ret.Crash.FullRepro)
					return nil
				})
			} else {
				origTitle := ret.Crash.Report.Title
				log.Logf(1, "failed repro for %q, err=%s", origTitle, ret.Err)
			}
			dc.store.SaveRepro(ret)
		case rep := <-dc.new.crashes:
			// A new crash is found on the patched instance.
			crash := &Crash{Report: rep}
			need := dc.NeedRepro(crash)
			log.Logf(0, "patched crashed: %v [need repro = %v]",
				rep.Title, need)
			dc.store.PatchedCrashed(rep.Title, rep.Report, rep.Output)
			if need {
				reproLoop.Enqueue(crash)
			}
		}
	}
	return g.Wait()
}

func (dc *diffContext) waitCorpusTriage(ctx context.Context, threshold float64) chan struct{} {
	const backOffTime = 30 * time.Second
	ret := make(chan struct{})
	go func() {
		for {
			select {
			case <-time.After(backOffTime):
			case <-ctx.Done():
				return
			}
			triaged := dc.new.triageProgress()
			if triaged >= threshold {
				log.Logf(0, "triaged %.1f%% of the corpus", triaged*100.0)
				close(ret)
				return
			}
		}
	}()
	return ret
}

var ErrPatchedAreaNotReached = errors.New("fuzzer has not reached the patched area")

func (dc *diffContext) monitorPatchedCoverage(ctx context.Context) error {
	if dc.cfg.FuzzToReachPatched == 0 {
		// The feature is disabled.
		return nil
	}

	// First wait until we have almost triaged all of the corpus.
	select {
	case <-ctx.Done():
		return nil
	case <-dc.waitCorpusTriage(ctx, corpusTriageToMonitor):
	}

	// By this moment, we must have coverage filters already filled out.
	focusPCs := 0
	// The last one is "everything else", so it's not of interest.
	coverFilters := dc.new.coverFilters
	for i := 0; i < len(coverFilters.Areas)-1; i++ {
		focusPCs += len(coverFilters.Areas[i].CoverPCs)
	}
	if focusPCs == 0 {
		// No areas were configured.
		log.Logf(1, "no PCs in the areas of focused fuzzing, skipping the zero patched coverage check")
		return nil
	}

	// Then give the fuzzer some change to get through.
	select {
	case <-time.After(dc.cfg.FuzzToReachPatched):
	case <-ctx.Done():
		return nil
	}
	focusAreaStats := dc.new.progsPerArea()
	if focusAreaStats[symbolsArea]+focusAreaStats[filesArea]+focusAreaStats[includesArea] > 0 {
		log.Logf(0, "fuzzer has reached the modified code (%d + %d + %d), continuing fuzzing",
			focusAreaStats[symbolsArea], focusAreaStats[filesArea], focusAreaStats[includesArea])
		return nil
	}
	log.Logf(0, "fuzzer has not reached the modified code in %s, aborting",
		dc.cfg.FuzzToReachPatched)
	return ErrPatchedAreaNotReached
}

// TODO: instead of this limit, consider expotentially growing delays between reproduction attempts.
const maxReproAttempts = 6

func skipDiffRepro(title string) bool {
	if strings.Contains(title, "no output") ||
		strings.Contains(title, "lost connection") ||
		strings.Contains(title, "detected stall") ||
		strings.Contains(title, "SYZ") {
		// Don't waste time reproducing these.
		return true
	}
	return false
}

func (dc *diffContext) NeedRepro(crash *Crash) bool {
	if crash.FullRepro {
		return true
	}
	if skipDiffRepro(crash.Title) {
		return true
	}
	dc.mu.Lock()
	defer dc.mu.Unlock()
	if dc.store.EverCrashedBase(crash.Title) {
		return false
	}
	if dc.reproAttempts[crash.Title] > maxReproAttempts {
		return false
	}
	return true
}

func (dc *diffContext) RunRepro(ctx context.Context, crash *Crash) *ReproResult {
	dc.mu.Lock()
	dc.reproAttempts[crash.Title]++
	dc.mu.Unlock()

	res, stats, err := repro.Run(ctx, crash.Output, repro.Environment{
		Config:   dc.new.cfg,
		Features: dc.new.features,
		Reporter: dc.new.reporter,
		Pool:     dc.new.pool,
		Fast:     !crash.FullRepro,
	})
	if res != nil && res.Report != nil {
		dc.mu.Lock()
		dc.reproAttempts[res.Report.Title] = maxReproAttempts
		dc.mu.Unlock()
	}
	ret := &ReproResult{
		Crash: crash,
		Repro: res,
		Stats: stats,
		Err:   err,
	}
	select {
	case dc.doneRepro <- ret:
	case <-ctx.Done():
		// If the context is cancelled, no one may be listening on doneRepro.
	}
	return ret
}

func (dc *diffContext) ResizeReproPool(size int) {
	dc.new.pool.ReserveForRun(size)
}

type kernelContext struct {
	name       string
	ctx        context.Context
	debug      bool
	cfg        *mgrconfig.Config
	reporter   *report.Reporter
	fuzzer     atomic.Pointer[fuzzer.Fuzzer]
	serv       rpcserver.Server
	servStats  rpcserver.Stats
	crashes    chan *report.Report
	pool       *vm.Dispatcher
	features   flatrpc.Feature
	candidates chan []fuzzer.Candidate
	// Once candidates is assigned, candidatesCount holds their original count.
	candidatesCount atomic.Int64

	coverFilters    CoverageFilters
	reportGenerator *ReportGeneratorWrapper

	http          *HTTPServer
	source        queue.Source
	duplicateInto queue.Executor
}

func setup(name string, cfg *mgrconfig.Config, debug bool) (*kernelContext, error) {
	osutil.MkdirAll(cfg.Workdir)

	kernelCtx := &kernelContext{
		name:            name,
		debug:           debug,
		cfg:             cfg,
		crashes:         make(chan *report.Report, 128),
		candidates:      make(chan []fuzzer.Candidate),
		servStats:       rpcserver.NewNamedStats(name),
		reportGenerator: ReportGeneratorCache(cfg),
	}

	var err error
	kernelCtx.reporter, err = report.NewReporter(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create reporter for %q: %w", name, err)
	}

	kernelCtx.serv, err = rpcserver.New(&rpcserver.RemoteConfig{
		Config:  cfg,
		Manager: kernelCtx,
		Stats:   kernelCtx.servStats,
		Debug:   debug,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create rpc server for %q: %w", name, err)
	}

	vmPool, err := vm.Create(cfg, debug)
	if err != nil {
		return nil, fmt.Errorf("failed to create vm.Pool for %q: %w", name, err)
	}

	kernelCtx.pool = vm.NewDispatcher(vmPool, kernelCtx.fuzzerInstance)
	return kernelCtx, nil
}

func (kc *kernelContext) Loop(baseCtx context.Context) error {
	defer log.Logf(1, "syz-diff (%s): kernel context loop terminated", kc.name)

	if err := kc.serv.Listen(); err != nil {
		return fmt.Errorf("failed to start rpc server: %w", err)
	}
	eg, ctx := errgroup.WithContext(baseCtx)
	kc.ctx = ctx
	eg.Go(func() error {
		return kc.serv.Serve(ctx)
	})
	eg.Go(func() error {
		kc.pool.Loop(ctx)
		return nil
	})
	return eg.Wait()
}

func (kc *kernelContext) MaxSignal() signal.Signal {
	if fuzzer := kc.fuzzer.Load(); fuzzer != nil {
		return fuzzer.Cover.CopyMaxSignal()
	}
	return nil
}

func (kc *kernelContext) BugFrames() (leaks, races []string) {
	return nil, nil
}

func (kc *kernelContext) MachineChecked(features flatrpc.Feature,
	syscalls map[*prog.Syscall]bool) (queue.Source, error) {
	if len(syscalls) == 0 {
		return nil, fmt.Errorf("all system calls are disabled")
	}
	log.Logf(0, "%s: machine check complete", kc.name)
	kc.features = features

	var source queue.Source
	if kc.source == nil {
		source = queue.Tee(kc.setupFuzzer(features, syscalls), kc.duplicateInto)
	} else {
		source = kc.source
	}
	opts := fuzzer.DefaultExecOpts(kc.cfg, features, kc.debug)
	return queue.DefaultOpts(source, opts), nil
}

func (kc *kernelContext) setupFuzzer(features flatrpc.Feature, syscalls map[*prog.Syscall]bool) queue.Source {
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	corpusObj := corpus.NewFocusedCorpus(kc.ctx, nil, kc.coverFilters.Areas)
	fuzzerObj := fuzzer.NewFuzzer(kc.ctx, &fuzzer.Config{
		Corpus:   corpusObj,
		Coverage: kc.cfg.Cover,
		// Fault injection may bring instaibility into bug reproducibility, which may lead to false positives.
		FaultInjection: false,
		Comparisons:    features&flatrpc.FeatureComparisons != 0,
		Collide:        true,
		EnabledCalls:   syscalls,
		NoMutateCalls:  kc.cfg.NoMutateCalls,
		PatchTest:      true,
		Logf: func(level int, msg string, args ...interface{}) {
			if level != 0 {
				return
			}
			log.Logf(level, msg, args...)
		},
	}, rnd, kc.cfg.Target)

	if kc.http != nil {
		kc.http.Fuzzer.Store(fuzzerObj)
		kc.http.EnabledSyscalls.Store(syscalls)
		kc.http.Corpus.Store(corpusObj)
	}

	var candidates []fuzzer.Candidate
	select {
	case candidates = <-kc.candidates:
	case <-kc.ctx.Done():
		// The loop will be aborted later.
		break
	}
	// We assign kc.fuzzer after kc.candidatesCount to simplify the triageProgress implementation.
	kc.candidatesCount.Store(int64(len(candidates)))
	kc.fuzzer.Store(fuzzerObj)

	filtered := FilterCandidates(candidates, syscalls, false).Candidates
	log.Logf(0, "%s: adding %d seeds", kc.name, len(filtered))
	fuzzerObj.AddCandidates(filtered)

	go func() {
		if !kc.cfg.Cover {
			return
		}
		for {
			select {
			case <-time.After(time.Second):
			case <-kc.ctx.Done():
				return
			}
			newSignal := fuzzerObj.Cover.GrabSignalDelta()
			if len(newSignal) == 0 {
				continue
			}
			kc.serv.DistributeSignalDelta(newSignal)
		}
	}()
	return fuzzerObj
}

func (kc *kernelContext) CoverageFilter(modules []*vminfo.KernelModule) ([]uint64, error) {
	kc.reportGenerator.Init(modules)
	filters, err := PrepareCoverageFilters(kc.reportGenerator, kc.cfg, false)
	if err != nil {
		return nil, fmt.Errorf("failed to init coverage filter: %w", err)
	}
	kc.coverFilters = filters
	for _, area := range filters.Areas {
		log.Logf(0, "area %q: %d PCs in the cover filter",
			area.Name, len(area.CoverPCs))
	}
	log.Logf(0, "executor cover filter: %d PCs", len(filters.ExecutorFilter))
	if kc.http != nil {
		kc.http.Cover.Store(&CoverageInfo{
			Modules:         modules,
			ReportGenerator: kc.reportGenerator,
			CoverFilter:     filters.ExecutorFilter,
		})
	}
	var pcs []uint64
	for pc := range filters.ExecutorFilter {
		pcs = append(pcs, pc)
	}
	return pcs, nil
}

func (kc *kernelContext) fuzzerInstance(ctx context.Context, inst *vm.Instance, updInfo dispatcher.UpdateInfo) {
	index := inst.Index()
	injectExec := make(chan bool, 10)
	kc.serv.CreateInstance(index, injectExec, updInfo)
	rep, err := kc.runInstance(ctx, inst, injectExec)
	lastExec, _ := kc.serv.ShutdownInstance(index, rep != nil)
	if rep != nil {
		rpcserver.PrependExecuting(rep, lastExec)
		select {
		case kc.crashes <- rep:
		case <-ctx.Done():
		}
	}
	if err != nil {
		log.Errorf("#%d run failed: %s", inst.Index(), err)
	}
}

func (kc *kernelContext) runInstance(ctx context.Context, inst *vm.Instance,
	injectExec <-chan bool) (*report.Report, error) {
	fwdAddr, err := inst.Forward(kc.serv.Port())
	if err != nil {
		return nil, fmt.Errorf("failed to setup port forwarding: %w", err)
	}
	executorBin, err := inst.Copy(kc.cfg.ExecutorBin)
	if err != nil {
		return nil, fmt.Errorf("failed to copy binary: %w", err)
	}
	host, port, err := net.SplitHostPort(fwdAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse manager's address")
	}
	cmd := fmt.Sprintf("%v runner %v %v %v", executorBin, inst.Index(), host, port)
	ctxTimeout, cancel := context.WithTimeout(ctx, kc.cfg.Timeouts.VMRunningTime)
	defer cancel()
	_, rep, err := inst.Run(ctxTimeout, kc.reporter, cmd, vm.ExitTimeout,
		vm.InjectExecuting(injectExec),
		vm.EarlyFinishCb(func() {
			// Depending on the crash type and kernel config, fuzzing may continue
			// running for several seconds even after kernel has printed a crash report.
			// This litters the log and we want to prevent it.
			kc.serv.StopFuzzing(inst.Index())
		}),
	)
	return rep, err
}

func (kc *kernelContext) triageProgress() float64 {
	fuzzer := kc.fuzzer.Load()
	if fuzzer == nil {
		return 0
	}
	total := kc.candidatesCount.Load()
	if total == 0.0 {
		// There were no candidates in the first place.
		return 1
	}
	return 1.0 - float64(fuzzer.CandidatesToTriage())/float64(total)
}

func (kc *kernelContext) progsPerArea() map[string]int {
	fuzzer := kc.fuzzer.Load()
	if fuzzer == nil {
		return nil
	}
	return fuzzer.Config.Corpus.ProgsPerArea()
}

// reproRunner is used to run reproducers on the base kernel to determine whether it is affected.
type reproRunner struct {
	done    chan reproRunnerResult
	running atomic.Int64
	kernel  *kernelContext
}

type reproRunnerResult struct {
	origReport  *report.Report
	crashReport *report.Report
	repro       *repro.Result
	fullRepro   bool // whether this was a full reproduction
}

const (
	// We want to avoid false positives as much as possible, so let's use
	// a stricter relibability cut-off than what's used inside pkg/repro.
	reliabilityCutOff = 0.4
	// 80% reliability x 3 runs is a 0.8% chance of false positives.
	// 6 runs at 40% reproducibility gives a ~4% false positive chance.
	reliabilityThreshold = 0.8
)

// Run executes the reproducer 3 times with slightly different options.
// The objective is to verify whether the bug triggered by the reproducer affects the base kernel.
// To avoid reporting false positives, the function does not require the kernel to crash with exactly
// the same crash title as in the original crash report. Any single crash is accepted.
// The result is sent back over the rr.done channel.
func (rr *reproRunner) Run(ctx context.Context, r *repro.Result, fullRepro bool) {
	if r.Reliability < reliabilityCutOff {
		log.Logf(1, "%s: repro is too unreliable, skipping", r.Report.Title)
		return
	}
	needRuns := 3
	if r.Reliability < reliabilityThreshold {
		needRuns = 6
	}

	pool := rr.kernel.pool
	cnt := int(rr.running.Add(1))
	pool.ReserveForRun(min(cnt, pool.Total()))
	defer func() {
		cnt := int(rr.running.Add(-1))
		rr.kernel.pool.ReserveForRun(min(cnt, pool.Total()))
	}()

	ret := reproRunnerResult{origReport: r.Report, repro: r, fullRepro: fullRepro}
	for doneRuns := 0; doneRuns < needRuns; {
		if ctx.Err() != nil {
			return
		}
		opts := r.Opts
		opts.Repeat = true
		if doneRuns%3 != 2 {
			// Two times out of 3, test with Threaded=true.
			// The third time we leave it as it was in the reproducer (in case it was important).
			opts.Threaded = true
		}
		var err error
		var result *instance.RunResult
		runErr := pool.Run(ctx, func(ctx context.Context, inst *vm.Instance, updInfo dispatcher.UpdateInfo) {
			var ret *instance.ExecProgInstance
			ret, err = instance.SetupExecProg(inst, rr.kernel.cfg, rr.kernel.reporter, nil)
			if err != nil {
				return
			}
			result, err = ret.RunSyzProg(instance.ExecParams{
				SyzProg:  r.Prog.Serialize(),
				Duration: max(r.Duration, time.Minute),
				Opts:     opts,
			})
		})
		logPrefix := fmt.Sprintf("attempt #%d to run %q on base", doneRuns, ret.origReport.Title)
		if errors.Is(runErr, context.Canceled) {
			// Just exit without sending anything over the channel.
			log.Logf(1, "%s: aborting due to context cancelation", logPrefix)
			return
		} else if runErr != nil || err != nil {
			log.Logf(1, "%s: skipping due to errors: %v / %v", logPrefix, runErr, err)
			continue
		}
		doneRuns++
		if result != nil && result.Report != nil {
			log.Logf(1, "%s: crashed with %s", logPrefix, result.Report.Title)
			ret.crashReport = result.Report
			break
		} else {
			log.Logf(1, "%s: did not crash", logPrefix)
		}
	}
	select {
	case rr.done <- ret:
	case <-ctx.Done():
	}
}

const (
	symbolsArea  = "symbols"
	filesArea    = "files"
	includesArea = "included"
)

func PatchFocusAreas(cfg *mgrconfig.Config, gitPatches [][]byte, baseHashes, patchedHashes map[string]string) {
	funcs := modifiedSymbols(baseHashes, patchedHashes)
	if len(funcs) > 0 {
		log.Logf(0, "adding modified_functions to focus areas: %q", funcs)
		cfg.Experimental.FocusAreas = append(cfg.Experimental.FocusAreas,
			mgrconfig.FocusArea{
				Name: symbolsArea,
				Filter: mgrconfig.CovFilterCfg{
					Functions: funcs,
				},
				Weight: 6.0,
			})
	}

	direct, transitive := affectedFiles(cfg, gitPatches)
	if len(direct) > 0 {
		sort.Strings(direct)
		log.Logf(0, "adding directly modified files to focus areas: %q", direct)
		cfg.Experimental.FocusAreas = append(cfg.Experimental.FocusAreas,
			mgrconfig.FocusArea{
				Name: filesArea,
				Filter: mgrconfig.CovFilterCfg{
					Files: direct,
				},
				Weight: 3.0,
			})
	}

	if len(transitive) > 0 {
		sort.Strings(transitive)
		log.Logf(0, "adding transitively affected to focus areas: %q", transitive)
		cfg.Experimental.FocusAreas = append(cfg.Experimental.FocusAreas,
			mgrconfig.FocusArea{
				Name: includesArea,
				Filter: mgrconfig.CovFilterCfg{
					Files: transitive,
				},
				Weight: 2.0,
			})
	}

	// Still fuzz the rest of the kernel.
	if len(cfg.Experimental.FocusAreas) > 0 {
		cfg.Experimental.FocusAreas = append(cfg.Experimental.FocusAreas,
			mgrconfig.FocusArea{
				Weight: 1.0,
			})
	}
}

func affectedFiles(cfg *mgrconfig.Config, gitPatches [][]byte) (direct, transitive []string) {
	const maxAffectedByHeader = 50

	directMap := make(map[string]struct{})
	transitiveMap := make(map[string]struct{})
	var allFiles []string
	for _, patch := range gitPatches {
		allFiles = append(allFiles, vcs.ParseGitDiff(patch)...)
	}
	for _, file := range allFiles {
		directMap[file] = struct{}{}
		if strings.HasSuffix(file, ".h") && cfg.KernelSrc != "" {
			// Ideally, we should combine this with the recompilation process - then we know
			// exactly which files were affected by the patch.
			out, err := osutil.RunCmd(time.Minute, cfg.KernelSrc, "/usr/bin/grep",
				"-rl", "--include", `*.c`, `<`+strings.TrimPrefix(file, "include/")+`>`)
			if err != nil {
				log.Logf(0, "failed to grep for the header usages: %v", err)
				continue
			}
			lines := strings.Split(string(out), "\n")
			if len(lines) >= maxAffectedByHeader {
				// It's too widespread. It won't help us focus on anything.
				log.Logf(0, "the header %q is included in too many files (%d)", file, len(lines))
				continue
			}
			for _, name := range lines {
				name = strings.TrimSpace(name)
				if name == "" {
					continue
				}
				transitiveMap[name] = struct{}{}
			}
		}
	}
	for name := range directMap {
		direct = append(direct, name)
	}
	for name := range transitiveMap {
		if _, ok := directMap[name]; ok {
			continue
		}
		transitive = append(transitive, name)
	}
	return
}

// If there are too many different symbols, they are no longer specific enough.
// Don't use them to focus the fuzzer.
const modifiedSymbolThreshold = 0.05

func modifiedSymbols(baseHashes, patchedHashes map[string]string) []string {
	var ret []string
	for name, hash := range patchedHashes {
		if baseHash, ok := baseHashes[name]; !ok || baseHash != hash {
			ret = append(ret, name)
			if float64(len(ret)) > float64(len(patchedHashes))*modifiedSymbolThreshold {
				return nil
			}
		}
	}
	sort.Strings(ret)
	return ret
}

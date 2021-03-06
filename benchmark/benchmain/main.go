/*
 *
 * Copyright 2017 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

/*
Package main provides benchmark with setting flags.

An example to run some benchmarks with profiling enabled:

go run benchmark/benchmain/main.go -benchtime=10s -workloads=all \
  -compression=gzip -maxConcurrentCalls=1 -trace=off \
  -reqSizeBytes=1,1048576 -respSizeBytes=1,1048576 -networkMode=Local \
  -cpuProfile=cpuProf -memProfile=memProf -memProfileRate=10000 -resultFile=result

As a suggestion, when creating a branch, you can run this benchmark and save the result
file "-resultFile=basePerf", and later when you at the middle of the work or finish the
work, you can get the benchmark result and compare it with the base anytime.

Assume there are two result files names as "basePerf" and "curPerf" created by adding
-resultFile=basePerf and -resultFile=curPerf.
	To format the curPerf, run:
  	go run benchmark/benchresult/main.go curPerf
	To observe how the performance changes based on a base result, run:
  	go run benchmark/benchresult/main.go basePerf curPerf
*/
package main

import (
	"context"
	"encoding/gob"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"reflect"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	bm "google.golang.org/grpc/benchmark"
	testpb "google.golang.org/grpc/benchmark/grpc_testing"
	"google.golang.org/grpc/benchmark/latency"
	"google.golang.org/grpc/benchmark/stats"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/internal/channelz"
	"google.golang.org/grpc/test/bufconn"
)

const (
	modeOn   = "on"
	modeOff  = "off"
	modeBoth = "both"

	// compression modes
	modeAll  = "all"
	modeGzip = "gzip"
	modeNop  = "nop"
)

var allCompressionModes = []string{modeOff, modeGzip, modeNop, modeAll}
var allTraceModes = []string{modeOn, modeOff, modeBoth}
var allPreloaderModes = []string{modeOn, modeOff, modeBoth}

const (
	workloadsUnary         = "unary"
	workloadsStreaming     = "streaming"
	workloadsUnconstrained = "unconstrained"
	workloadsAll           = "all"
)

var allWorkloads = []string{workloadsUnary, workloadsStreaming, workloadsUnconstrained, workloadsAll}

var (
	runMode = []bool{true, true, true} // {runUnary, runStream, runUnconstrained}
	// When set the latency to 0 (no delay), the result is slower than the real result with no delay
	// because latency simulation section has extra operations
	ltc                    = []time.Duration{0, 40 * time.Millisecond} // if non-positive, no delay.
	kbps                   = []int{0, 10240}                           // if non-positive, infinite
	mtu                    = []int{0}                                  // if non-positive, infinite
	maxConcurrentCalls     = []int{1, 8, 64, 512}
	reqSizeBytes           = []int{1, 1024, 1024 * 1024}
	respSizeBytes          = []int{1, 1024, 1024 * 1024}
	enableTrace            []bool
	benchtime              time.Duration
	memProfile, cpuProfile string
	memProfileRate         int
	modeCompressor         []string
	enablePreloader        []bool
	enableChannelz         []bool
	networkMode            string
	benchmarkResultFile    string
	networks               = map[string]latency.Network{
		"Local":    latency.Local,
		"LAN":      latency.LAN,
		"WAN":      latency.WAN,
		"Longhaul": latency.Longhaul,
	}
)

func unaryBenchmark(startTimer func(), stopTimer func(uint64), benchFeatures stats.Features, benchtime time.Duration, s *stats.Stats) uint64 {
	caller, cleanup := makeFuncUnary(benchFeatures)
	defer cleanup()
	return runBenchmark(caller, startTimer, stopTimer, benchFeatures, benchtime, s)
}

func streamBenchmark(startTimer func(), stopTimer func(uint64), benchFeatures stats.Features, benchtime time.Duration, s *stats.Stats) uint64 {
	caller, cleanup := makeFuncStream(benchFeatures)
	defer cleanup()
	return runBenchmark(caller, startTimer, stopTimer, benchFeatures, benchtime, s)
}

func unconstrainedStreamBenchmark(benchFeatures stats.Features, warmuptime, benchtime time.Duration) (uint64, uint64) {
	var sender, recver func(int)
	var cleanup func()
	if benchFeatures.EnablePreloader {
		sender, recver, cleanup = makeFuncUnconstrainedStreamPreloaded(benchFeatures)
	} else {
		sender, recver, cleanup = makeFuncUnconstrainedStream(benchFeatures)
	}
	defer cleanup()

	var (
		wg            sync.WaitGroup
		requestCount  uint64
		responseCount uint64
	)
	wg.Add(2 * benchFeatures.MaxConcurrentCalls)

	// Resets the counters once warmed up
	go func() {
		<-time.NewTimer(warmuptime).C
		atomic.StoreUint64(&requestCount, 0)
		atomic.StoreUint64(&responseCount, 0)
	}()

	bmEnd := time.Now().Add(benchtime + warmuptime)
	for i := 0; i < benchFeatures.MaxConcurrentCalls; i++ {
		go func(pos int) {
			for {
				t := time.Now()
				if t.After(bmEnd) {
					break
				}
				sender(pos)
				atomic.AddUint64(&requestCount, 1)
			}
			wg.Done()
		}(i)
		go func(pos int) {
			for {
				t := time.Now()
				if t.After(bmEnd) {
					break
				}
				recver(pos)
				atomic.AddUint64(&responseCount, 1)
			}
			wg.Done()
		}(i)
	}
	wg.Wait()
	return requestCount, responseCount
}

func makeClient(benchFeatures stats.Features) (testpb.BenchmarkServiceClient, func()) {
	nw := &latency.Network{Kbps: benchFeatures.Kbps, Latency: benchFeatures.Latency, MTU: benchFeatures.Mtu}
	opts := []grpc.DialOption{}
	sopts := []grpc.ServerOption{}
	if benchFeatures.ModeCompressor == "nop" {
		sopts = append(sopts,
			grpc.RPCCompressor(nopCompressor{}),
			grpc.RPCDecompressor(nopDecompressor{}),
		)
		opts = append(opts,
			grpc.WithCompressor(nopCompressor{}),
			grpc.WithDecompressor(nopDecompressor{}),
		)
	}
	if benchFeatures.ModeCompressor == "gzip" {
		sopts = append(sopts,
			grpc.RPCCompressor(grpc.NewGZIPCompressor()),
			grpc.RPCDecompressor(grpc.NewGZIPDecompressor()),
		)
		opts = append(opts,
			grpc.WithCompressor(grpc.NewGZIPCompressor()),
			grpc.WithDecompressor(grpc.NewGZIPDecompressor()),
		)
	}
	sopts = append(sopts, grpc.MaxConcurrentStreams(uint32(benchFeatures.MaxConcurrentCalls+1)))
	opts = append(opts, grpc.WithInsecure())

	var lis net.Listener
	if *useBufconn {
		bcLis := bufconn.Listen(256 * 1024)
		lis = bcLis
		opts = append(opts, grpc.WithContextDialer(func(ctx context.Context, address string) (net.Conn, error) {
			return nw.ContextDialer(func(context.Context, string, string) (net.Conn, error) {
				return bcLis.Dial()
			})(ctx, "", "")
		}))
	} else {
		var err error
		lis, err = net.Listen("tcp", "localhost:0")
		if err != nil {
			grpclog.Fatalf("Failed to listen: %v", err)
		}
		opts = append(opts, grpc.WithContextDialer(func(ctx context.Context, address string) (net.Conn, error) {
			return nw.ContextDialer((&net.Dialer{}).DialContext)(ctx, "tcp", lis.Addr().String())
		}))
	}
	lis = nw.Listener(lis)
	stopper := bm.StartServer(bm.ServerInfo{Type: "protobuf", Listener: lis}, sopts...)
	conn := bm.NewClientConn("" /* target not used */, opts...)
	return testpb.NewBenchmarkServiceClient(conn), func() {
		conn.Close()
		stopper()
	}
}

func makeFuncUnary(benchFeatures stats.Features) (func(int), func()) {
	tc, cleanup := makeClient(benchFeatures)
	return func(int) {
		unaryCaller(tc, benchFeatures.ReqSizeBytes, benchFeatures.RespSizeBytes)
	}, cleanup
}

func makeFuncStream(benchFeatures stats.Features) (func(int), func()) {
	tc, cleanup := makeClient(benchFeatures)

	streams := make([]testpb.BenchmarkService_StreamingCallClient, benchFeatures.MaxConcurrentCalls)
	for i := 0; i < benchFeatures.MaxConcurrentCalls; i++ {
		stream, err := tc.StreamingCall(context.Background())
		if err != nil {
			grpclog.Fatalf("%v.StreamingCall(_) = _, %v", tc, err)
		}
		streams[i] = stream
	}

	return func(pos int) {
		streamCaller(streams[pos], benchFeatures.ReqSizeBytes, benchFeatures.RespSizeBytes)
	}, cleanup
}

func makeFuncUnconstrainedStreamPreloaded(benchFeatures stats.Features) (func(int), func(int), func()) {
	streams, req, cleanup := setupUnconstrainedStream(benchFeatures)

	preparedMsg := make([]*grpc.PreparedMsg, len(streams))
	for i, stream := range streams {
		preparedMsg[i] = &grpc.PreparedMsg{}
		err := preparedMsg[i].Encode(stream, req)
		if err != nil {
			grpclog.Fatalf("%v.Encode(%v, %v) = %v", preparedMsg[i], req, stream, err)
		}
	}

	return func(pos int) {
			streams[pos].SendMsg(preparedMsg[pos])
		}, func(pos int) {
			streams[pos].Recv()
		}, cleanup
}

func makeFuncUnconstrainedStream(benchFeatures stats.Features) (func(int), func(int), func()) {
	streams, req, cleanup := setupUnconstrainedStream(benchFeatures)

	return func(pos int) {
			streams[pos].Send(req)
		}, func(pos int) {
			streams[pos].Recv()
		}, cleanup
}

func setupUnconstrainedStream(benchFeatures stats.Features) ([]testpb.BenchmarkService_StreamingCallClient, *testpb.SimpleRequest, func()) {
	tc, cleanup := makeClient(benchFeatures)

	streams := make([]testpb.BenchmarkService_StreamingCallClient, benchFeatures.MaxConcurrentCalls)
	for i := 0; i < benchFeatures.MaxConcurrentCalls; i++ {
		stream, err := tc.UnconstrainedStreamingCall(context.Background())
		if err != nil {
			grpclog.Fatalf("%v.UnconstrainedStreamingCall(_) = _, %v", tc, err)
		}
		streams[i] = stream
	}

	pl := bm.NewPayload(testpb.PayloadType_COMPRESSABLE, benchFeatures.ReqSizeBytes)
	req := &testpb.SimpleRequest{
		ResponseType: pl.Type,
		ResponseSize: int32(benchFeatures.RespSizeBytes),
		Payload:      pl,
	}

	return streams, req, cleanup
}

func unaryCaller(client testpb.BenchmarkServiceClient, reqSize, respSize int) {
	if err := bm.DoUnaryCall(client, reqSize, respSize); err != nil {
		grpclog.Fatalf("DoUnaryCall failed: %v", err)
	}
}

func streamCaller(stream testpb.BenchmarkService_StreamingCallClient, reqSize, respSize int) {
	if err := bm.DoStreamingRoundTrip(stream, reqSize, respSize); err != nil {
		grpclog.Fatalf("DoStreamingRoundTrip failed: %v", err)
	}
}

func runBenchmark(caller func(int), startTimer func(), stopTimer func(uint64), benchFeatures stats.Features, benchtime time.Duration, s *stats.Stats) uint64 {
	// Warm up connection.
	for i := 0; i < 10; i++ {
		caller(0)
	}
	// Run benchmark.
	startTimer()
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	wg.Add(benchFeatures.MaxConcurrentCalls)
	bmEnd := time.Now().Add(benchtime)
	var count uint64
	for i := 0; i < benchFeatures.MaxConcurrentCalls; i++ {
		go func(pos int) {
			for {
				t := time.Now()
				if t.After(bmEnd) {
					break
				}
				start := time.Now()
				caller(pos)
				elapse := time.Since(start)
				atomic.AddUint64(&count, 1)
				mu.Lock()
				s.Add(elapse)
				mu.Unlock()
			}
			wg.Done()
		}(i)
	}
	wg.Wait()
	stopTimer(count)
	return count
}

var useBufconn = flag.Bool("bufconn", false, "Use in-memory connection instead of system network I/O")

// Initiate main function to get settings of features.
func init() {
	var (
		workloads, traceMode, compressorMode, readLatency, channelzOn string
		preloaderMode                                                 string
		readKbps, readMtu, readMaxConcurrentCalls                     intSliceType
		readReqSizeBytes, readRespSizeBytes                           intSliceType
	)
	flag.StringVar(&workloads, "workloads", workloadsAll,
		fmt.Sprintf("Workloads to execute - One of: %v", strings.Join(allWorkloads, ", ")))
	flag.StringVar(&traceMode, "trace", modeOff,
		fmt.Sprintf("Trace mode - One of: %v", strings.Join(allTraceModes, ", ")))
	flag.StringVar(&readLatency, "latency", "", "Simulated one-way network latency - may be a comma-separated list")
	flag.StringVar(&channelzOn, "channelz", modeOff, "whether channelz should be turned on")
	flag.DurationVar(&benchtime, "benchtime", time.Second, "Configures the amount of time to run each benchmark")
	flag.Var(&readKbps, "kbps", "Simulated network throughput (in kbps) - may be a comma-separated list")
	flag.Var(&readMtu, "mtu", "Simulated network MTU (Maximum Transmission Unit) - may be a comma-separated list")
	flag.Var(&readMaxConcurrentCalls, "maxConcurrentCalls", "Number of concurrent RPCs during benchmarks")
	flag.Var(&readReqSizeBytes, "reqSizeBytes", "Request size in bytes - may be a comma-separated list")
	flag.Var(&readRespSizeBytes, "respSizeBytes", "Response size in bytes - may be a comma-separated list")
	flag.StringVar(&memProfile, "memProfile", "", "Enables memory profiling output to the filename provided.")
	flag.IntVar(&memProfileRate, "memProfileRate", 512*1024, "Configures the memory profiling rate. \n"+
		"memProfile should be set before setting profile rate. To include every allocated block in the profile, "+
		"set MemProfileRate to 1. To turn off profiling entirely, set MemProfileRate to 0. 512 * 1024 by default.")
	flag.StringVar(&cpuProfile, "cpuProfile", "", "Enables CPU profiling output to the filename provided")
	flag.StringVar(&compressorMode, "compression", modeOff,
		fmt.Sprintf("Compression mode - One of: %v", strings.Join(allCompressionModes, ", ")))
	flag.StringVar(&preloaderMode, "preloader", modeOff,
		fmt.Sprintf("Preloader mode - One of: %v", strings.Join(allPreloaderModes, ", ")))
	flag.StringVar(&benchmarkResultFile, "resultFile", "", "Save the benchmark result into a binary file")
	flag.StringVar(&networkMode, "networkMode", "", "Network mode includes LAN, WAN, Local and Longhaul")
	flag.Parse()
	if flag.NArg() != 0 {
		log.Fatal("Error: unparsed arguments: ", flag.Args())
	}
	switch workloads {
	case workloadsUnary:
		runMode[0] = true
		runMode[1] = false
		runMode[2] = false
	case workloadsStreaming:
		runMode[0] = false
		runMode[1] = true
		runMode[2] = false
	case workloadsUnconstrained:
		runMode[0] = false
		runMode[1] = false
		runMode[2] = true
	case workloadsAll:
		runMode[0] = true
		runMode[1] = true
		runMode[2] = true
	default:
		log.Fatalf("Unknown workloads setting: %v (want one of: %v)",
			workloads, strings.Join(allWorkloads, ", "))
	}
	modeCompressor = setModeCompressor(compressorMode)
	enablePreloader = setMode(preloaderMode)
	enableTrace = setMode(traceMode)
	enableChannelz = setMode(channelzOn)
	// Time input formats as (time + unit).
	readTimeFromInput(&ltc, readLatency)
	readIntFromIntSlice(&kbps, readKbps)
	readIntFromIntSlice(&mtu, readMtu)
	readIntFromIntSlice(&maxConcurrentCalls, readMaxConcurrentCalls)
	readIntFromIntSlice(&reqSizeBytes, readReqSizeBytes)
	readIntFromIntSlice(&respSizeBytes, readRespSizeBytes)
	// Re-write latency, kpbs and mtu if network mode is set.
	if network, ok := networks[networkMode]; ok {
		ltc = []time.Duration{network.Latency}
		kbps = []int{network.Kbps}
		mtu = []int{network.MTU}
	}
}

func setMode(name string) []bool {
	switch name {
	case modeOn:
		return []bool{true}
	case modeOff:
		return []bool{false}
	case modeBoth:
		return []bool{false, true}
	default:
		log.Fatalf("Unknown %s setting: %v (want one of: %v)",
			name, name, strings.Join(allTraceModes, ", "))
		return []bool{}
	}
}

func setModeCompressor(name string) []string {
	switch name {
	case modeNop:
		return []string{"nop"}
	case modeGzip:
		return []string{"gzip"}
	case modeAll:
		return []string{"off", "nop", "gzip"}
	case modeOff:
		return []string{"off"}
	default:
		log.Fatalf("Unknown %s setting: %v (want one of: %v)",
			name, name, strings.Join(allCompressionModes, ", "))
		return []string{}
	}
}

type intSliceType []int

func (intSlice *intSliceType) String() string {
	return fmt.Sprintf("%v", *intSlice)
}

func (intSlice *intSliceType) Set(value string) error {
	if len(*intSlice) > 0 {
		return errors.New("interval flag already set")
	}
	for _, num := range strings.Split(value, ",") {
		next, err := strconv.Atoi(num)
		if err != nil {
			return err
		}
		*intSlice = append(*intSlice, next)
	}
	return nil
}

func readIntFromIntSlice(values *[]int, replace intSliceType) {
	// If not set replace in the flag, just return to run the default settings.
	if len(replace) == 0 {
		return
	}
	*values = replace
}

func readTimeFromInput(values *[]time.Duration, replace string) {
	if strings.Compare(replace, "") != 0 {
		*values = []time.Duration{}
		for _, ltc := range strings.Split(replace, ",") {
			duration, err := time.ParseDuration(ltc)
			if err != nil {
				log.Fatal(err.Error())
			}
			*values = append(*values, duration)
		}
	}
}

func printThroughput(requestCount uint64, requestSize int, responseCount uint64, responseSize int) {
	requestThroughput := float64(requestCount) * float64(requestSize) * 8 / benchtime.Seconds()
	responseThroughput := float64(responseCount) * float64(responseSize) * 8 / benchtime.Seconds()
	fmt.Printf("Number of requests:  %v\tRequest throughput:  %v bit/s\n", requestCount, requestThroughput)
	fmt.Printf("Number of responses: %v\tResponse throughput: %v bit/s\n", responseCount, responseThroughput)
	fmt.Println()
}

func main() {
	before()
	featuresPos := make([]int, 10)
	// 0:enableTracing 1:ltc 2:kbps 3:mtu 4:maxC 5:reqSize 6:respSize
	featuresNum := []int{len(enableTrace), len(ltc), len(kbps), len(mtu),
		len(maxConcurrentCalls), len(reqSizeBytes), len(respSizeBytes), len(modeCompressor), len(enableChannelz), len(enablePreloader)}
	initalPos := make([]int, len(featuresPos))
	s := stats.NewStats(10)
	s.SortLatency()
	var memStats runtime.MemStats
	var results testing.BenchmarkResult
	var startAllocs, startBytes uint64
	var startTime time.Time
	start := true
	var startTimer = func() {
		runtime.ReadMemStats(&memStats)
		startAllocs = memStats.Mallocs
		startBytes = memStats.TotalAlloc
		startTime = time.Now()
	}
	var stopTimer = func(count uint64) {
		runtime.ReadMemStats(&memStats)
		results = testing.BenchmarkResult{N: int(count), T: time.Since(startTime),
			Bytes: 0, MemAllocs: memStats.Mallocs - startAllocs, MemBytes: memStats.TotalAlloc - startBytes}
	}
	sharedPos := make([]bool, len(featuresPos))
	for i := 0; i < len(featuresPos); i++ {
		if featuresNum[i] <= 1 {
			sharedPos[i] = true
		}
	}

	// Run benchmarks
	resultSlice := []stats.BenchResults{}
	for !reflect.DeepEqual(featuresPos, initalPos) || start {
		start = false
		benchFeature := stats.Features{
			NetworkMode:        networkMode,
			EnableTrace:        enableTrace[featuresPos[0]],
			Latency:            ltc[featuresPos[1]],
			Kbps:               kbps[featuresPos[2]],
			Mtu:                mtu[featuresPos[3]],
			MaxConcurrentCalls: maxConcurrentCalls[featuresPos[4]],
			ReqSizeBytes:       reqSizeBytes[featuresPos[5]],
			RespSizeBytes:      respSizeBytes[featuresPos[6]],
			ModeCompressor:     modeCompressor[featuresPos[7]],
			EnableChannelz:     enableChannelz[featuresPos[8]],
			EnablePreloader:    enablePreloader[featuresPos[9]],
		}

		grpc.EnableTracing = enableTrace[featuresPos[0]]
		if enableChannelz[featuresPos[8]] {
			channelz.TurnOn()
		}
		if runMode[0] {
			count := unaryBenchmark(startTimer, stopTimer, benchFeature, benchtime, s)
			s.SetBenchmarkResult("Unary", benchFeature, results.N,
				results.AllocedBytesPerOp(), results.AllocsPerOp(), sharedPos)
			fmt.Println(s.BenchString())
			fmt.Println(s.String())
			printThroughput(count, benchFeature.ReqSizeBytes, count, benchFeature.RespSizeBytes)
			resultSlice = append(resultSlice, s.GetBenchmarkResults())
			s.Clear()
		}
		if runMode[1] {
			count := streamBenchmark(startTimer, stopTimer, benchFeature, benchtime, s)
			s.SetBenchmarkResult("Stream", benchFeature, results.N,
				results.AllocedBytesPerOp(), results.AllocsPerOp(), sharedPos)
			fmt.Println(s.BenchString())
			fmt.Println(s.String())
			printThroughput(count, benchFeature.ReqSizeBytes, count, benchFeature.RespSizeBytes)
			resultSlice = append(resultSlice, s.GetBenchmarkResults())
			s.Clear()
		}
		if runMode[2] {
			requestCount, responseCount := unconstrainedStreamBenchmark(benchFeature, time.Second, benchtime)
			fmt.Printf("Unconstrained Stream-%v\n", benchFeature)
			printThroughput(requestCount, benchFeature.ReqSizeBytes, responseCount, benchFeature.RespSizeBytes)
		}
		bm.AddOne(featuresPos, featuresNum)
	}
	after(resultSlice)
}

func before() {
	if memProfile != "" {
		runtime.MemProfileRate = memProfileRate
	}
	if cpuProfile != "" {
		f, err := os.Create(cpuProfile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "testing: %s\n", err)
			return
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			fmt.Fprintf(os.Stderr, "testing: can't start cpu profile: %s\n", err)
			f.Close()
			return
		}
	}
}

func after(data []stats.BenchResults) {
	if cpuProfile != "" {
		pprof.StopCPUProfile() // flushes profile to disk
	}
	if memProfile != "" {
		f, err := os.Create(memProfile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "testing: %s\n", err)
			os.Exit(2)
		}
		runtime.GC() // materialize all statistics
		if err = pprof.WriteHeapProfile(f); err != nil {
			fmt.Fprintf(os.Stderr, "testing: can't write heap profile %s: %s\n", memProfile, err)
			os.Exit(2)
		}
		f.Close()
	}
	if benchmarkResultFile != "" {
		f, err := os.Create(benchmarkResultFile)
		if err != nil {
			log.Fatalf("testing: can't write benchmark result %s: %s\n", benchmarkResultFile, err)
		}
		dataEncoder := gob.NewEncoder(f)
		dataEncoder.Encode(data)
		f.Close()
	}
}

// nopCompressor is a compressor that just copies data.
type nopCompressor struct{}

func (nopCompressor) Do(w io.Writer, p []byte) error {
	n, err := w.Write(p)
	if err != nil {
		return err
	}
	if n != len(p) {
		return fmt.Errorf("nopCompressor.Write: wrote %v bytes; want %v", n, len(p))
	}
	return nil
}

func (nopCompressor) Type() string { return "nop" }

// nopDecompressor is a decompressor that just copies data.
type nopDecompressor struct{}

func (nopDecompressor) Do(r io.Reader) ([]byte, error) { return ioutil.ReadAll(r) }
func (nopDecompressor) Type() string                   { return "nop" }

package state

import (
	"math"
	"math/rand/v2"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

type DataSource struct {
	Name string
	Data []time.Duration
}

//func generateMultiLinePlot(dataSources []DataSource, title string) (*plot.Plot, error) {
//	p := plot.New()
//
//	p.Title.Text = title
//	p.X.Label.Text = "Sample #"
//	p.Y.Label.Text = "Duration (ms)"
//
//	// Define a color palette for the lines
//	colors := []color.Color{
//		color.RGBA{R: 255, G: 0, B: 0, A: 255},   // Red
//		color.RGBA{R: 0, G: 0, B: 255, A: 255},   // Blue
//		color.RGBA{R: 0, G: 255, B: 0, A: 255},   // Green
//		color.RGBA{R: 255, G: 0, B: 255, A: 255}, // Magenta
//		color.RGBA{R: 0, G: 255, B: 255, A: 255}, // Cyan
//	}
//
//	for i, ds := range dataSources {
//		points := make(plotter.XYs, len(ds.Data))
//		for j, d := range ds.Data {
//			points[j].X = float64(j)
//			points[j].Y = float64(d.Milliseconds())
//		}
//
//		line, err := plotter.NewLine(points)
//		if err != nil {
//			return nil, fmt.Errorf("failed to create line for %s: %v", ds.Name, err)
//		}
//
//		line.Color = colors[i%len(colors)] // Cycle through colors
//		p.Add(line)
//		p.Legend.Add(ds.Name, line)
//	}
//
//	return p, nil
//}

func runTests(t *testing.T, ping func(i int) float64, dura time.Duration, fn string) (DataSource, DataSource) {
	t.Helper()
	tunables := DefaultRouterTunables()
	dep := NewEndpoint("127.0.0.1:0", false, nil, &tunables)

	truth := DataSource{
		Name: "Truth",
		Data: []time.Duration{},
	}

	low := DataSource{
		Name: "Low",
		Data: []time.Duration{},
	}

	high := DataSource{
		Name: "High",
		Data: []time.Duration{},
	}

	filtered := DataSource{
		Name: "Filtered",
		Data: []time.Duration{},
	}

	stabilized := DataSource{
		Name: "Stabilized",
		Data: []time.Duration{},
	}

	samples := int(dura / tunables.ProbeDelay)
	for i := 0; i < samples; i++ {
		nping := time.Duration(ping(i) * float64(time.Millisecond))
		dep.UpdatePing(nping)
		if i > tunables.MinimumConfidenceWindow {
			truth.Data = append(truth.Data, nping)
			high.Data = append(high.Data, dep.HighRange())
			low.Data = append(low.Data, dep.LowRange())
			filtered.Data = append(filtered.Data, dep.FilteredPing())
			stabilized.Data = append(stabilized.Data, dep.StabilizedPing())
		}
	}

	//dataSources := []DataSource{truth, high, low, filtered, stabilized}

	//p, err := generateMultiLinePlot(dataSources, "Comparison of ping and stabilized ping")
	//if err != nil {
	//	t.Fatal(err)
	//}
	//if err := p.Save(8*vg.Inch, 6*vg.Inch, spew.Sprintf("method_comparison_%s.png", fn)); err != nil {
	//	t.Fatalf("Failed to save plot: %v", err)
	//}

	return truth, stabilized
}

func TestEndpointSin(t *testing.T) {
	rng := rand.New(rand.NewPCG(0, 0))
	truth, finalFiltered := runTests(t, func(i int) float64 {
		val := math.Cos(float64(i)/1000.0-math.Pi/2) * 10
		if rng.Int()%30 == 0 {
			val += float64(rng.Int() % 20)
		}
		val2 := math.Sin(float64(i+400)/50.0)*2 + rng.Float64()
		val3 := math.Abs(rng.NormFloat64()) * 5
		return val + val2 + val3 + 75
	}, time.Hour*2, "sin")

	distinctValues := make(map[uint64]struct{})

	variance := 0.0
	for i, d := range finalFiltered.Data {
		distinctValues[uint64(d)] = struct{}{}
		diff := float64(d - truth.Data[i])
		variance += diff * diff
	}
	// deviation from pingY should be 10 + 5 + 2 = 17ms
	stdev := math.Sqrt(variance / float64(len(finalFiltered.Data)))
	assert.Less(t, time.Duration(stdev), time.Millisecond*20)
	assert.Less(t, len(distinctValues), int((time.Hour*2)/time.Minute))
}

func TestEndpointPosX(t *testing.T) {
	// absolute worst case scenario for number of metric changes
	rng := rand.New(rand.NewPCG(0, 0))
	truth, finalFiltered := runTests(t, func(i int) float64 {
		val := float64(i) / 50.0
		if rng.Int()%30 == 0 {
			val += float64(rng.Int() % 20)
		}
		val2 := math.Sin(float64(i+400)/50.0)*2 + rng.Float64()
		val3 := math.Abs(rng.NormFloat64()) * 5
		return val + val2 + val3 + 75
	}, time.Hour*2, "PosX")

	distinctValues := make(map[uint64]struct{})

	variance := 0.0
	for i, d := range finalFiltered.Data {
		distinctValues[uint64(d)] = struct{}{}
		diff := float64(d - truth.Data[i])
		variance += diff * diff
	}
	stdev := math.Sqrt(variance / float64(len(finalFiltered.Data)))
	assert.Less(t, time.Duration(stdev), time.Millisecond*20)
	// once per minute is acceptable
	assert.Less(t, len(distinctValues), int(time.Hour*2/time.Minute))
}

func TestEndpointNegX(t *testing.T) {
	// absolute worst case scenario for number of metric changes
	rng := rand.New(rand.NewPCG(0, 0))
	truth, finalFiltered := runTests(t, func(i int) float64 {
		val := -float64(i) / 50.0
		if rng.Int()%30 == 0 {
			val += float64(rng.Int() % 20)
		}
		val2 := math.Sin(float64(i+400)/50.0)*2 + rng.Float64()
		val3 := math.Abs(rng.NormFloat64()) * 5
		return val + val2 + val3 + 500
	}, time.Hour*2, "NegX")

	distinctValues := make(map[uint64]struct{})

	variance := 0.0
	for i, d := range finalFiltered.Data {
		distinctValues[uint64(d)] = struct{}{}
		diff := float64(d - truth.Data[i])
		variance += diff * diff
	}
	stdev := math.Sqrt(variance / float64(len(finalFiltered.Data)))
	assert.Less(t, time.Duration(stdev), time.Millisecond*40)
	// once per minute is acceptable
	assert.Less(t, len(distinctValues), int(time.Hour*2/time.Minute))
}

func TestEndpointNormal(t *testing.T) {
	// absolute worst case scenario for number of metric changes
	rng := rand.New(rand.NewPCG(0, 0))
	truth, finalFiltered := runTests(t, func(i int) float64 {
		return 50 + rng.NormFloat64()*10
	}, time.Hour*2, "normal")

	distinctValues := make(map[uint64]struct{})

	variance := 0.0
	for i, d := range finalFiltered.Data {
		distinctValues[uint64(d)] = struct{}{}
		diff := float64(d - truth.Data[i])
		variance += diff * diff
	}
	stdev := math.Sqrt(variance / float64(len(finalFiltered.Data)))
	assert.Less(t, time.Duration(stdev), time.Millisecond*40)
	// once per minute is acceptable
	assert.Less(t, len(distinctValues), int(time.Hour*2/time.Minute))
}

func TestParseEndpoint(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		expectedHost string
		expectedPort uint16
		wantErr      bool
	}{
		{
			name:         "IPv4 with port",
			input:        "127.0.0.1:12345",
			expectedHost: "127.0.0.1",
			expectedPort: 12345,
		},
		{
			name:         "IPv6 with port",
			input:        "[::1]:12345",
			expectedHost: "::1",
			expectedPort: 12345,
		},
		{
			name:         "Hostname with port",
			input:        "example.com:54321",
			expectedHost: "example.com",
			expectedPort: 54321,
		},
		{
			name:         "Hostname default port",
			input:        "nylon.example.com",
			expectedHost: "nylon.example.com",
			expectedPort: uint16(DefaultPort),
		},
		{
			name:         "IPv4 default port",
			input:        "192.168.1.1",
			expectedHost: "192.168.1.1",
			expectedPort: uint16(DefaultPort),
		},
		{
			name:    "Invalid port",
			input:   "example.com:abc",
			wantErr: true,
		},
		{
			name:    "Not a URL",
			input:   "http://example.com/nylon",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, port, err := parseEndpoint(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expectedHost, host)
				assert.Equal(t, tt.expectedPort, port)
			}
		})
	}
}

func newProbeTestEndpoint(minimumConfidence int) *NylonEndpoint {
	tunables := DefaultRouterTunables()
	tunables.MinimumConfidenceWindow = minimumConfidence
	dep := NewEndpoint("127.0.0.1:0", false, nil, &tunables)
	dep.Renew()
	return dep
}

func TestProbeLossDemotion(t *testing.T) {
	dep := newProbeTestEndpoint(5)
	for range 11 {
		dep.RecordProbe(false)
	}
	assert.True(t, dep.IsActive())
	assert.Equal(t, uint64(11), dep.ProbeTimeouts())
	assert.Equal(t, INF, dep.Metric())
}

func TestProbeLossDemotionNeedsConfidence(t *testing.T) {
	dep := newProbeTestEndpoint(15)
	for range 5 {
		dep.RecordProbe(false)
	}
	assert.NotEqual(t, INF, dep.Metric())
}

func TestProbeSuccessNoPenaltyWhenDisabled(t *testing.T) {
	tunables := DefaultRouterTunables()
	tunables.MinimumConfidenceWindow = 5
	tunables.ProbeTimeout = 0
	dep := NewEndpoint("127.0.0.1:0", false, nil, &tunables)
	dep.Renew()
	for range 15 {
		dep.UpdatePing(10 * time.Millisecond)
	}
	base := dep.Metric()
	for range 20 {
		dep.RecordProbe(false)
	}
	assert.Equal(t, base, dep.Metric())
	assert.Equal(t, uint64(0), dep.ProbeTimeouts())
	assert.Equal(t, float64(1), dep.ProbeSuccess())
}

func TestRenewWipesProbeStateOnSilenceRevival(t *testing.T) {
	dep := newProbeTestEndpoint(5)
	for range 11 {
		dep.RecordProbe(false)
	}
	assert.Equal(t, INF, dep.Metric())
	dep.Lock()
	dep.lastHeardBack = time.Now().Add(-2 * dep.t.LinkDeadThreshold)
	dep.Unlock()
	dep.Renew()
	assert.True(t, dep.IsActive())
	assert.Equal(t, float64(1), dep.ProbeSuccess())
	assert.NotEqual(t, INF, dep.Metric())
}

func TestPingRenewalKeepsLossDemotion(t *testing.T) {
	dep := newProbeTestEndpoint(5)
	for range 11 {
		dep.RecordProbe(false)
	}
	assert.Equal(t, INF, dep.Metric())
	dep.Renew()
	assert.Equal(t, INF, dep.Metric())
}

func TestProbeLossEstimate(t *testing.T) {
	alpha := float64(time.Second) / float64(2*time.Minute)
	dep := newProbeTestEndpoint(5)
	for range 15 {
		dep.UpdatePing(10 * time.Millisecond)
		dep.RecordProbe(true)
	}
	assert.Zero(t, dep.probeLoss, "successful probes keep the estimate at zero")
	for lost, want := range []uint32{10840, 11687, 12542, 13403, 14272, 15149} {
		dep.RecordProbe(false)
		assert.InDelta(t, 1-math.Pow(1-alpha, float64(lost+1)), dep.probeLoss, 1e-12,
			"%d consecutive timed-out probes", lost+1)
		assert.InDelta(t, math.Pow(1-ewmaAlpha, float64(lost+1)), dep.ProbeSuccess(), 1e-12,
			"the demotion EWMA keeps its own time constant")
		assert.Equal(t, want, dep.Metric(), "%d consecutive timed-out probes", lost+1)
	}
	dep.Lock()
	dep.lastHeardBack = time.Now().Add(-2 * dep.t.LinkDeadThreshold)
	dep.Unlock()
	dep.Renew()
	assert.Zero(t, dep.probeLoss, "a revival after silence starts the estimate over")
	assert.Equal(t, uint32(0), dep.probeSamples)
}

func confidentEndpoint(t *testing.T, lost float64) *NylonEndpoint {
	t.Helper()
	dep := newProbeTestEndpoint(5)
	for range 15 {
		dep.UpdatePing(10 * time.Millisecond)
		dep.RecordProbe(true)
	}
	dep.probeLoss = lost
	return dep
}

func TestEndpointMetric_LossCurve(t *testing.T) {
	cases := []struct {
		loss   float64
		factor float64
		metric uint32
	}{
		{0, 1, 10000}, {0.01, 1.10, 11010}, {0.02, 1.20, 12040}, {0.05, 1.53, 15263},
		{0.10, 2.11, 21111}, {0.20, 3.50, 35000}, {0.30, 5.29, 52857}, {0.39, 7.39, 73934},
	}
	prev := uint32(0)
	for _, tc := range cases {
		t.Run(strconv.FormatFloat(tc.loss, 'f', -1, 64), func(t *testing.T) {
			dep := confidentEndpoint(t, tc.loss)
			metric := dep.Metric()
			assert.Equal(t, tc.metric, metric)
			assert.InDelta(t, tc.factor, float64(metric)/10000, 0.006)
			assert.GreaterOrEqual(t, metric, prev, "the curve is monotonic in loss")
			assert.LessOrEqual(t, metric, uint32(10000*10), "never above base × cap")
			assert.Less(t, metric, INF, "below the demotion threshold the metric stays finite")
			prev = metric
		})
	}
}

func TestEndpointMetric_LossCap(t *testing.T) {
	for _, deadband := range []float64{1.1, 1.5} {
		tunables := DefaultRouterTunables()
		tunables.LinkSwitchDeadband = deadband
		tunables.MinimumConfidenceWindow = 5
		tunables.LossMetricGain = 100
		dep := NewEndpoint("127.0.0.1:0", false, nil, &tunables)
		dep.Renew()
		for range 15 {
			dep.UpdatePing(10 * time.Millisecond)
			dep.RecordProbe(true)
		}
		for loss, want := range map[float64]uint32{0.05: 62631, 0.39: 100000, 0.59: 100000, 1: 100000} {
			dep.probeLoss = loss
			assert.Equal(t, want, dep.Metric(), "deadband %v, loss %v", deadband, loss)
		}
		tunables.LossMetricGain = 10
		tunables.LossMetricCap = 2.5
		for loss, want := range map[float64]uint32{0.05: 15263, 0.30: 25000} {
			dep.probeLoss = loss
			assert.Equal(t, want, dep.Metric(), "deadband %v, loss %v", deadband, loss)
		}
	}
}

func TestEndpointMetric_NoiseGate(t *testing.T) {
	dep := newProbeTestEndpoint(15)
	for range 15 {
		dep.UpdatePing(10 * time.Millisecond)
	}
	base := dep.Metric()
	dep.probeLoss = 0.30
	for samples := range 15 {
		dep.probeSamples = uint32(samples)
		assert.Equal(t, base, dep.Metric(), "%d samples: the loss factor is 1", samples)
	}
	dep.probeSamples = 15
	assert.Equal(t, uint32(52857), dep.Metric(), "the factor applies from the MinimumConfidenceWindow-th sample")
}

func TestEndpointMetric_SubBucketBase(t *testing.T) {
	dep := newProbeTestEndpoint(5)
	for range 15 {
		dep.UpdatePing(60 * time.Microsecond)
		dep.RecordProbe(true)
	}
	assert.Equal(t, uint32(0), dep.Metric())
	dep.probeLoss = 0.20
	assert.Equal(t, uint32(875), dep.Metric(), "loss on a zero-metric link is charged on one bucket")
}

func TestDurationToMetric_Quantisation(t *testing.T) {
	// Round-half-up to 250 µs buckets. The measured drift endpoints 1811 and
	// 2173 land in DISTINCT buckets: quantisation alone cannot suppress the
	// measured drift — that is RouteSwitchMinImprovement's job; the bucket
	// only erases sub-250 µs wobble.
	for d, want := range map[time.Duration]uint32{
		0:                            0,
		124 * time.Microsecond:       0,
		125 * time.Microsecond:       250,
		249 * time.Microsecond:       250,
		250 * time.Microsecond:       250,
		374 * time.Microsecond:       250,
		375 * time.Microsecond:       500,
		1598 * time.Microsecond:      1500,
		1811 * time.Microsecond:      1750,
		2101 * time.Microsecond:      2000,
		2173 * time.Microsecond:      2250,
		time.Second:                  1_000_000,
		time.Duration(math.MaxInt64): INF,
		5000 * time.Second:           INF - 1,
	} {
		assert.Equal(t, want, DurationToMetric(d), "DurationToMetric(%s)", d)
	}
}

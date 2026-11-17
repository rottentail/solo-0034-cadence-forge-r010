package media

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// SilenceScan is measurement only. It describes quiet intervals in the exact
// bytes it was given; it does not decide what an edit should remove.
type SilenceScan struct {
	ThresholdDB   float64
	SoftCeilingDB float64
	PureFloorDB   float64
	MinSilenceMS  int64
	MinSoftMS     int64
	Candidates    []QuietInterval
}

type QuietInterval struct {
	StartMS    int64
	EndMS      int64
	PeakDB     *float64
	MeanRMSDB  *float64
	FrameCount int
	Pure       bool
}

const (
	DefaultSilenceThresholdDB = -35.0
	DefaultSoftCeilingDB      = -20.0
	DefaultMinSilenceMS       = 250
	DefaultMinSoftMS          = 120
	// Frames more than this far below the silence threshold carry no usable
	// level. They are treated as digital silence rather than as measured
	// quiet audio, and no numeric level is invented for them.
	pureFloorOffsetDB = 15.0
	scanFrameMS       = 10
	// A 120s source yields about 24k frames and two bounded level files.
	maxScanLog = 8 << 20
)

var (
	levelPattern = regexp.MustCompile(`=(-?(?:[0-9.]+|inf))$`)
)

// ScanSilence measures per-frame peak and RMS levels and groups frames into
// three honest bands: audible speech, low-level speech (breaths and soft word
// tails), and effectively digital silence. Measuring per frame keeps a quiet
// word tail from being absorbed into a neighbouring noise-gate verdict.
func ScanSilence(ctx context.Context, binary, path string, durationMS int64) (SilenceScan, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "cadence-silence-*")
	if err != nil {
		return SilenceScan{}, err
	}
	defer os.RemoveAll(dir)

	scan := SilenceScan{
		ThresholdDB:   DefaultSilenceThresholdDB,
		SoftCeilingDB: DefaultSoftCeilingDB,
		PureFloorDB:   DefaultSilenceThresholdDB - pureFloorOffsetDB,
		MinSilenceMS:  DefaultMinSilenceMS,
		MinSoftMS:     DefaultMinSoftMS,
	}
	levels, err := frameLevels(ctx, binary, path, dir)
	if err != nil {
		return SilenceScan{}, err
	}
	if len(levels) == 0 {
		return SilenceScan{}, fmt.Errorf("silence scan produced no audio frames")
	}
	scan.Candidates = classify(levels, scan, durationMS)
	return scan, nil
}

func frameLevels(ctx context.Context, binary, path, dir string) ([]frameLevel, error) {
	peakPath, rmsPath := filepath.Join(dir, "peak.txt"), filepath.Join(dir, "rms.txt")
	opts := strings.Join([]string{
		"asetnsamples=480:p=0",
		"astats=metadata=1:reset=1:measure_perchannel=none:measure_overall=Peak_level+RMS_level",
		"ametadata=mode=print:key=lavfi.astats.Overall.Peak_level:file=" + escapeFilterPath(peakPath),
		"ametadata=mode=print:key=lavfi.astats.Overall.RMS_level:file=" + escapeFilterPath(rmsPath),
	}, ",")
	args := []string{"-nostdin", "-hide_banner", "-nostats", "-loglevel", "error",
		"-protocol_whitelist", "file,pipe", "-format_whitelist", Formats,
		"-i", path, "-vn", "-map", "0:a:0", "-threads", "1", "-af", opts, "-f", "null", "-"}
	cmd := exec.CommandContext(ctx, binary, args...)
	var log LimitedBuffer
	log.Limit = 16384
	cmd.Stdout = &log
	cmd.Stderr = &log
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("level scan: %w: %s", err, log.String())
	}
	peaks, err := readLevels(peakPath)
	if err != nil {
		return nil, err
	}
	rmss, err := readLevels(rmsPath)
	if err != nil {
		return nil, err
	}
	n := min(len(peaks), len(rmss))
	out := make([]frameLevel, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, frameLevel{peak: peaks[i], rms: rmss[i]})
	}
	return out, nil
}

type frameLevel struct {
	peak *float64
	rms  *float64
}

func readLevels(path string) ([]*float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) > maxScanLog {
		return nil, fmt.Errorf("level output exceeded bound")
	}
	out := []*float64{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "frame:") {
			continue
		}
		m := levelPattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		out = append(out, finite(m[1]))
	}
	return out, nil
}

func escapeFilterPath(path string) string {
	path = strings.ReplaceAll(path, `\`, `\\`)
	path = strings.ReplaceAll(path, `:`, `\:`)
	path = strings.ReplaceAll(path, `'`, `\'`)
	return path
}

type band int

const (
	bandSpeech band = iota
	bandSoft
	bandSilent
)

// frameBand keeps unmeasured and effectively digital frames separate from
// measured low-level audio. "No level" is never turned into zero or -inf
// numbers in the output; it only selects the silent band.
func frameBand(level frameLevel, scan SilenceScan) band {
	if level.peak == nil || *level.peak <= scan.PureFloorDB {
		return bandSilent
	}
	if *level.peak < scan.SoftCeilingDB {
		return bandSoft
	}
	return bandSpeech
}

type run struct {
	band       band
	startFrame int
	endFrame   int
}

// classify groups frame bands into quiet intervals. Short runs are absorbed by
// their neighbours so one codec artifact cannot create a fake pause, and a
// short quiet gap inside speech stays protected as speech.
func classify(levels []frameLevel, scan SilenceScan, durationMS int64) []QuietInterval {
	runs := []run{}
	start := 0
	current := frameBand(levels[0], scan)
	for i := 1; i < len(levels); i++ {
		b := frameBand(levels[i], scan)
		if b != current {
			runs = append(runs, run{band: current, startFrame: start, endFrame: i})
			start, current = i, b
		}
	}
	runs = append(runs, run{band: current, startFrame: start, endFrame: len(levels)})
	runs = absorbShortRuns(runs, scan)

	out := []QuietInterval{}
	for _, r := range runs {
		if r.band == bandSpeech {
			continue
		}
		begin := int64(r.startFrame) * scanFrameMS
		finish := min(int64(r.endFrame)*scanFrameMS, durationMS)
		if finish <= begin {
			continue
		}
		minimum := scan.MinSilenceMS
		if r.band == bandSoft {
			minimum = scan.MinSoftMS
		}
		if finish-begin < minimum {
			continue
		}
		peak, rms, count := intervalLevels(levels, begin, finish)
		out = append(out, QuietInterval{StartMS: begin, EndMS: finish, PeakDB: peak, MeanRMSDB: rms, FrameCount: count, Pure: r.band == bandSilent})
	}
	return out
}

func absorbShortRuns(runs []run, scan SilenceScan) []run {
	if len(runs) == 0 {
		return runs
	}
	changed := true
	for changed {
		changed = false
		for i := 0; i < len(runs); i++ {
			r := runs[i]
			if r.band == bandSpeech {
				continue
			}
			duration := int64(r.endFrame-r.startFrame) * scanFrameMS
			minimum := scan.MinSilenceMS
			if r.band == bandSoft {
				minimum = scan.MinSoftMS
			}
			if duration >= minimum {
				continue
			}
			left, right := bandSpeech, bandSpeech
			if i > 0 {
				left = runs[i-1].band
			}
			if i+1 < len(runs) {
				right = runs[i+1].band
			}
			switch {
			case left == right:
				r.band = left
			case left == bandSoft || right == bandSoft:
				r.band = bandSoft
			default:
				r.band = bandSpeech
			}
			runs[i] = r
			changed = true
		}
		runs = coalesce(runs)
	}
	return runs
}

func coalesce(runs []run) []run {
	out := []run{}
	for _, r := range runs {
		if len(out) > 0 && out[len(out)-1].band == r.band {
			out[len(out)-1].endFrame = r.endFrame
			continue
		}
		out = append(out, r)
	}
	return out
}

func intervalLevels(levels []frameLevel, startMS, endMS int64) (peak, rms *float64, count int) {
	first := int(startMS / scanFrameMS)
	if first < 0 {
		first = 0
	}
	last := int((endMS + scanFrameMS - 1) / scanFrameMS)
	if last > len(levels) {
		last = len(levels)
	}
	if last <= first {
		return nil, nil, 0
	}
	var peakMax *float64
	total := 0.0
	n := 0
	for _, level := range levels[first:last] {
		if level.peak != nil && (peakMax == nil || *level.peak > *peakMax) {
			v := *level.peak
			peakMax = &v
		}
		if level.rms != nil {
			total += *level.rms
			n++
		}
	}
	if n > 0 {
		mean := total / float64(n)
		rms = &mean
	}
	return peakMax, rms, last - first
}

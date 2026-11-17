package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Silence analysis is evidence about exact source bytes. A ready revision is
// never edited: re-analysis appends another revision, and adoption pins the
// revision it reviewed. Editing policy (pauses, target, actual cut intervals)
// lives with the sheet-side decision, not in the analysis result.
const (
	AnalysisPending = "pending"
	AnalysisRunning = "running"
	AnalysisReady   = "ready"
	AnalysisFailed  = "failed"

	CandidateSilence    = "silence"
	CandidateSoftSpeech = "soft_speech"
	SuggestionCut       = "cut"
	SuggestionTradeoff  = "tradeoff"

	OptionKeepAll            = "keep_all"
	OptionSilenceOnly        = "silence_only"
	OptionTightenPauses      = "tighten_pauses"
	OptionSilenceAndSoftTail = "silence_and_soft_tails"
	OptionCustom             = "custom"

	DecisionCut  = "cut"
	DecisionKeep = "keep"

	MaxSilenceAnalyses = 32
	MaxSilenceCands    = 1024
)

type SilenceCandidate struct {
	ID         string   `json:"id"`
	SourceID   string   `json:"source_id"`
	Kind       string   `json:"kind"`
	StartMS    int64    `json:"start_ms"`
	EndMS      int64    `json:"end_ms"`
	DurationMS int64    `json:"duration_ms"`
	PeakDB     *float64 `json:"peak_db"`
	MeanRMSDB  *float64 `json:"mean_rms_db"`
	FrameCount int      `json:"frame_count"`
	Suggested  string   `json:"suggested"`
}

type SilenceAnalysis struct {
	SourceID       string             `json:"source_id"`
	SourceDigest   string             `json:"source_digest"`
	SourceBytes    int64              `json:"source_bytes"`
	Revision       int                `json:"revision"`
	State          string             `json:"state"`
	ThresholdDB    float64            `json:"threshold_db"`
	SoftCeilingDB  float64            `json:"soft_ceiling_db"`
	MinSilenceMS   int64              `json:"min_silence_ms"`
	MinSoftMS      int64              `json:"min_soft_ms"`
	CandidateCount int                `json:"candidate_count"`
	Candidates     []SilenceCandidate `json:"candidates"`
	Error          string             `json:"error,omitempty"`
	RequestedAt    time.Time          `json:"requested_at"`
	StartedAt      *time.Time         `json:"started_at,omitempty"`
	FinishedAt     *time.Time         `json:"finished_at,omitempty"`
}

type AnalysisRef struct {
	SourceID     string `json:"source_id"`
	Revision     int    `json:"revision"`
	SourceDigest string `json:"source_digest"`
	SourceBytes  int64  `json:"source_bytes"`
}

func Ref(analysis SilenceAnalysis) AnalysisRef {
	return AnalysisRef{SourceID: analysis.SourceID, Revision: analysis.Revision, SourceDigest: analysis.SourceDigest, SourceBytes: analysis.SourceBytes}
}

type AppliedCut struct {
	SpanIndex   int   `json:"span_index"`
	SourceInMS  int64 `json:"source_in_ms"`
	SourceOutMS int64 `json:"source_out_ms"`
}

// SilenceDecision records the edit-side judgment for one evidence interval,
// including deliberately kept intervals. Applied cuts name the spans and exact
// source bytes that were removed.
type SilenceDecision struct {
	SourceID    string       `json:"source_id"`
	CandidateID string       `json:"candidate_id"`
	Kind        string       `json:"kind"`
	StartMS     int64        `json:"start_ms"`
	EndMS       int64        `json:"end_ms"`
	Action      string       `json:"action"`
	CutInMS     int64        `json:"cut_in_ms"`
	CutOutMS    int64        `json:"cut_out_ms"`
	CutMS       int64        `json:"cut_ms"`
	Cuts        []AppliedCut `json:"cuts,omitempty"`
	Reason      string       `json:"reason"`
}

type CueImpact struct {
	CueIndex  int    `json:"cue_index"`
	SourceID  string `json:"source_id"`
	StartMS   int64  `json:"start_ms"`
	EndMS     int64  `json:"end_ms"`
	RemovedMS int64  `json:"removed_ms"`
	Action    string `json:"action"`
}

// Adoption is the edit-side record joining reviewed analysis revisions to the
// exact sheet revision they produced. Fingerprints prevent the analysis flow
// and later edits from drifting apart.
type Adoption struct {
	ID                string            `json:"id"`
	SheetID           string            `json:"sheet_id"`
	BaseRevision      int               `json:"base_revision"`
	ResultRevision    int               `json:"result_revision"`
	BaseFingerprint   string            `json:"base_fingerprint"`
	ResultFingerprint string            `json:"result_fingerprint"`
	Analyses          []AnalysisRef     `json:"analyses"`
	TargetMS          int64             `json:"target_ms"`
	NaturalPauseMS    int64             `json:"natural_pause_ms"`
	MinPauseMS        int64             `json:"min_pause_ms"`
	PadMS             int64             `json:"pad_ms"`
	DurationMS        int64             `json:"duration_ms"`
	Reached           bool              `json:"reached"`
	ShortfallMS       int64             `json:"shortfall_ms"`
	SoftSpeechCutMS   int64             `json:"soft_speech_cut_ms"`
	CueRemoved        int               `json:"cue_removed"`
	CueClipped        int               `json:"cue_clipped"`
	CueImpacts        []CueImpact       `json:"cue_impacts,omitempty"`
	Option            string            `json:"option"`
	Decisions         []SilenceDecision `json:"decisions"`
	CreatedAt         time.Time         `json:"created_at"`
}

func SheetFingerprint(s Sheet) string {
	payload := struct {
		Spans []Span `json:"spans"`
		Cues  []Cue  `json:"cues"`
	}{Spans: s.Spans, Cues: s.Cues}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func SpanDuration(spans []Span) int64 {
	var total int64
	for _, span := range spans {
		total += span.OutMS - span.InMS
	}
	return total
}

func OptionKnown(option string) bool {
	switch option {
	case OptionKeepAll, OptionSilenceOnly, OptionTightenPauses, OptionSilenceAndSoftTail, OptionCustom:
		return true
	}
	return false
}

package domain

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

func ValidateRecipe(r Recipe) error {
	if strings.TrimSpace(r.Name) == "" || len(r.Name) > 160 {
		return Invalid("recipe name must contain 1..160 bytes")
	}
	if r.Format != "mp4" && r.Format != "wav" {
		return Invalid("format must be mp4 or wav")
	}
	if r.SampleRate != 48000 && r.SampleRate != 44100 {
		return Invalid("sample_rate must be 44100 or 48000")
	}
	if r.Format == "mp4" {
		if r.Width < 64 || r.Height < 64 || r.Width > 1920 || r.Height > 1080 || r.Width%2 != 0 || r.Height%2 != 0 {
			return Invalid("invalid even video dimensions")
		}
		if r.FPS < 1 || r.FPS > 60 {
			return Invalid("fps must be 1..60")
		}
	}
	if r.Normalize && (math.IsNaN(r.TargetLUFS) || math.IsInf(r.TargetLUFS, 0) || r.TargetLUFS < -36 || r.TargetLUFS > -9) {
		return Invalid("normalization target must be -36..-9 LUFS")
	}
	return nil
}
func ValidateSheet(s Sheet, sources map[string]Source) error {
	if strings.TrimSpace(s.Name) == "" || len(s.Name) > 160 {
		return Invalid("sheet name must contain 1..160 bytes")
	}
	if len(s.Spans) == 0 || len(s.Spans) > 32 {
		return Invalid("a sheet requires 1..32 spans")
	}
	var duration int64
	for i, p := range s.Spans {
		source, ok := sources[p.SourceID]
		if !ok {
			return Missing("source", p.SourceID)
		}
		if p.InMS < 0 || p.OutMS <= p.InMS || p.OutMS > source.Media.DurationMS {
			return Invalid("span %d is outside source duration", i)
		}
		if p.OutMS-p.InMS < 100 {
			return Invalid("span %d is shorter than 100ms", i)
		}
		if math.IsNaN(p.GainDB) || math.IsInf(p.GainDB, 0) || p.GainDB < -60 || p.GainDB > 12 {
			return Invalid("span %d has invalid gain", i)
		}
		duration += p.OutMS - p.InMS
	}
	if duration > 120000 {
		return Invalid("sheet exceeds 120 seconds")
	}
	if len(s.Cues) > 500 {
		return Invalid("sheet exceeds 500 cues")
	}
	for i, c := range s.Cues {
		source, ok := sources[c.SourceID]
		if !ok {
			return Missing("cue source", c.SourceID)
		}
		if c.StartMS < 0 || c.EndMS <= c.StartMS || c.EndMS > source.Media.DurationMS {
			return Invalid("cue %d has invalid interval", i)
		}
		if !utf8.ValidString(c.Text) || strings.TrimSpace(c.Text) == "" || len(c.Text) > 2000 || strings.Contains(c.Text, "-->") || strings.ContainsAny(c.Text, "\x00\r") {
			return Invalid("cue %d has invalid text", i)
		}
	}
	return nil
}
func ValidateSilenceAnalysis(a SilenceAnalysis) error {
	switch a.State {
	case AnalysisPending, AnalysisRunning, AnalysisReady, AnalysisFailed:
	default:
		return Invalid("analysis state is invalid")
	}
	if a.Revision < 1 {
		return Invalid("analysis revision must be positive")
	}
	if len(a.SourceDigest) != 64 || a.SourceBytes <= 0 {
		return Invalid("analysis must bind positive source bytes and a digest")
	}
	if a.ThresholdDB > 0 || a.ThresholdDB < -90 || a.SoftCeilingDB > 0 || a.SoftCeilingDB < a.ThresholdDB {
		return Invalid("analysis level thresholds are invalid")
	}
	if a.MinSilenceMS < 50 || a.MinSilenceMS > 10000 || a.MinSoftMS < 20 || a.MinSoftMS > a.MinSilenceMS {
		return Invalid("analysis minimum intervals are invalid")
	}
	if len(a.Candidates) > MaxSilenceCands {
		return Invalid("analysis exceeds candidate bound")
	}
	previousEnd := int64(-1)
	for i, c := range a.Candidates {
		if c.Kind != CandidateSilence && c.Kind != CandidateSoftSpeech {
			return Invalid("candidate %d kind is invalid", i)
		}
		if c.StartMS < 0 || c.EndMS <= c.StartMS || c.DurationMS != c.EndMS-c.StartMS {
			return Invalid("candidate %d interval is invalid", i)
		}
		if c.StartMS < previousEnd {
			return Invalid("candidate %d overlaps or is unsorted", i)
		}
		previousEnd = c.EndMS
		if c.Suggested != SuggestionCut && c.Suggested != SuggestionTradeoff {
			return Invalid("candidate %d suggestion is invalid", i)
		}
		if c.Kind == CandidateSoftSpeech && c.Suggested == SuggestionCut && (c.PeakDB == nil || c.MeanRMSDB == nil) {
			return Invalid("candidate %d cannot cut unmeasured low-level speech", i)
		}
		if c.Kind == CandidateSilence && c.EndMS-c.StartMS < a.MinSilenceMS {
			return Invalid("candidate %d is shorter than the silence minimum", i)
		}
		if c.Kind == CandidateSoftSpeech && c.EndMS-c.StartMS < a.MinSoftMS {
			return Invalid("candidate %d is shorter than the soft-speech minimum", i)
		}
	}
	if a.State == AnalysisReady && a.FinishedAt == nil {
		return Invalid("ready analysis requires a finish time")
	}
	if a.State == AnalysisFailed && a.Error == "" {
		return Invalid("failed analysis requires an error")
	}
	return nil
}
func ValidateState(s State) error {
	if s.Schema != 1 || s.Sources == nil || s.Sheets == nil || s.Recipes == nil || s.Renders == nil || s.Analyses == nil {
		return fmt.Errorf("unsupported or incomplete state schema")
	}
	for id, source := range s.Sources {
		if source.ID != id || len(source.Digest) != 64 || source.Bytes <= 0 {
			return fmt.Errorf("invalid source %s", id)
		}
	}
	for id, versions := range s.Sheets {
		if len(versions) == 0 {
			return fmt.Errorf("empty sheet history")
		}
		for i, v := range versions {
			if v.ID != id || v.Revision != i+1 {
				return fmt.Errorf("invalid sheet history")
			}
			if err := ValidateSheet(v, s.Sources); err != nil {
				return err
			}
		}
	}
	for id, versions := range s.Recipes {
		if len(versions) == 0 {
			return fmt.Errorf("empty recipe history")
		}
		for i, v := range versions {
			if v.ID != id || v.Revision != i+1 {
				return fmt.Errorf("invalid recipe history")
			}
			if err := ValidateRecipe(v); err != nil {
				return err
			}
		}
	}
	for id, versions := range s.Analyses {
		if len(versions) == 0 {
			return fmt.Errorf("empty analysis history")
		}
		for i, v := range versions {
			if v.SourceID != id || v.Revision != i+1 {
				return fmt.Errorf("invalid analysis history")
			}
			if err := ValidateSilenceAnalysis(v); err != nil {
				return err
			}
			source, ok := s.Sources[id]
			if ok && (source.Digest != v.SourceDigest || source.Bytes != v.SourceBytes) {
				return fmt.Errorf("analysis %s#%d is not bound to current source bytes", id, v.Revision)
			}
		}
	}
	keys := map[string]bool{}
	for id, decision := range s.Adoptions {
		sheet, ok := s.Sheets[decision.SheetID]
		if !ok || id != decision.ID {
			return fmt.Errorf("invalid adoption identity")
		}
		if decision.ResultRevision < 1 || decision.ResultRevision > len(sheet) {
			return fmt.Errorf("invalid adoption result revision")
		}
		if decision.BaseRevision < 0 || decision.BaseRevision >= decision.ResultRevision {
			return fmt.Errorf("invalid adoption base revision")
		}
		if sheet[decision.ResultRevision-1].Adoption != id {
			return fmt.Errorf("adoption is not referenced by its sheet revision")
		}
		if sheet[decision.ResultRevision-1].Revision != decision.ResultRevision {
			return fmt.Errorf("adoption revision drifted")
		}
		for _, ref := range decision.Analyses {
			versions := s.Analyses[ref.SourceID]
			if ref.Revision < 1 || ref.Revision > len(versions) {
				return fmt.Errorf("adoption references unknown analysis")
			}
			if versions[ref.Revision-1].State != AnalysisReady {
				return fmt.Errorf("adoption references an analysis that is not ready")
			}
			if versions[ref.Revision-1].SourceDigest != ref.SourceDigest || versions[ref.Revision-1].SourceBytes != ref.SourceBytes {
				return fmt.Errorf("adoption analysis bytes drifted")
			}
		}
	}
	for id, r := range s.Renders {
		if id != r.ID || keys[r.Key] {
			return fmt.Errorf("invalid render identity")
		}
		keys[r.Key] = true
		switch r.State {
		case "queued", "running", "succeeded", "failed", "cancelled":
		default:
			return fmt.Errorf("invalid render state")
		}
		if (r.State == "succeeded") != (r.Artifact != nil) {
			return fmt.Errorf("artifact state mismatch")
		}
		if r.Sources == nil {
			return fmt.Errorf("missing render source snapshot")
		}
		if err := ValidateSheet(r.Sheet, r.Sources); err != nil {
			return err
		}
		if err := ValidateRecipe(r.Recipe); err != nil {
			return err
		}
		if r.Adoption != nil {
			adoption, ok := s.Adoptions[r.Adoption.ID]
			if !ok {
				return fmt.Errorf("render references unknown silence adoption")
			}
			if adoption.SheetID != r.Sheet.ID || adoption.ResultRevision != r.Sheet.Revision {
				return fmt.Errorf("render adoption does not match its frozen sheet")
			}
			if adoption.ResultFingerprint != SheetFingerprint(r.Sheet) {
				return fmt.Errorf("render sheet drifted from its adoption")
			}
			if len(r.Silence) != len(adoption.Analyses) {
				return fmt.Errorf("render analysis refs do not match its adoption")
			}
		}
		for _, ref := range r.Silence {
			versions := s.Analyses[ref.SourceID]
			if ref.Revision < 1 || ref.Revision > len(versions) {
				return fmt.Errorf("render references unknown silence analysis")
			}
			analysis := versions[ref.Revision-1]
			if analysis.SourceDigest != ref.SourceDigest || analysis.SourceBytes != ref.SourceBytes {
				return fmt.Errorf("render analysis bytes drifted")
			}
		}
	}
	var previous uint64
	for _, e := range s.Events {
		if e.Sequence <= previous || e.Sequence > s.Sequence {
			return fmt.Errorf("event sequence is invalid")
		}
		previous = e.Sequence
	}
	return nil
}

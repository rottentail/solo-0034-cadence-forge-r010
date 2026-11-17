package service

import (
	"cadenceforge/internal/domain"
	"cadenceforge/internal/inspect"
	"cadenceforge/internal/media"
	"cadenceforge/internal/silence"
	"context"
	"fmt"
	"log/slog"
	"time"
)

// RequestAnalysis appends one pending analysis revision for a source and
// returns immediately. Media work happens in a dedicated analysis worker, so
// editing, previewing and rendering stay available while the scan runs.
func (s *Service) RequestAnalysis(sourceID string) (domain.SilenceAnalysis, error) {
	s.mutation.Lock()
	defer s.mutation.Unlock()
	var out domain.SilenceAnalysis
	err := s.Store.Update(func(st *domain.State) error {
		source, ok := st.Sources[sourceID]
		if !ok {
			return domain.Missing("source", sourceID)
		}
		if !source.Media.Audio {
			return domain.Invalid("source has no audio")
		}
		versions := st.Analyses[sourceID]
		if len(versions) >= domain.MaxSilenceAnalyses {
			return domain.Conflict("silence analysis history is full; a source supports at most 32 revisions")
		}
		now := time.Now().UTC()
		out = domain.SilenceAnalysis{
			SourceID: sourceID, SourceDigest: source.Digest, SourceBytes: source.Bytes,
			Revision: len(versions) + 1, State: domain.AnalysisPending,
			ThresholdDB: media.DefaultSilenceThresholdDB, SoftCeilingDB: media.DefaultSoftCeilingDB,
			MinSilenceMS: media.DefaultMinSilenceMS, MinSoftMS: media.DefaultMinSoftMS,
			Candidates: []domain.SilenceCandidate{}, RequestedAt: now,
		}
		st.Analyses[sourceID] = append(versions, out)
		st.Emit(sourceID, "silence.requested", fmt.Sprintf("revision %d binds digest %s", out.Revision, source.Digest))
		return nil
	})
	if err == nil {
		s.Notify()
	}
	return out, err
}

func (s *Service) Analyses(sourceID string) ([]domain.SilenceAnalysis, error) {
	st, err := s.Store.Snapshot()
	if err != nil {
		return nil, err
	}
	if _, ok := st.Sources[sourceID]; !ok {
		return nil, domain.Missing("source", sourceID)
	}
	return st.Analyses[sourceID], nil
}

func (s *Service) Analysis(sourceID string, revision int) (domain.SilenceAnalysis, error) {
	st, err := s.Store.Snapshot()
	if err != nil {
		return domain.SilenceAnalysis{}, err
	}
	versions := st.Analyses[sourceID]
	if revision < 1 || revision > len(versions) {
		return domain.SilenceAnalysis{}, domain.Missing("silence analysis revision", fmt.Sprintf("%s#%d", sourceID, revision))
	}
	return versions[revision-1], nil
}

// RunAnalysis performs one pending revision. It is scheduled by its own worker
// so that long renders cannot starve analysis, and a later re-analysis never
// rewrites an earlier ready revision. The first return value reports whether
// this call claimed the revision.
func (s *Service) RunAnalysis(ctx context.Context, sourceID string, revision int) (bool, error) {
	analysis, source, claimed, err := s.claimAnalysis(sourceID, revision)
	if err != nil || !claimed {
		return claimed, err
	}
	path, err := s.Blobs.Object(analysis.SourceDigest)
	if err == nil {
		var digest string
		var n int64
		digest, n, err = inspect.Digest(path)
		if err == nil && (digest != analysis.SourceDigest || n != analysis.SourceBytes) {
			err = fmt.Errorf("source bytes no longer match the analyzed object")
		}
	}
	var scan media.SilenceScan
	if err == nil {
		// A cancelled process must not leave the revision looking runnable.
		if ctx.Err() != nil {
			err = fmt.Errorf("analysis canceled: %w", ctx.Err())
		} else {
			scan, err = media.ScanSilence(ctx, s.Config.FFmpeg, path, source.Media.DurationMS)
			if err != nil && ctx.Err() != nil {
				err = fmt.Errorf("analysis canceled: %w", ctx.Err())
			}
		}
	}
	s.finishAnalysis(sourceID, revision, scan, err)
	return true, err
}

// claimAnalysis moves one pending revision to running. A ready or failed
// revision is never edited again: re-analysis always appends a new revision, so
// adoption pins exactly what was reviewed.
func (s *Service) claimAnalysis(sourceID string, revision int) (domain.SilenceAnalysis, domain.Source, bool, error) {
	s.mutation.Lock()
	defer s.mutation.Unlock()
	var analysis domain.SilenceAnalysis
	var source domain.Source
	claimed := false
	err := s.Store.Update(func(st *domain.State) error {
		versions := st.Analyses[sourceID]
		if revision < 1 || revision > len(versions) {
			return domain.Missing("silence analysis revision", fmt.Sprintf("%s#%d", sourceID, revision))
		}
		analysis = versions[revision-1]
		if analysis.State != domain.AnalysisPending {
			return nil
		}
		current, ok := st.Sources[sourceID]
		if !ok {
			return domain.Conflict("source was retired after analysis was requested")
		}
		if current.Digest != analysis.SourceDigest || current.Bytes != analysis.SourceBytes {
			return domain.Conflict("source bytes changed since analysis was requested")
		}
		source = current
		now := time.Now().UTC()
		analysis.State = domain.AnalysisRunning
		analysis.StartedAt = &now
		versions[revision-1] = analysis
		st.Analyses[sourceID] = versions
		st.Emit(sourceID, "silence.started", fmt.Sprint(revision))
		claimed = true
		return nil
	})
	return analysis, source, claimed, err
}

func (s *Service) finishAnalysis(sourceID string, revision int, scan media.SilenceScan, cause error) {
	s.mutation.Lock()
	defer s.mutation.Unlock()
	err := s.Store.Update(func(st *domain.State) error {
		versions := st.Analyses[sourceID]
		if revision < 1 || revision > len(versions) {
			return nil
		}
		analysis := versions[revision-1]
		if analysis.State != domain.AnalysisRunning {
			return nil
		}
		now := time.Now().UTC()
		analysis.FinishedAt = &now
		if cause != nil {
			analysis.State = domain.AnalysisFailed
			analysis.Error = truncate(cause.Error(), 512)
			analysis.Candidates = []domain.SilenceCandidate{}
		} else {
			analysis.State = domain.AnalysisReady
			analysis.ThresholdDB = scan.ThresholdDB
			analysis.SoftCeilingDB = scan.SoftCeilingDB
			analysis.MinSilenceMS = scan.MinSilenceMS
			analysis.MinSoftMS = scan.MinSoftMS
			analysis.Candidates = candidatesFor(sourceID, revision, scan)
		}
		versions[revision-1] = analysis
		st.Analyses[sourceID] = versions
		if cause != nil {
			st.Emit(sourceID, "silence.failed", analysis.Error)
		} else {
			st.Emit(sourceID, "silence.ready", fmt.Sprintf("revision %d measured %d intervals", revision, len(analysis.Candidates)))
		}
		return nil
	})
	if err != nil {
		slog.Error("commit silence analysis", "source", sourceID, "revision", revision, "error", err)
	}
}

func truncate(text string, limit int) string {
	if len(text) > limit {
		return text[:limit]
	}
	return text
}

// candidatesFor turns measurements into reviewable evidence. A candidate is a
// suggestion, never an automatic edit: measured low-level audio is always a
// tradeoff, and intervals with no level at all stay honest nulls.
func candidatesFor(sourceID string, revision int, scan media.SilenceScan) []domain.SilenceCandidate {
	prefix := sourceID
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	out := make([]domain.SilenceCandidate, 0, len(scan.Candidates))
	for index, interval := range scan.Candidates {
		kind := domain.CandidateSilence
		suggested := domain.SuggestionCut
		if !interval.Pure {
			// Above the unmeasurable floor: quiet but audible, so removing it is
			// always a reviewed tradeoff rather than a free silence cut.
			kind = domain.CandidateSoftSpeech
			suggested = domain.SuggestionTradeoff
		}
		out = append(out, domain.SilenceCandidate{
			ID: fmt.Sprintf("%s-r%d-%d", prefix, revision, index+1), SourceID: sourceID, Kind: kind,
			StartMS: interval.StartMS, EndMS: interval.EndMS, DurationMS: interval.EndMS - interval.StartMS,
			PeakDB: interval.PeakDB, MeanRMSDB: interval.MeanRMSDB, FrameCount: interval.FrameCount, Suggested: suggested,
		})
		if len(out) >= domain.MaxSilenceCands {
			break
		}
	}
	return out
}

func (s *Service) evidence(st domain.State, refs []domain.AnalysisRef) ([]silence.Evidence, error) {
	if len(refs) == 0 {
		return nil, domain.Invalid("at least one analysis reference is required")
	}
	if len(refs) > domain.MaxSilenceAnalyses {
		return nil, domain.Invalid("too many analysis references")
	}
	seen := map[string]bool{}
	out := []silence.Evidence{}
	for _, ref := range refs {
		if seen[ref.SourceID] {
			return nil, domain.Invalid("duplicate analysis reference for source %s", ref.SourceID)
		}
		seen[ref.SourceID] = true
		versions := st.Analyses[ref.SourceID]
		if ref.Revision < 1 || ref.Revision > len(versions) {
			return nil, domain.Missing("silence analysis revision", fmt.Sprintf("%s#%d", ref.SourceID, ref.Revision))
		}
		analysis := versions[ref.Revision-1]
		if analysis.State != domain.AnalysisReady {
			return nil, domain.Conflict(fmt.Sprintf("silence analysis %s#%d is %s, not ready", ref.SourceID, ref.Revision, analysis.State))
		}
		if analysis.SourceDigest != ref.SourceDigest || analysis.SourceBytes != ref.SourceBytes {
			return nil, domain.Conflict("analysis reference does not match the measured bytes")
		}
		source, ok := st.Sources[ref.SourceID]
		if !ok {
			return nil, domain.Missing("source", ref.SourceID)
		}
		if source.Digest != analysis.SourceDigest || source.Bytes != analysis.SourceBytes {
			return nil, domain.Conflict("source bytes changed; request a fresh silence analysis")
		}
		out = append(out, silence.Evidence{SourceID: source.ID, Revision: analysis.Revision, Digest: analysis.SourceDigest, Bytes: analysis.SourceBytes, Candidates: analysis.Candidates})
	}
	return out, nil
}

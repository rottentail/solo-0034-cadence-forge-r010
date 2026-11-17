package service

import (
	"cadenceforge/internal/caption"
	"cadenceforge/internal/domain"
	"cadenceforge/internal/silence"
	"cadenceforge/internal/timeline"
	"fmt"
	"time"
)

// SilenceOptions is the edit-side input for reviewing measured quiet intervals.
// The target is a composition length; the option states which tradeoffs the
// reviewer is willing to consider.
type SilenceOptions struct {
	TargetMS       int64                    `json:"target_ms"`
	Option         string                   `json:"option"`
	NaturalPauseMS int64                    `json:"natural_pause_ms,omitempty"`
	MinPauseMS     int64                    `json:"min_pause_ms,omitempty"`
	PadMS          int64                    `json:"pad_ms,omitempty"`
	Analyses       []domain.AnalysisRef     `json:"analyses"`
	Decisions      []domain.SilenceDecision `json:"decisions,omitempty"`
}

type SilenceProposal struct {
	SheetID      string           `json:"sheet_id"`
	BaseRevision int              `json:"base_revision"`
	Proposal     silence.Proposal `json:"proposal"`
	Preview      domain.Plan      `json:"preview"`
	PreviewReady bool             `json:"preview_ready"`
}

func (s *Service) sheetVersion(st domain.State, sheetID string, revision int) (domain.Sheet, error) {
	versions := st.Sheets[sheetID]
	if revision < 1 || revision > len(versions) {
		return domain.Sheet{}, domain.Missing("sheet revision", fmt.Sprintf("%s#%d", sheetID, revision))
	}
	return versions[revision-1], nil
}

func (s *Service) silenceRequest(st domain.State, sheet domain.Sheet, options SilenceOptions) (silence.Request, error) {
	used := map[string]bool{}
	for _, span := range sheet.Spans {
		used[span.SourceID] = true
	}
	referenced := map[string]bool{}
	for _, ref := range options.Analyses {
		if _, ok := st.Sources[ref.SourceID]; !ok {
			return silence.Request{}, domain.Missing("source", ref.SourceID)
		}
		if !used[ref.SourceID] {
			return silence.Request{}, domain.Invalid("analysis source %s is not used by sheet revision %d", ref.SourceID, sheet.Revision)
		}
		referenced[ref.SourceID] = true
	}
	// A partial analysis set would silently ignore speech-bearing sources, so
	// every source that contributes spans must have reviewed evidence.
	for sourceID := range used {
		if !referenced[sourceID] {
			return silence.Request{}, domain.Invalid("sheet source %s has no reviewed analysis; analyze every span source before reviewing silence", sourceID)
		}
	}
	evidence, err := s.evidence(st, options.Analyses)
	if err != nil {
		return silence.Request{}, err
	}
	request := silence.Request{
		Spans: sheet.Spans, Cues: sheet.Cues, Sources: st.Sources, Evidence: evidence,
		TargetMS: options.TargetMS, Option: options.Option,
		NaturalPauseMS: options.NaturalPauseMS, MinPauseMS: options.MinPauseMS, PadMS: options.PadMS,
		Custom: options.Decisions,
	}
	silence.PauseDefaults(&request)
	return request, nil
}

// ProposeSilence reviews measured intervals against a frozen sheet revision
// without saving anything. It reuses analysis output and never runs media tools.
func (s *Service) ProposeSilence(sheetID string, revision int, options SilenceOptions) (SilenceProposal, error) {
	st, err := s.Store.Snapshot()
	if err != nil {
		return SilenceProposal{}, err
	}
	sheet, err := s.sheetVersion(st, sheetID, revision)
	if err != nil {
		return SilenceProposal{}, err
	}
	request, err := s.silenceRequest(st, sheet, options)
	if err != nil {
		return SilenceProposal{}, err
	}
	proposal, err := silence.Plan(request)
	if err != nil {
		return SilenceProposal{}, err
	}
	proposal.SheetID = sheetID
	proposal.BaseRevision = sheet.Revision
	proposal.BaseFingerprint = domain.SheetFingerprint(sheet)
	out := SilenceProposal{SheetID: sheetID, BaseRevision: sheet.Revision, Proposal: proposal}
	if !proposal.Feasible {
		return out, nil
	}
	candidate := domain.Sheet{Name: sheet.Name, Spans: proposal.Spans, Cues: sheet.Cues}
	if err := domain.ValidateSheet(candidate, st.Sources); err != nil {
		out.Proposal.Feasible = false
		out.Proposal.Problem = err.Error()
		return out, nil
	}
	plan, err := timeline.Build(candidate, domain.Recipe{Name: "preview", Format: "wav", SampleRate: 48000}, st.Sources)
	if err != nil {
		out.Proposal.Feasible = false
		out.Proposal.Problem = err.Error()
		return out, nil
	}
	plan.VTT = caption.VTT(caption.Map(candidate, plan))
	out.Preview = plan
	out.PreviewReady = true
	return out, nil
}

// AdoptSilence appends a new sheet revision from a reviewed proposal. It is a
// compare-and-swap on the sheet revision: a proposal reviewed against revision
// N is refused once any later edit is confirmed, so re-analysis and old
// suggestions cannot overwrite newer editing decisions. The sheet revision, its
// adoption record, and its analysis references commit in one durable update.
func (s *Service) AdoptSilence(sheetID string, baseRevision int, options SilenceOptions) (domain.Sheet, domain.Adoption, error) {
	if options.Option == domain.OptionKeepAll {
		return domain.Sheet{}, domain.Adoption{}, domain.Invalid("keep_all changes nothing; review it instead of adopting it")
	}
	s.mutation.Lock()
	defer s.mutation.Unlock()
	var outSheet domain.Sheet
	var outAdoption domain.Adoption
	err := s.Store.Update(func(st *domain.State) error {
		versions := st.Sheets[sheetID]
		if len(versions) == 0 {
			return domain.Missing("sheet", sheetID)
		}
		if len(versions) != baseRevision {
			return domain.Conflict(fmt.Sprintf("sheet is at revision %d, not %d; the reviewed proposal is stale and cannot overwrite later edits", len(versions), baseRevision))
		}
		sheet, err := s.sheetVersion(*st, sheetID, baseRevision)
		if err != nil {
			return err
		}
		request, err := s.silenceRequest(*st, sheet, options)
		if err != nil {
			return err
		}
		proposal, err := silence.Plan(request)
		if err != nil {
			return err
		}
		if !proposal.Feasible {
			return domain.Conflict("proposal cannot be adopted: " + proposal.Problem)
		}
		if proposal.ResultMS == proposal.CurrentMS {
			return domain.Conflict("proposal removes no source bytes; no sheet revision would change")
		}
		candidate := domain.Sheet{ID: sheetID, Name: sheet.Name, Spans: proposal.Spans, Cues: sheet.Cues}
		if err := domain.ValidateSheet(candidate, st.Sources); err != nil {
			return err
		}
		if _, err := timeline.Build(candidate, domain.Recipe{Name: "adoption-check", Format: "wav", SampleRate: 48000}, st.Sources); err != nil {
			return err
		}
		now := time.Now().UTC()
		adoption := domain.Adoption{
			ID: ID(), SheetID: sheetID, BaseRevision: sheet.Revision, ResultRevision: sheet.Revision + 1,
			BaseFingerprint: domain.SheetFingerprint(sheet), Analyses: proposal.Analyses,
			TargetMS: options.TargetMS, NaturalPauseMS: proposal.NaturalPauseMS,
			MinPauseMS: proposal.MinPauseMS, PadMS: proposal.PadMS,
			DurationMS: proposal.ResultMS, Reached: proposal.Reached, ShortfallMS: proposal.ShortfallMS,
			SoftSpeechCutMS: proposal.SoftSpeechCutMS, Option: options.Option,
			Decisions: proposal.Decisions, CueImpacts: proposal.CueImpacts, CreatedAt: now,
		}
		for _, impact := range proposal.CueImpacts {
			if impact.Action == "removed" {
				adoption.CueRemoved++
				continue
			}
			adoption.CueClipped++
		}
		next := candidate
		next.Revision = sheet.Revision + 1
		next.Adoption = adoption.ID
		next.CreatedAt = now
		adoption.ResultFingerprint = domain.SheetFingerprint(next)
		st.Sheets[sheetID] = append(st.Sheets[sheetID], next)
		st.Adoptions[adoption.ID] = adoption
		outSheet, outAdoption = next, adoption
		st.Emit(sheetID, "sheet.silence_adopted", fmt.Sprintf("revision %d from revision %d: %dms removed toward %dms target; reached=%t shortfall=%dms", next.Revision, sheet.Revision, proposal.CurrentMS-proposal.ResultMS, proposal.TargetMS, proposal.Reached, proposal.ShortfallMS))
		for _, ref := range proposal.Analyses {
			st.Emit(ref.SourceID, "silence.adopted", fmt.Sprintf("analysis %s#%d into sheet %s revision %d", ref.SourceID, ref.Revision, sheetID, next.Revision))
		}
		return nil
	})
	return outSheet, outAdoption, err
}

// Adoption returns the recorded edit-side judgment behind a sheet revision.
func (s *Service) Adoption(id string) (domain.Adoption, error) {
	st, err := s.Store.Snapshot()
	if err != nil {
		return domain.Adoption{}, err
	}
	adoption, ok := st.Adoptions[id]
	if !ok {
		return domain.Adoption{}, domain.Missing("silence adoption", id)
	}
	return adoption, nil
}

// adoptionsFor returns the analysis evidence a sheet revision carries, so a
// render freezes the same judgment the editor adopted.
func adoptionsFor(st domain.State, sheet domain.Sheet) ([]domain.AnalysisRef, *domain.Adoption) {
	if sheet.Adoption == "" {
		return nil, nil
	}
	adoption, ok := st.Adoptions[sheet.Adoption]
	if !ok {
		return nil, nil
	}
	copy := adoption
	return append([]domain.AnalysisRef{}, adoption.Analyses...), &copy
}

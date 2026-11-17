package silence

import (
	"fmt"
	"sort"

	"cadenceforge/internal/domain"
)

// Evidence is the measurement side resolved for planning: one ready analysis
// revision per source, bound to the source bytes by digest and size.
type Evidence struct {
	SourceID   string
	Revision   int
	Digest     string
	Bytes      int64
	Candidates []domain.SilenceCandidate
}

// Request carries both sides of the contract: measured evidence and the edit
// judgment (target, option, pause policy or explicit per-interval decisions).
type Request struct {
	Spans          []domain.Span
	Cues           []domain.Cue
	Sources        map[string]domain.Source
	Evidence       []Evidence
	TargetMS       int64
	Option         string
	NaturalPauseMS int64
	MinPauseMS     int64
	PadMS          int64
	Custom         []domain.SilenceDecision
}

type Choice struct {
	Option          string `json:"option"`
	Reached         bool   `json:"reached"`
	Feasible        bool   `json:"feasible"`
	ResultMS        int64  `json:"result_ms"`
	ShortfallMS     int64  `json:"shortfall_ms"`
	SilenceCutMS    int64  `json:"silence_cut_ms"`
	SoftSpeechCutMS int64  `json:"soft_speech_cut_ms"`
	Note            string `json:"note"`
}

type Available struct {
	SilenceConservativeMS int64 `json:"silence_conservative_ms"`
	SilenceAggressiveMS   int64 `json:"silence_aggressive_ms"`
	SoftSpeechMS          int64 `json:"soft_speech_ms"`
	MaxRemovableMS        int64 `json:"max_removable_ms"`
	ShortfallAtMaxMS      int64 `json:"shortfall_at_max_ms"`
}

type Proposal struct {
	SheetID         string                   `json:"sheet_id"`
	BaseRevision    int                      `json:"base_revision"`
	BaseFingerprint string                   `json:"base_fingerprint"`
	Option          string                   `json:"option"`
	TargetMS        int64                    `json:"target_ms"`
	CurrentMS       int64                    `json:"current_ms"`
	ResultMS        int64                    `json:"result_ms"`
	Reached         bool                     `json:"reached"`
	ShortfallMS     int64                    `json:"shortfall_ms"`
	Feasible        bool                     `json:"feasible"`
	Problem         string                   `json:"problem,omitempty"`
	NaturalPauseMS  int64                    `json:"natural_pause_ms"`
	MinPauseMS      int64                    `json:"min_pause_ms"`
	PadMS           int64                    `json:"pad_ms"`
	Available       Available                `json:"available"`
	Choices         []Choice                 `json:"choices"`
	Analyses        []domain.AnalysisRef     `json:"analyses"`
	Warnings        []string                 `json:"warnings"`
	Decisions       []domain.SilenceDecision `json:"decisions"`
	CueImpacts      []domain.CueImpact       `json:"cue_impacts"`
	Spans           []domain.Span            `json:"spans"`
	SilenceCutMS    int64                    `json:"silence_cut_ms"`
	SoftSpeechCutMS int64                    `json:"soft_speech_cut_ms"`
}

const (
	defaultNaturalPauseMS = 400
	defaultMinPauseMS     = 180
	defaultPadMS          = 30
	minPieceMS            = 100
	maxSpanCount          = 32
)

// PauseDefaults fills the pause policy used when a request does not state one.
func PauseDefaults(req *Request) {
	if req.NaturalPauseMS == 0 {
		req.NaturalPauseMS = defaultNaturalPauseMS
	}
	if req.MinPauseMS == 0 {
		req.MinPauseMS = defaultMinPauseMS
	}
	if req.PadMS == 0 {
		req.PadMS = defaultPadMS
	}
}

// Validate rejects planning inputs that cannot describe a reviewable edit.
func Validate(req Request) error {
	if !domain.OptionKnown(req.Option) {
		return domain.Invalid("option must be keep_all, silence_only, tighten_pauses, silence_and_soft_tails or custom")
	}
	if req.TargetMS < minPieceMS || req.TargetMS > 120000 {
		return domain.Invalid("target_ms must be 100..120000")
	}
	if req.NaturalPauseMS < 120 || req.NaturalPauseMS > 3000 {
		return domain.Invalid("natural_pause_ms must be 120..3000")
	}
	if req.MinPauseMS < 80 || req.MinPauseMS > req.NaturalPauseMS {
		return domain.Invalid("min_pause_ms must be 80..natural_pause_ms")
	}
	if req.PadMS < 10 || req.PadMS > 250 {
		return domain.Invalid("pad_ms must be 10..250")
	}
	seen := map[string]bool{}
	for _, ev := range req.Evidence {
		if seen[ev.SourceID] {
			return domain.Invalid("duplicate analysis for source %s", ev.SourceID)
		}
		seen[ev.SourceID] = true
	}
	return nil
}

type candidateView struct {
	evidence  Evidence
	candidate domain.SilenceCandidate
}

// segment is one measured quiet interval clipped to one span. Planning per
// segment keeps every applied cut tied to a span index and exact source bytes.
type segment struct {
	view                        candidateView
	spanIndex                   int
	startMS                     int64
	endMS                       int64
	beforeNatural, afterNatural int64
	beforeMinimal, afterMinimal int64
}

func (s segment) duration() int64     { return s.endMS - s.startMS }
func (s segment) keepNatural() int64  { return s.beforeNatural + s.afterNatural }
func (s segment) keepMinimal() int64  { return s.beforeMinimal + s.afterMinimal }
func (s segment) conservative() int64 { return max(0, s.duration()-s.keepNatural()) }
func (s segment) aggressive() int64   { return max(0, s.duration()-s.keepMinimal()) }

// pauseKeeps counts the pause bytes retained on each side of a possible cut.
// A side that touches a span or source edge keeps nothing: there is no speech
// on that side of the visible interval to protect. Each speech-facing side
// keeps half the pause policy plus padding.
func pauseKeeps(span domain.Span, candidate domain.SilenceCandidate, naturalPause, minPause, pad, sourceDurationMS int64) (beforeNatural, afterNatural, beforeMinimal, afterMinimal int64) {
	speechBefore := candidate.StartMS > 0 && candidate.StartMS > span.InMS
	speechAfter := candidate.EndMS < sourceDurationMS && candidate.EndMS < span.OutMS
	if speechBefore {
		beforeNatural = naturalPause/2 + pad
		beforeMinimal = minPause/2 + pad
	}
	if speechAfter {
		afterNatural = naturalPause/2 + pad
		afterMinimal = minPause/2 + pad
	}
	return
}

func buildSegments(req Request, views []candidateView) []segment {
	spansBySource := map[string][]int{}
	for index, span := range req.Spans {
		spansBySource[span.SourceID] = append(spansBySource[span.SourceID], index)
	}
	out := []segment{}
	for _, view := range views {
		source := req.Sources[view.evidence.SourceID]
		for _, spanIndex := range spansBySource[view.evidence.SourceID] {
			span := req.Spans[spanIndex]
			start, end := max(view.candidate.StartMS, span.InMS), min(view.candidate.EndMS, span.OutMS)
			if end-start < 1 {
				continue
			}
			beforeNatural, afterNatural, beforeMinimal, afterMinimal := pauseKeeps(span, view.candidate, req.NaturalPauseMS, req.MinPauseMS, req.PadMS, source.Media.DurationMS)
			out = append(out, segment{view: view, spanIndex: spanIndex, startMS: start, endMS: end,
				beforeNatural: beforeNatural, afterNatural: afterNatural, beforeMinimal: beforeMinimal, afterMinimal: afterMinimal})
		}
	}
	return out
}

// cutWindow places a cut in the pause bytes that are free under the chosen
// pause policy. Retained pause stays next to the speech on each side, and a
// partial cut is centred in what remains so cut boundaries stay away from
// audible material.
func cutWindow(seg segment, amount, before, after int64) (int64, int64) {
	if amount <= 0 {
		return 0, 0
	}
	if before < 0 {
		before = 0
	}
	if after < 0 {
		after = 0
	}
	if amount > seg.duration() {
		amount = seg.duration()
	}
	lo := seg.startMS + before
	hi := seg.endMS - after
	if hi < lo {
		hi = lo
	}
	room := hi - lo
	if amount > room {
		amount = room
	}
	if amount <= 0 {
		return 0, 0
	}
	start := lo + (room-amount)/2
	return start, start + amount
}

type removal struct {
	sourceID string
	startMS  int64
	endMS    int64
}

type outcome struct {
	decisions  []domain.SilenceDecision
	removals   []removal
	silenceCut int64
	softCut    int64
	spans      []domain.Span
	impacts    []domain.CueImpact
	resultMS   int64
	feasible   bool
	problem    string
}

// Plan builds a reviewable proposal. It never mutates a sheet, never runs
// media tools, and never treats low-level speech as free silence: soft
// intervals are cut only under the explicit option or an explicit custom
// decision. Every measured interval receives a recorded decision.
func Plan(req Request) (Proposal, error) {
	if err := Validate(req); err != nil {
		return Proposal{}, err
	}
	proposal := Proposal{
		Option:         req.Option,
		TargetMS:       req.TargetMS,
		CurrentMS:      domain.SpanDuration(req.Spans),
		NaturalPauseMS: req.NaturalPauseMS,
		MinPauseMS:     req.MinPauseMS,
		PadMS:          req.PadMS,
		Feasible:       true,
		Choices:        []Choice{},
		Analyses:       []domain.AnalysisRef{},
		Warnings:       []string{},
		Decisions:      []domain.SilenceDecision{},
		CueImpacts:     []domain.CueImpact{},
		Spans:          req.Spans,
	}
	sort.SliceStable(req.Evidence, func(i, j int) bool { return req.Evidence[i].SourceID < req.Evidence[j].SourceID })
	for _, ev := range req.Evidence {
		source, ok := req.Sources[ev.SourceID]
		if !ok {
			return proposal, domain.Invalid("analysis source %s is not part of the sheet", ev.SourceID)
		}
		if ev.Digest != source.Digest || ev.Bytes != source.Bytes {
			return proposal, domain.Conflict("analysis bytes do not match source " + ev.SourceID)
		}
		proposal.Analyses = append(proposal.Analyses, domain.AnalysisRef{SourceID: ev.SourceID, Revision: ev.Revision, SourceDigest: ev.Digest, SourceBytes: ev.Bytes})
	}

	views := []candidateView{}
	for _, ev := range req.Evidence {
		for _, c := range ev.Candidates {
			views = append(views, candidateView{evidence: ev, candidate: c})
		}
	}
	segments := buildSegments(req, views)
	for _, seg := range segments {
		if seg.view.candidate.Kind == domain.CandidateSoftSpeech {
			proposal.Available.SoftSpeechMS += seg.aggressive()
			continue
		}
		proposal.Available.SilenceConservativeMS += seg.conservative()
		proposal.Available.SilenceAggressiveMS += seg.aggressive()
	}
	proposal.Available.MaxRemovableMS = proposal.Available.SilenceAggressiveMS + proposal.Available.SoftSpeechMS
	proposal.Available.ShortfallAtMaxMS = max(0, proposal.CurrentMS-proposal.Available.MaxRemovableMS-req.TargetMS)

	selected, err := selectCuts(req, req.Option, views, segments)
	if err != nil {
		return proposal, err
	}
	if selected.resultMS < proposal.CurrentMS {
		proposal.Warnings = append(proposal.Warnings, fmt.Sprintf("this option removes %dms of source bytes", proposal.CurrentMS-selected.resultMS))
	}
	if selected.softCut > 0 {
		proposal.Warnings = append(proposal.Warnings, fmt.Sprintf("%dms of low-level speech would be removed; review each tradeoff decision", selected.softCut))
	}
	if !selected.feasible {
		proposal.Warnings = append(proposal.Warnings, "this option cannot be adopted as-is: "+selected.problem)
	} else if selected.resultMS > req.TargetMS {
		proposal.Warnings = append(proposal.Warnings, fmt.Sprintf("the target stays %dms short; options that keep every pause cannot remove more", selected.resultMS-req.TargetMS))
	}
	if proposal.Available.ShortfallAtMaxMS > 0 {
		proposal.Warnings = append(proposal.Warnings, fmt.Sprintf("even removing every measured quiet interval leaves a %dms shortfall; the source has no further removable pause without cutting speech", proposal.Available.ShortfallAtMaxMS))
	}
	proposal.Decisions = selected.decisions
	proposal.Spans = selected.spans
	proposal.CueImpacts = selected.impacts
	proposal.ResultMS = selected.resultMS
	proposal.SilenceCutMS = selected.silenceCut
	proposal.SoftSpeechCutMS = selected.softCut
	proposal.Reached = selected.feasible && selected.resultMS <= req.TargetMS
	proposal.ShortfallMS = max(0, selected.resultMS-req.TargetMS)
	proposal.Feasible = selected.feasible
	proposal.Problem = selected.problem
	proposal.Choices = choices(req, views, segments)
	return proposal, nil
}

// selectCuts is the policy step: it decides which measured intervals to remove
// and how much of each, then rebuilds spans from those removals. Captions stay
// source-anchored, so the plan's caption mapping clips them the same way the
// render will.
func selectCuts(req Request, option string, views []candidateView, segments []segment) (outcome, error) {
	out := outcome{feasible: true, spans: req.Spans, decisions: []domain.SilenceDecision{}, impacts: []domain.CueImpact{}}
	out.resultMS = domain.SpanDuration(req.Spans)
	order := []string{}
	byID := map[string]candidateView{}
	for _, view := range views {
		order = append(order, view.candidate.ID)
		byID[view.candidate.ID] = view
	}
	decided := map[string]domain.SilenceDecision{}
	keep := func(view candidateView, reason string) {
		if _, ok := decided[view.candidate.ID]; ok {
			return
		}
		decided[view.candidate.ID] = domain.SilenceDecision{
			SourceID: view.evidence.SourceID, CandidateID: view.candidate.ID, Kind: view.candidate.Kind,
			StartMS: view.candidate.StartMS, EndMS: view.candidate.EndMS, Action: domain.DecisionKeep, Reason: reason,
		}
	}
	finish := func() (outcome, error) {
		for _, id := range order {
			if _, ok := decided[id]; !ok {
				keep(byID[id], "kept; not needed to approach the target")
			}
			out.decisions = append(out.decisions, decided[id])
		}
		spans, err := rebuildSpans(req.Spans, out.removals)
		if err != nil {
			out.feasible = false
			out.problem = err.Error()
			out.spans = nil
			out.resultMS = domain.SpanDuration(req.Spans) - out.silenceCut - out.softCut
			return out, nil
		}
		out.spans = spans
		out.resultMS = domain.SpanDuration(spans)
		out.impacts = cueImpacts(req.Cues, spans)
		if out.resultMS < minPieceMS {
			out.feasible = false
			out.problem = "the requested cuts would leave less than one 100ms span"
		}
		return out, nil
	}

	current := domain.SpanDuration(req.Spans)
	if option == domain.OptionKeepAll || current <= req.TargetMS {
		reason := "target already met; interval retained"
		if option == domain.OptionKeepAll {
			reason = "keep_all selected; no source bytes removed"
		}
		for _, view := range views {
			keep(view, reason)
		}
		return finish()
	}

	if option == domain.OptionCustom {
		index := map[string]domain.SilenceDecision{}
		for _, decision := range req.Custom {
			if _, ok := byID[decision.CandidateID]; !ok {
				return outcome{}, domain.Invalid("custom decision references unknown candidate %s", decision.CandidateID)
			}
			index[decision.CandidateID] = decision
		}
		for _, id := range order {
			view := byID[id]
			decision, ok := index[id]
			if !ok {
				keep(view, "no explicit decision; retained")
				continue
			}
			if decision.Action == domain.DecisionKeep {
				keep(view, "retained by explicit review")
				continue
			}
			if decision.Action != domain.DecisionCut {
				return outcome{}, domain.Invalid("custom decision action must be cut or keep")
			}
			limit := int64(0)
			for _, seg := range segments {
				if seg.view.candidate.ID == id {
					limit += seg.aggressive()
				}
			}
			amount := decision.CutMS
			if amount == 0 {
				amount = limit
			}
			if amount < 0 || amount > limit {
				return outcome{}, domain.Invalid("custom cut for %s must be 0..%dms", id, limit)
			}
			if amount == 0 {
				keep(view, "no cuttable room at the chosen pause policy")
				continue
			}
			applied, cuts := applyCandidate(id, segments, amount, &out)
			if applied == 0 {
				keep(view, "nothing to remove inside the current sheet spans")
				continue
			}
			decided[id] = cutDecision(view, applied, cuts, "cut by explicit review")
		}
		return finish()
	}

	aggressive := option == domain.OptionTightenPauses || option == domain.OptionSilenceAndSoftTail
	allowSoft := option == domain.OptionSilenceAndSoftTail
	ranked := append([]segment{}, segments...)
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		as, bs := a.view.candidate.Kind == domain.CandidateSoftSpeech, b.view.candidate.Kind == domain.CandidateSoftSpeech
		if as != bs {
			return !as
		}
		// Pauses that carry caption text are cut last so natural pauses near
		// dialogue survive as long as possible.
		ac, bc := carriesText(req.Cues, a), carriesText(req.Cues, b)
		if ac != bc {
			return !ac
		}
		if a.duration() != b.duration() {
			return a.duration() > b.duration()
		}
		if a.view.candidate.StartMS != b.view.candidate.StartMS {
			return a.view.candidate.StartMS < b.view.candidate.StartMS
		}
		return a.spanIndex < b.spanIndex
	})
	remaining := current - req.TargetMS
	for _, seg := range ranked {
		if remaining <= 0 {
			break
		}
		if seg.view.candidate.Kind == domain.CandidateSoftSpeech && !allowSoft {
			continue
		}
		limit := seg.conservative()
		beforeKeep, afterKeep := seg.beforeNatural, seg.afterNatural
		if aggressive {
			limit = seg.aggressive()
			beforeKeep, afterKeep = seg.beforeMinimal, seg.afterMinimal
		}
		if limit <= 0 {
			continue
		}
		amount := min(limit, remaining)
		start, end := cutWindow(seg, amount, beforeKeep, afterKeep)
		if end <= start {
			continue
		}
		if removesWholeCue(req.Cues, removal{sourceID: seg.view.evidence.SourceID, startMS: start, endMS: end}) {
			keep(seg.view, "retained because removing this interval would delete caption text")
			continue
		}
		applied, cuts := applySegment(seg, start, end, &out)
		if applied == 0 {
			continue
		}
		remaining -= applied
		reason := "redundant pause beyond the retained natural pause"
		if seg.view.candidate.Kind == domain.CandidateSoftSpeech {
			reason = "low-level speech (breath or soft tail); removing it is a reviewed tradeoff"
		}
		if existing, ok := decided[seg.view.candidate.ID]; ok && existing.Action == domain.DecisionCut {
			existing.CutMS += applied
			existing.Cuts = append(existing.Cuts, cuts...)
			existing.CutInMS = min(existing.CutInMS, start)
			existing.CutOutMS = max(existing.CutOutMS, end)
			decided[seg.view.candidate.ID] = existing
			continue
		}
		decided[seg.view.candidate.ID] = cutDecision(seg.view, applied, cuts, reason)
	}
	return finish()
}

// applyCandidate distributes one explicitly reviewed amount across every span
// showing the same measured interval, longest first. An explicit amount is
// honoured by keeping only the minimum pause on each speech-facing side, since
// the reviewer chose the exact bytes; the retained minimum is still recorded in
// the proposal's pause policy.
func applyCandidate(candidateID string, segments []segment, amount int64, out *outcome) (int64, []domain.AppliedCut) {
	matches := []segment{}
	for _, seg := range segments {
		if seg.view.candidate.ID == candidateID {
			matches = append(matches, seg)
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].aggressive() != matches[j].aggressive() {
			return matches[i].aggressive() > matches[j].aggressive()
		}
		return matches[i].spanIndex < matches[j].spanIndex
	})
	var applied int64
	var cuts []domain.AppliedCut
	for _, seg := range matches {
		if amount <= 0 {
			break
		}
		take := min(seg.aggressive(), amount)
		if take <= 0 {
			continue
		}
		start, end := cutWindow(seg, take, seg.beforeMinimal, seg.afterMinimal)
		if end <= start {
			continue
		}
		n, made := applySegment(seg, start, end, out)
		applied += n
		amount -= n
		cuts = append(cuts, made...)
	}
	return applied, cuts
}

func applySegment(seg segment, start, end int64, out *outcome) (int64, []domain.AppliedCut) {
	if end <= start {
		return 0, nil
	}
	out.removals = append(out.removals, removal{sourceID: seg.view.evidence.SourceID, startMS: start, endMS: end})
	if seg.view.candidate.Kind == domain.CandidateSoftSpeech {
		out.softCut += end - start
	} else {
		out.silenceCut += end - start
	}
	return end - start, []domain.AppliedCut{{SpanIndex: seg.spanIndex, SourceInMS: start, SourceOutMS: end}}
}

func cutDecision(view candidateView, applied int64, cuts []domain.AppliedCut, reason string) domain.SilenceDecision {
	decision := domain.SilenceDecision{
		SourceID: view.evidence.SourceID, CandidateID: view.candidate.ID, Kind: view.candidate.Kind,
		StartMS: view.candidate.StartMS, EndMS: view.candidate.EndMS, Action: domain.DecisionCut,
		CutMS: applied, Cuts: cuts, Reason: reason, CutInMS: view.candidate.StartMS, CutOutMS: view.candidate.EndMS,
	}
	for i, cut := range cuts {
		if i == 0 {
			decision.CutInMS = cut.SourceInMS
			decision.CutOutMS = cut.SourceOutMS
			continue
		}
		decision.CutInMS = min(decision.CutInMS, cut.SourceInMS)
		decision.CutOutMS = max(decision.CutOutMS, cut.SourceOutMS)
	}
	return decision
}

func removesWholeCue(cues []domain.Cue, r removal) bool {
	for _, cue := range cues {
		if cue.SourceID != r.sourceID {
			continue
		}
		if cue.StartMS >= r.startMS && cue.EndMS <= r.endMS && cue.EndMS > cue.StartMS {
			return true
		}
	}
	return false
}

func carriesText(cues []domain.Cue, seg segment) bool {
	for _, cue := range cues {
		if cue.SourceID != seg.view.evidence.SourceID {
			continue
		}
		if min(cue.EndMS, seg.endMS) > max(cue.StartMS, seg.startMS) {
			return true
		}
	}
	return false
}

func rebuildSpans(spans []domain.Span, removals []removal) ([]domain.Span, error) {
	bySource := groupRemovals(removals)
	out := []domain.Span{}
	for _, span := range spans {
		pieces, err := subtractSpan(span, bySource[span.SourceID])
		if err != nil {
			return nil, err
		}
		out = append(out, pieces...)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("removing these intervals would delete every span")
	}
	if len(out) > maxSpanCount {
		return nil, fmt.Errorf("this plan would need %d spans; a sheet allows at most %d", len(out), maxSpanCount)
	}
	for _, span := range out {
		if span.OutMS-span.InMS < minPieceMS {
			return nil, fmt.Errorf("this plan would leave a %dms span (%d..%dms); spans require at least %dms", span.OutMS-span.InMS, span.InMS, span.OutMS, minPieceMS)
		}
	}
	return out, nil
}

func groupRemovals(removals []removal) map[string][]removal {
	bySource := map[string][]removal{}
	for _, r := range removals {
		bySource[r.sourceID] = append(bySource[r.sourceID], r)
	}
	for id := range bySource {
		sort.SliceStable(bySource[id], func(i, j int) bool { return bySource[id][i].startMS < bySource[id][j].startMS })
		bySource[id] = mergeRemovals(bySource[id])
	}
	return bySource
}

func mergeRemovals(in []removal) []removal {
	out := []removal{}
	for _, r := range in {
		if r.endMS <= r.startMS {
			continue
		}
		if len(out) > 0 && r.startMS <= out[len(out)-1].endMS {
			if r.endMS > out[len(out)-1].endMS {
				out[len(out)-1].endMS = r.endMS
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

func subtractSpan(span domain.Span, removals []removal) ([]domain.Span, error) {
	pieces := []domain.Span{}
	cursor := span.InMS
	for _, r := range removals {
		start, end := max(span.InMS, r.startMS), min(span.OutMS, r.endMS)
		if end <= start {
			continue
		}
		if start > cursor {
			pieces = append(pieces, domain.Span{SourceID: span.SourceID, InMS: cursor, OutMS: start, GainDB: span.GainDB})
		}
		cursor = max(cursor, end)
	}
	if cursor < span.OutMS {
		pieces = append(pieces, domain.Span{SourceID: span.SourceID, InMS: cursor, OutMS: span.OutMS, GainDB: span.GainDB})
	}
	if len(pieces) == 0 {
		return nil, fmt.Errorf("span %d..%dms would be removed entirely", span.InMS, span.OutMS)
	}
	return pieces, nil
}

// cueImpacts reports how the resulting spans clip source-anchored captions.
// Cue text is never rewritten: the caption mapping derives the delivered
// captions from these spans and cues, exactly as the render will.
func cueImpacts(cues []domain.Cue, spans []domain.Span) []domain.CueImpact {
	spansBySource := map[string][]domain.Span{}
	for _, span := range spans {
		spansBySource[span.SourceID] = append(spansBySource[span.SourceID], span)
	}
	impacts := []domain.CueImpact{}
	for index, cue := range cues {
		total := cue.EndMS - cue.StartMS
		kept := unionOverlap(cue.StartMS, cue.EndMS, spansBySource[cue.SourceID])
		if kept >= total {
			continue
		}
		impact := domain.CueImpact{CueIndex: index, SourceID: cue.SourceID, StartMS: cue.StartMS, EndMS: cue.EndMS, RemovedMS: total - kept}
		switch {
		case kept == 0:
			impact.Action = "removed"
		case len(piecesOf(cue.StartMS, cue.EndMS, spansBySource[cue.SourceID])) > 1:
			impact.Action = "split"
		default:
			impact.Action = "clipped"
		}
		impacts = append(impacts, impact)
	}
	return impacts
}

func piecesOf(start, end int64, spans []domain.Span) [][2]int64 {
	out := [][2]int64{}
	for _, span := range spans {
		s, e := max(start, span.InMS), min(end, span.OutMS)
		if e > s {
			out = append(out, [2]int64{s, e})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	merged := [][2]int64{}
	for _, piece := range out {
		if len(merged) > 0 && piece[0] <= merged[len(merged)-1][1] {
			if piece[1] > merged[len(merged)-1][1] {
				merged[len(merged)-1][1] = piece[1]
			}
			continue
		}
		merged = append(merged, piece)
	}
	return merged
}

func unionOverlap(start, end int64, spans []domain.Span) int64 {
	var total int64
	for _, piece := range piecesOf(start, end, spans) {
		total += piece[1] - piece[0]
	}
	return total
}

// choices reports every built-in option so a reviewer can compare outcomes
// that were not selected. Only existing measurements are reused.
func choices(req Request, views []candidateView, segments []segment) []Choice {
	out := []Choice{}
	for _, option := range []string{domain.OptionKeepAll, domain.OptionSilenceOnly, domain.OptionTightenPauses, domain.OptionSilenceAndSoftTail} {
		selected, err := selectCuts(req, option, views, segments)
		if err != nil {
			continue
		}
		choice := Choice{
			Option: option, Reached: selected.feasible && selected.resultMS <= req.TargetMS, Feasible: selected.feasible,
			ResultMS: selected.resultMS, ShortfallMS: max(0, selected.resultMS-req.TargetMS),
			SilenceCutMS: selected.silenceCut, SoftSpeechCutMS: selected.softCut,
		}
		switch option {
		case domain.OptionKeepAll:
			choice.Note = "no source bytes removed"
		case domain.OptionSilenceOnly:
			choice.Note = fmt.Sprintf("keeps at least %dms of every pause", req.NaturalPauseMS)
		case domain.OptionTightenPauses:
			choice.Note = fmt.Sprintf("tightens pauses to %dms but never touches low-level speech", req.MinPauseMS)
		case domain.OptionSilenceAndSoftTail:
			choice.Note = "also removes low-level speech such as breaths or soft word tails; review each flagged interval"
		}
		out = append(out, choice)
	}
	return out
}

package transport

import (
	"cadenceforge/internal/domain"
	"cadenceforge/internal/service"
	"net/http"
	"strconv"
)

// analyseSilence requests a new analysis revision and returns at once; the
// revision is pending until the analysis worker finishes. Editing is not
// blocked while the scan runs.
func (a API) analyseSilence(w http.ResponseWriter, r *http.Request) {
	v, err := a.Service.RequestAnalysis(r.PathValue("id"))
	if err != nil {
		failure(w, err)
		return
	}
	w.Header().Set("Location", "/forge/v1/sources/"+v.SourceID+"/silence/"+strconv.Itoa(v.Revision))
	write(w, 202, v)
}

func (a API) silenceHistory(w http.ResponseWriter, r *http.Request) {
	v, err := a.Service.Analyses(r.PathValue("id"))
	if err != nil {
		failure(w, err)
		return
	}
	if v == nil {
		v = []domain.SilenceAnalysis{}
	}
	write(w, 200, v)
}

func (a API) silenceRevision(w http.ResponseWriter, r *http.Request) {
	revision, err := strconv.Atoi(r.PathValue("revision"))
	if err != nil || revision < 1 {
		failure(w, domain.Invalid("silence analysis revision must be a positive integer"))
		return
	}
	v, err := a.Service.Analysis(r.PathValue("id"), revision)
	if err != nil {
		failure(w, err)
		return
	}
	write(w, 200, v)
}

func (a API) proposeSilence(w http.ResponseWriter, r *http.Request) {
	var input service.SilenceOptions
	if err := decode(w, r, &input); err != nil {
		failure(w, err)
		return
	}
	// A proposal does not save a revision; the base revision is explicit in the
	// query so a stale review cannot be mistaken for current work.
	revision, err := strconv.Atoi(r.URL.Query().Get("revision"))
	if err != nil || revision < 1 {
		failure(w, domain.Invalid("silence proposals require ?revision with a positive sheet revision"))
		return
	}
	v, err := a.Service.ProposeSilence(r.PathValue("id"), revision, input)
	if err != nil {
		failure(w, err)
		return
	}
	w.Header().Set("ETag", strconv.Itoa(revision))
	write(w, 200, v)
}

// adoptSilence commits a reviewed proposal as a new sheet revision. If-Match is
// mandatory: adoption is a compare-and-swap against confirmed editing work.
func (a API) adoptSilence(w http.ResponseWriter, r *http.Request) {
	revision, err := requiredRevision(r)
	if err != nil {
		failure(w, err)
		return
	}
	var input service.SilenceOptions
	if err := decode(w, r, &input); err != nil {
		failure(w, err)
		return
	}
	sheet, adoption, err := a.Service.AdoptSilence(r.PathValue("id"), revision, input)
	if err != nil {
		failure(w, err)
		return
	}
	w.Header().Set("ETag", strconv.Itoa(sheet.Revision))
	write(w, 201, map[string]any{"sheet": sheet, "adoption": adoption})
}

func (a API) adoption(w http.ResponseWriter, r *http.Request) {
	v, err := a.Service.Adoption(r.PathValue("id"))
	if err != nil {
		failure(w, err)
		return
	}
	write(w, 200, v)
}

package idempotency

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// DefaultProblemTypeBase is the default prefix of the "type" URI of the
// problem details this package writes. Each problem type is documented under
// a fragment of docs/problems.md, as the draft (Section 2.7) recommends
// pointing clients at documentation.
const DefaultProblemTypeBase = "https://github.com/aozmekik/go-idempotency/blob/main/docs/problems.md#"

// Problem type slugs. The full "type" URI is Config.ProblemTypeBase followed
// by the slug.
const (
	ProblemKeyMissing            = "key-missing"
	ProblemKeyInvalid            = "key-invalid"
	ProblemKeyReused             = "key-reused"
	ProblemRequestInFlight       = "request-in-flight"
	ProblemBodyTooLarge          = "body-too-large"
	ProblemBodyUnreadable        = "body-unreadable"
	ProblemStoreUnavailable      = "store-unavailable"
	ProblemResponseNotReplayable = "response-not-replayable"
)

// Problem is an RFC 9457 problem details object.
type Problem struct {
	// Type is a URI reference that identifies the problem type.
	Type string `json:"type"`
	// Title is a short, human-readable summary of the problem type.
	Title string `json:"title"`
	// Status is the HTTP status code.
	Status int `json:"status"`
	// Detail is a human-readable explanation of this occurrence.
	Detail string `json:"detail,omitempty"`
	// Slug is the problem type slug (one of the Problem* constants). It is
	// not serialized; it lets a custom ProblemWriter switch on the problem.
	Slug string `json:"-"`
}

// WriteProblem writes p as an application/problem+json response. It is the
// default Config.ProblemWriter.
func WriteProblem(w http.ResponseWriter, _ *http.Request, p *Problem) {
	body, err := json.Marshal(p)
	if err != nil { // unreachable: Problem holds only strings and an int
		http.Error(w, http.StatusText(p.Status), p.Status)
		return
	}
	body = append(body, '\n')
	h := w.Header()
	h.Set("Content-Type", "application/problem+json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(p.Status)
	_, _ = w.Write(body) // nothing useful to do if the client went away
}

// problemTitles maps each slug to its status code and title.
var problemTitles = map[string]struct {
	status int
	title  string
}{
	ProblemKeyMissing:            {http.StatusBadRequest, "Idempotency-Key header is required"},
	ProblemKeyInvalid:            {http.StatusBadRequest, "Idempotency-Key header is malformed"},
	ProblemKeyReused:             {http.StatusUnprocessableEntity, "Idempotency-Key reused with a different request"},
	ProblemRequestInFlight:       {http.StatusConflict, "A request with this Idempotency-Key is being processed"},
	ProblemBodyTooLarge:          {http.StatusRequestEntityTooLarge, "Request body too large for idempotent processing"},
	ProblemBodyUnreadable:        {http.StatusBadRequest, "Request body could not be read"},
	ProblemStoreUnavailable:      {http.StatusServiceUnavailable, "Idempotency store unavailable"},
	ProblemResponseNotReplayable: {http.StatusInternalServerError, "Original response cannot be replayed"},
}

func (m *Middleware) problem(w http.ResponseWriter, r *http.Request, slug, detail string) {
	t := problemTitles[slug]
	m.cfg.ProblemWriter(w, r, &Problem{
		Type:   m.cfg.ProblemTypeBase + slug,
		Title:  t.title,
		Status: t.status,
		Detail: detail,
		Slug:   slug,
	})
}

package badges

import (
	"context"
	"crypto/sha256"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	badgetoken "github.com/gocdnext/gocdnext/server/internal/badges"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

type badgeStore interface {
	GetBadgeLatestRun(ctx context.Context, slug, tokenHash, pipelineName, branch string) (store.BadgeRun, bool, error)
}

type Handler struct {
	store      badgeStore
	log        *slog.Logger
	publicBase string
}

func NewHandler(s badgeStore, log *slog.Logger, publicBase string) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{
		store:      s,
		log:        log,
		publicBase: strings.TrimRight(publicBase, "/"),
	}
}

func (h *Handler) Mount(r chi.Router) {
	r.Get("/api/v1/badge/{slug}.svg", h.Project)
	r.Get("/api/v1/badge/{slug}/{pipeline}.svg", h.Pipeline)
}

func (h *Handler) Project(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, chi.URLParam(r, "slug"), "")
}

func (h *Handler) Pipeline(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, chi.URLParam(r, "slug"), chi.URLParam(r, "pipeline"))
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, slug, pipelineName string) {
	status := badgeUnknown
	href := ""
	lookupErr := false

	token := r.URL.Query().Get("token")
	branch := r.URL.Query().Get("branch")
	if h.store != nil &&
		validSegment(slug, 120) &&
		(pipelineName == "" || validSegment(pipelineName, 160)) &&
		validBranch(branch) &&
		badgetoken.LooksLikeToken(token) {
		run, ok, err := h.store.GetBadgeLatestRun(r.Context(),
			slug,
			badgetoken.HashToken(token),
			pipelineName,
			branch,
		)
		if err != nil {
			lookupErr = true
			h.log.Warn("badge lookup failed", "slug", slug, "pipeline", pipelineName, "err", err)
		} else if ok {
			status = statusFromRun(run.Status)
			if h.publicBase != "" {
				href = h.publicBase + "/runs/" + run.ID.String()
			}
		}
	}

	body := renderSVG(status, href)
	writeSVG(w, r, body, lookupErr)
}

func validSegment(s string, maxBytes int) bool {
	if s == "" || len(s) > maxBytes || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == '/' {
			return false
		}
	}
	return true
}

func validBranch(s string) bool {
	if len(s) > 255 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

type badgeStatus string

const (
	badgePassing badgeStatus = "passing"
	badgeFailing badgeStatus = "failing"
	badgeRunning badgeStatus = "running"
	badgeUnknown badgeStatus = "unknown"
)

func statusFromRun(status string) badgeStatus {
	switch domain.RunStatus(status) {
	case domain.StatusSuccess:
		return badgePassing
	case domain.StatusQueued, domain.StatusRunning, domain.StatusWaiting:
		return badgeRunning
	case domain.StatusFailed, domain.StatusCanceled:
		return badgeFailing
	default:
		return badgeUnknown
	}
}

func writeSVG(w http.ResponseWriter, r *http.Request, body []byte, lookupErr bool) {
	sum := sha256.Sum256(body)
	etag := fmt.Sprintf(`W/"%x"`, sum[:8])

	h := w.Header()
	h.Set("Content-Type", "image/svg+xml; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("ETag", etag)
	if lookupErr {
		h.Set("Cache-Control", "no-store")
	} else {
		h.Set("Cache-Control", "public, max-age=60")
	}
	if !lookupErr && r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func renderSVG(status badgeStatus, href string) []byte {
	label := "build"
	message := string(status)
	color := badgeColor(status)
	leftWidth := textWidth(label)
	rightWidth := textWidth(message)
	width := leftWidth + rightWidth

	content := fmt.Sprintf(`<g shape-rendering="crispEdges"><rect width="%d" height="20" fill="#555"/><rect x="%d" width="%d" height="20" fill="%s"/></g><g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="11"><text x="%d" y="14">%s</text><text x="%d" y="14">%s</text></g>`,
		width,
		leftWidth,
		rightWidth,
		color,
		leftWidth/2,
		html.EscapeString(label),
		leftWidth+rightWidth/2,
		html.EscapeString(message),
	)
	if href != "" {
		content = `<a href="` + html.EscapeString(href) + `">` + content + `</a>`
	}
	return []byte(fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="20" role="img" aria-label="%s: %s">%s</svg>`,
		width,
		html.EscapeString(label),
		html.EscapeString(message),
		content,
	))
}

func badgeColor(status badgeStatus) string {
	switch status {
	case badgePassing:
		return "#4c1"
	case badgeFailing:
		return "#e05d44"
	case badgeRunning:
		return "#007ec6"
	default:
		return "#9f9f9f"
	}
}

func textWidth(s string) int {
	return 10 + utf8.RuneCountInString(s)*7
}

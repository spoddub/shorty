package httpapi

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
	sentrygin "github.com/getsentry/sentry-go/gin"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"

	db "shorty/internal/db/sqlc"
)

type Handler struct {
	Q       *db.Queries
	BaseURL string
}

type linkIn struct {
	OriginalURL string `json:"original_url"`
	ShortName   string `json:"short_name"`
}

type linkOut struct {
	ID          int64  `json:"id"`
	OriginalURL string `json:"original_url"`
	ShortName   string `json:"short_name"`
	ShortURL    string `json:"short_url"`
}

var shortNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{3,32}$`)

func NewRouter(q *db.Queries, baseURL string) *gin.Engine {
	h := &Handler{
		Q:       q,
		BaseURL: strings.TrimRight(baseURL, "/"),
	}

	r := gin.New()
	r.Use(gin.Logger())

	r.Use(sentrygin.New(sentrygin.Options{
		Repanic:         true,
		WaitForDelivery: false,
		Timeout:         2 * time.Second,
	}))

	r.Use(gin.Recovery())

	r.GET("/ping", func(c *gin.Context) { c.String(http.StatusOK, "pong") })

	r.GET("/r/:short_name", h.redirectByShortName)

	api := r.Group("/api")
	{
		api.GET("/links", h.listLinks)
		api.POST("/links", h.createLink)
		api.GET("/links/:id", h.getLink)
		api.PUT("/links/:id", h.updateLink)
		api.DELETE("/links/:id", h.deleteLink)
	}

	r.GET("/debug/sentry", func(c *gin.Context) {
		err := errors.New("test error from /debug/sentry")
		if hub := sentrygin.GetHubFromContext(c); hub != nil {
			hub.CaptureException(err)
		} else {
			sentry.CaptureException(err)
		}
		c.String(http.StatusInternalServerError, "sent to sentry")
	})

	return r
}

func (h *Handler) shortURL(shortName string) string {
	return h.BaseURL + "/r/" + shortName
}

func (h *Handler) listLinks(c *gin.Context) {
	rows, err := h.Q.ListLinks(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	out := make([]linkOut, 0, len(rows))
	for _, r := range rows {
		out = append(out, linkOut{
			ID:          r.ID,
			OriginalURL: r.OriginalUrl,
			ShortName:   r.ShortName,
			ShortURL:    h.shortURL(r.ShortName),
		})
	}

	c.JSON(http.StatusOK, out)
}

func (h *Handler) createLink(c *gin.Context) {
	var in linkIn
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON"})
		return
	}

	if err := validateOriginalURL(in.OriginalURL); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}

	ctx := c.Request.Context()

	shortName := strings.TrimSpace(in.ShortName)
	if shortName != "" {
		if !shortNameRe.MatchString(shortName) {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "short_name is invalid"})
			return
		}

		row, err := h.Q.CreateLink(ctx, db.CreateLinkParams{
			OriginalUrl: in.OriginalURL,
			ShortName:   shortName,
		})
		if err != nil {
			if isUniqueViolation(err) {
				c.JSON(http.StatusConflict, gin.H{"error": "short_name already exists"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
			return
		}

		c.JSON(http.StatusCreated, linkOut{
			ID:          row.ID,
			OriginalURL: row.OriginalUrl,
			ShortName:   row.ShortName,
			ShortURL:    h.shortURL(row.ShortName),
		})
		return
	}

	for i := 0; i < 10; i++ {
		gen := randomBase62(7)
		row, err := h.Q.CreateLink(ctx, db.CreateLinkParams{
			OriginalUrl: in.OriginalURL,
			ShortName:   gen,
		})
		if err != nil {
			if isUniqueViolation(err) {
				continue
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
			return
		}

		c.JSON(http.StatusCreated, linkOut{
			ID:          row.ID,
			OriginalURL: row.OriginalUrl,
			ShortName:   row.ShortName,
			ShortURL:    h.shortURL(row.ShortName),
		})
		return
	}

	c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate unique short_name"})
}

func (h *Handler) getLink(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}

	row, err := h.Q.GetLink(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	c.JSON(http.StatusOK, linkOut{
		ID:          row.ID,
		OriginalURL: row.OriginalUrl,
		ShortName:   row.ShortName,
		ShortURL:    h.shortURL(row.ShortName),
	})
}

func (h *Handler) updateLink(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}

	var in linkIn
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON"})
		return
	}

	if err := validateOriginalURL(in.OriginalURL); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}

	shortName := strings.TrimSpace(in.ShortName)
	if shortName == "" || !shortNameRe.MatchString(shortName) {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "short_name is invalid"})
		return
	}

	row, err := h.Q.UpdateLink(c.Request.Context(), db.UpdateLinkParams{
		ID:          id,
		OriginalUrl: in.OriginalURL,
		ShortName:   shortName,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		if isUniqueViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "short_name already exists"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	c.JSON(http.StatusOK, linkOut{
		ID:          row.ID,
		OriginalURL: row.OriginalUrl,
		ShortName:   row.ShortName,
		ShortURL:    h.shortURL(row.ShortName),
	})
}

func (h *Handler) deleteLink(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}

	n, err := h.Q.DeleteLink(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}

	c.Status(http.StatusNoContent)
}

func (h *Handler) redirectByShortName(c *gin.Context) {
	shortName := strings.TrimSpace(c.Param("short_name"))
	if shortName == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}

	row, err := h.Q.GetLinkByShortName(c.Request.Context(), shortName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	c.Redirect(http.StatusFound, row.OriginalUrl)
}

func validateOriginalURL(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("original_url is required")
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return errors.New("original_url is invalid")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("original_url must start with http or https")
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func randomBase62(n int) string {
	b := make([]byte, n)
	for i := range b {
		num, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b[i] = alphabet[num.Int64()]
	}
	return string(b)
}

func parseID(c *gin.Context) (int64, bool) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return 0, false
	}
	return id, true
}

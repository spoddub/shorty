package httpapi

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	sentrygin "github.com/getsentry/sentry-go/gin"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"

	db "shorty/internal/db/sqlc"
)

type Handler struct {
	Q       *db.Queries
	BaseURL string
}

type linkIn struct {
	OriginalURL string `json:"original_url" binding:"required,url"`
	ShortName   string `json:"short_name" binding:"omitempty,shortname"`
}

type linkOut struct {
	ID          int64  `json:"id"`
	OriginalURL string `json:"original_url"`
	ShortName   string `json:"short_name"`
	ShortURL    string `json:"short_url"`
}

type linkVisitOut struct {
	ID        int64     `json:"id"`
	LinkID    int64     `json:"link_id"`
	CreatedAt time.Time `json:"created_at"`
	IP        string    `json:"ip"`
	UserAgent string    `json:"user_agent"`
	Status    int32     `json:"status"`
}

var shortNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{3,32}$`)

func NewRouter(q *db.Queries, baseURL string) *gin.Engine {
	setupValidator()

	h := &Handler{
		Q:       q,
		BaseURL: strings.TrimRight(baseURL, "/"),
	}

	r := gin.New()

	r.TrustedPlatform = gin.PlatformCloudflare

	_ = r.SetTrustedProxies([]string{"127.0.0.1", "::1"})

	allowedOrigins := []string{"http://localhost:5173"}
	if u, err := url.Parse(h.BaseURL); err == nil && u.Scheme != "" && u.Host != "" {
		allowedOrigins = append(allowedOrigins, u.Scheme+"://"+u.Host)
	}

	r.Use(cors.New(cors.Config{
		AllowOrigins: allowedOrigins,
		AllowMethods: []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders: []string{"Content-Type", "Authorization", "Range"},
		ExposeHeaders: []string{
			"Content-Range",
		},
		MaxAge: 12 * time.Hour,
	}))

	r.Use(gin.Logger())

	r.Use(sentrygin.New(sentrygin.Options{
		Repanic: true,
	}))

	r.Use(gin.Recovery())

	r.GET("/ping", func(c *gin.Context) {
		c.String(http.StatusOK, "pong")
	})

	r.GET("/r/:code", h.redirectByCode)

	api := r.Group("/api")
	{
		api.GET("/links", h.listLinks)
		api.POST("/links", h.createLink)
		api.GET("/links/:id", h.getLink)
		api.PUT("/links/:id", h.updateLink)
		api.DELETE("/links/:id", h.deleteLink)

		api.GET("/link_visits", h.listLinkVisits)
	}

	return r
}

func (h *Handler) shortURL(shortName string) string {
	return h.BaseURL + "/r/" + shortName
}

func (h *Handler) listLinks(c *gin.Context) {
	ctx := c.Request.Context()

	total, err := h.Q.CountLinks(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	rawRange := c.Query("range")

	if strings.TrimSpace(rawRange) == "" {
		rows, err := h.Q.ListLinks(ctx)
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

		setContentRange(c, "links", 0, len(out), total)
		c.JSON(http.StatusOK, out)
		return
	}

	from, to, ok := parseRange(rawRange)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid range"})
		return
	}

	inclusive := c.Query("sort") != "" || c.Query("filter") != ""

	limit := to - from
	if inclusive {
		limit = to - from + 1
	}

	if limit < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid range"})
		return
	}

	if total == 0 || limit == 0 || int64(from) >= total {
		c.Header("Content-Range", fmt.Sprintf("links */%d", total))
		c.JSON(http.StatusOK, []linkOut{})
		return
	}

	rows, err := h.Q.ListLinksRange(ctx, db.ListLinksRangeParams{
		Limit:  int32(limit),
		Offset: int32(from),
	})
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

	setContentRange(c, "links", from, len(out), total)
	c.JSON(http.StatusOK, out)
}

func (h *Handler) createLink(c *gin.Context) {
	var in linkIn
	if err := c.ShouldBindJSON(&in); err != nil {
		writeBindError(c, err)
		return
	}

	ctx := c.Request.Context()

	shortName := strings.TrimSpace(in.ShortName)
	if shortName != "" {
		row, err := h.Q.CreateLink(ctx, db.CreateLinkParams{
			OriginalUrl: in.OriginalURL,
			ShortName:   shortName,
		})
		if err != nil {
			if isUniqueViolation(err) {
				writeUniqueShortNameError(c)
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
		writeBindError(c, err)
		return
	}

	ctx := c.Request.Context()

	shortName := strings.TrimSpace(in.ShortName)
	if shortName == "" {
		existing, err := h.Q.GetLink(ctx, id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
			return
		}
		shortName = existing.ShortName
	}

	row, err := h.Q.UpdateLink(ctx, db.UpdateLinkParams{
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
			writeUniqueShortNameError(c)
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

func (h *Handler) redirectByCode(c *gin.Context) {
	code := strings.TrimSpace(c.Param("code"))
	if code == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}

	row, err := h.Q.GetLinkByShortName(c.Request.Context(), code)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	status := http.StatusFound

	ip := c.ClientIP()
	ua := c.GetHeader("User-Agent")
	ref := c.GetHeader("Referer")

	_, _ = h.Q.CreateLinkVisit(c.Request.Context(), db.CreateLinkVisitParams{
		LinkID:    row.ID,
		Ip:        ip,
		UserAgent: ua,
		Referer:   ref,
		Status:    int32(status),
	})

	c.Redirect(status, row.OriginalUrl)
}

func (h *Handler) listLinkVisits(c *gin.Context) {
	ctx := c.Request.Context()

	total, err := h.Q.CountLinkVisits(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	rawRange := strings.TrimSpace(c.GetHeader("Range"))
	if rawRange == "" {
		rawRange = strings.TrimSpace(c.Query("range"))
	}

	from, to := 0, 10
	if rawRange != "" {
		var ok bool
		from, to, ok = parseRange(rawRange)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid range"})
			return
		}
	}

	inclusive := c.Query("sort") != "" || c.Query("filter") != ""

	limit := to - from
	if inclusive {
		limit = to - from + 1
	}
	if limit < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid range"})
		return
	}

	if total == 0 || limit == 0 || int64(from) >= total {
		c.Header("Content-Range", fmt.Sprintf("link_visits */%d", total))
		c.JSON(http.StatusOK, []linkVisitOut{})
		return
	}

	rows, err := h.Q.ListLinkVisitsRange(ctx, db.ListLinkVisitsRangeParams{
		Limit:  int32(limit),
		Offset: int32(from),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	out := make([]linkVisitOut, 0, len(rows))
	for _, v := range rows {
		out = append(out, linkVisitOut{
			ID:        v.ID,
			LinkID:    v.LinkID,
			CreatedAt: v.CreatedAt.Time.UTC(),
			IP:        v.Ip,
			UserAgent: v.UserAgent,
			Status:    v.Status,
		})
	}

	setContentRange(c, "link_visits", from, len(out), total)
	c.JSON(http.StatusOK, out)
}

func setContentRange(c *gin.Context, resource string, from int, count int, total int64) {
	if count <= 0 {
		c.Header("Content-Range", fmt.Sprintf("%s */%d", resource, total))
		return
	}
	end := from + count - 1
	c.Header("Content-Range", fmt.Sprintf("%s %d-%d/%d", resource, from, end, total))
}

func parseRange(raw string) (start, end int, ok bool) {
	raw = strings.TrimSpace(raw)

	var arr []int
	if err := json.Unmarshal([]byte(raw), &arr); err != nil || len(arr) != 2 {
		return 0, 0, false
	}

	if arr[0] < 0 || arr[1] < 0 || arr[1] < arr[0] {
		return 0, 0, false
	}

	return arr[0], arr[1], true
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

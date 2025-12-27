package httpapi

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"
	"github.com/pressly/goose/v3"

	db "shorty/internal/db/sqlc"
)

var (
	envOnce     sync.Once
	migrateOnce sync.Once

	projectRoot string
	rootErr     error
)

func loadEnvAndRoot(t *testing.T) string {
	t.Helper()

	envOnce.Do(func() {
		wd, err := os.Getwd()
		if err != nil {
			rootErr = err
			return
		}

		dir := wd
		for i := 0; i < 10; i++ {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				projectRoot = dir
				break
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}

		if projectRoot == "" {
			rootErr = fmt.Errorf("project root not found (go.mod). wd=%s", wd)
			return
		}

		_ = godotenv.Load(
			filepath.Join(projectRoot, ".env"),
			filepath.Join(projectRoot, ".env.local"),
			filepath.Join(projectRoot, ".env.test"),
		)
	})

	if rootErr != nil {
		t.Fatal(rootErr)
	}

	return projectRoot
}

func mustDSN(t *testing.T) string {
	t.Helper()

	root := loadEnvAndRoot(t)
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Fatalf("DATABASE_URL is required for tests. Set it as env var or put it into %s", filepath.Join(root, ".env"))
	}
	return dsn
}

func openSQL(t *testing.T) *sql.DB {
	t.Helper()

	dsn := mustDSN(t)

	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}

	root := loadEnvAndRoot(t)
	migrationsDir := filepath.Join(root, "db", "migrations")

	migrateOnce.Do(func() {
		_ = goose.SetDialect("postgres")
		if err := goose.Up(sqlDB, migrationsDir); err != nil {
			_ = sqlDB.Close()
			t.Fatalf("goose up failed: %v", err)
		}
	})

	return sqlDB
}

func openPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := mustDSN(t)

	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func truncateAll(t *testing.T, sqlDB *sql.DB) {
	t.Helper()

	_, err := sqlDB.Exec(`TRUNCATE link_visits, links RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatal(err)
	}
}

func seedLink(t *testing.T, sqlDB *sql.DB, originalURL, shortName string) int64 {
	t.Helper()

	var id int64
	err := sqlDB.QueryRow(
		`INSERT INTO links (original_url, short_name) VALUES ($1, $2) RETURNING id`,
		originalURL, shortName,
	).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func newRouter(t *testing.T, pool *pgxpool.Pool) http.Handler {
	t.Helper()

	q := db.New(pool)
	return NewRouter(q, "https://short.io")
}

func TestRedirectCreatesVisit(t *testing.T) {
	sqlDB := openSQL(t)
	defer func() {
		if err := sqlDB.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	truncateAll(t, sqlDB)
	_ = seedLink(t, sqlDB, "https://example.com/long-url", "exmpl")

	pool := openPool(t)
	r := newRouter(t, pool)

	req := httptest.NewRequest(http.MethodGet, "/r/exmpl", nil)
	req.RemoteAddr = "172.18.0.1:12345"
	req.Header.Set("User-Agent", "curl/8.5.0")
	req.Header.Set("Referer", "https://ref.example/")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "https://example.com/long-url" {
		t.Fatalf("expected Location %q, got %q", "https://example.com/long-url", loc)
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/link_visits", nil)
	req.Header.Set("Range", "[0,10]")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Range"); got != "link_visits 0-0/1" {
		t.Fatalf("expected Content-Range %q, got %q", "link_visits 0-0/1", got)
	}

	var items []struct {
		ID        int64  `json:"id"`
		LinkID    int64  `json:"link_id"`
		IP        string `json:"ip"`
		UserAgent string `json:"user_agent"`
		Status    int32  `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].IP != "172.18.0.1" {
		t.Fatalf("expected ip %q, got %q", "172.18.0.1", items[0].IP)
	}
	if items[0].UserAgent != "curl/8.5.0" {
		t.Fatalf("expected user_agent %q, got %q", "curl/8.5.0", items[0].UserAgent)
	}
	if items[0].Status != 302 {
		t.Fatalf("expected status 302, got %d", items[0].Status)
	}
}

func TestLinkVisitsPagination(t *testing.T) {
	sqlDB := openSQL(t)
	defer func() {
		if err := sqlDB.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	truncateAll(t, sqlDB)
	linkID := seedLink(t, sqlDB, "https://example.com", "seed")

	for i := 0; i < 12; i++ {
		_, err := sqlDB.Exec(
			`INSERT INTO link_visits (link_id, ip, user_agent, referer, status)
			 VALUES ($1, $2, $3, $4, $5)`,
			linkID,
			"10.0.0.1",
			"ua",
			"",
			302,
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	pool := openPool(t)
	r := newRouter(t, pool)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/link_visits", nil)
	req.Header.Set("Range", "[0,10]")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Range"); got != "link_visits 0-9/12" {
		t.Fatalf("expected Content-Range %q, got %q", "link_visits 0-9/12", got)
	}

	var page []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page) != 10 {
		t.Fatalf("expected 10 items, got %d", len(page))
	}
}

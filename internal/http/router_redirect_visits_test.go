package httpapi

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"
	"github.com/pressly/goose/v3"

	db "shorty/internal/db/sqlc"
)

var (
	baseDSN       string
	schemaDSN     string
	schemaName    string
	migrationsDir string
)

func TestMain(m *testing.M) {
	root, err := findProjectRoot()
	if err != nil {
		fmt.Println("test setup failed:", err)
		os.Exit(1)
	}

	_ = godotenv.Load(
		filepath.Join(root, ".env"),
		filepath.Join(root, ".env.local"),
		filepath.Join(root, ".env.test"),
	)

	baseDSN = os.Getenv("DATABASE_URL")
	if strings.TrimSpace(baseDSN) == "" {
		fmt.Println("DATABASE_URL is required for tests (env var or .env/.env.local/.env.test)")
		os.Exit(1)
	}

	migrationsDir = filepath.Join(root, "db", "migrations")

	schemaName = fmt.Sprintf("test_httpapi_%d_%d", os.Getpid(), time.Now().UnixNano())
	if err := createSchema(baseDSN, schemaName); err != nil {
		fmt.Println("create schema failed:", err)
		os.Exit(1)
	}

	var dsnErr error
	schemaDSN, dsnErr = dsnWithSearchPath(baseDSN, schemaName)
	if dsnErr != nil {
		fmt.Println("build schema DSN failed:", dsnErr)
		_ = dropSchema(baseDSN, schemaName)
		os.Exit(1)
	}

	if err := runMigrations(schemaDSN, migrationsDir); err != nil {
		fmt.Println("goose up failed:", err)
		_ = dropSchema(baseDSN, schemaName)
		os.Exit(1)
	}

	code := m.Run()

	_ = dropSchema(baseDSN, schemaName)

	os.Exit(code)
}

func findProjectRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}

	dir := wd
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return "", fmt.Errorf("project root not found (go.mod). wd=%s", wd)
}

func dsnWithSearchPath(dsn, schema string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}

	q := u.Query()
	q.Set("options", "-csearch_path="+schema)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func createSchema(dsn, schema string) error {
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = sqlDB.Close() }()

	_, err = sqlDB.Exec(`CREATE SCHEMA IF NOT EXISTS ` + quoteIdent(schema))
	return err
}

func dropSchema(dsn, schema string) error {
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = sqlDB.Close() }()

	_, err = sqlDB.Exec(`DROP SCHEMA IF EXISTS ` + quoteIdent(schema) + ` CASCADE`)
	return err
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func runMigrations(dsn, dir string) error {
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = sqlDB.Close() }()

	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.Up(sqlDB, dir)
}

func openSQL(t *testing.T) *sql.DB {
	t.Helper()

	sqlDB, err := sql.Open("pgx", schemaDSN)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = sqlDB.Close()
	})

	return sqlDB
}

func openPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	pool, err := pgxpool.New(t.Context(), schemaDSN)
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

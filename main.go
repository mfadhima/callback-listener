package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	mysql "github.com/go-sql-driver/mysql"
	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"
)

const defaultResponseBody = `{"success": true, "message": "received"}`

type Config struct {
	Addr          string `yaml:"addr"`
	DSN           string `yaml:"db_dsn"`
	DefaultStatus int    `yaml:"default_status"`
	DefaultBody   string `yaml:"default_body"`
}

type Rule struct {
	ID              int64
	Method          string
	Path            string
	StatusCode      int
	ResponseBody    string
	ResponseHeaders sql.NullString
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type RequestLog struct {
	ID          int64
	Method      string
	Path        string
	RuleID      sql.NullInt64
	QueryJSON   sql.NullString
	HeadersJSON sql.NullString
	BodyText    sql.NullString
	CreatedAt   time.Time
}

type RequestSummary struct {
	ID        int64  `json:"id"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	RuleID    *int64 `json:"rule_id"`
	CreatedAt string `json:"created_at"`
}

type RuleSummary struct {
	ID         int64  `json:"id"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	StatusCode int    `json:"status_code"`
}

type RequestDetail struct {
	ID          int64        `json:"id"`
	Method      string       `json:"method"`
	Path        string       `json:"path"`
	RuleID      *int64       `json:"rule_id"`
	QueryJSON   string       `json:"query_json"`
	HeadersJSON string       `json:"headers_json"`
	BodyText    string       `json:"body_text"`
	CreatedAt   string       `json:"created_at"`
	Rule        *RuleSummary `json:"rule"`
}

type IndexView struct {
	Requests  []RequestLog
	Paths     []string
	Active    string
	StartTime string
	EndTime   string
}

type RequestView struct {
	Request RequestLog
	Rule    *Rule
}

type RulesView struct {
	Rules         []Rule
	DefaultStatus int
	DefaultBody   string
}

type RuleEditView struct {
	Rule          Rule
	DefaultStatus int
	DefaultBody   string
}

type SettingsView struct {
	Paths []string
}

type wsClient struct {
	conn *websocket.Conn
	send chan RequestSummary
	hub  *requestHub
}

type requestHub struct {
	mu      sync.Mutex
	clients map[*wsClient]struct{}
}

func newRequestHub() *requestHub {
	return &requestHub{
		clients: make(map[*wsClient]struct{}),
	}
}

func (h *requestHub) add(conn *websocket.Conn) {
	client := &wsClient{
		conn: conn,
		send: make(chan RequestSummary, 16),
		hub:  h,
	}

	h.mu.Lock()
	h.clients[client] = struct{}{}
	h.mu.Unlock()

	go client.writePump()
	go client.readPump()
}

func (h *requestHub) remove(client *wsClient) {
	h.mu.Lock()
	if _, ok := h.clients[client]; !ok {
		h.mu.Unlock()
		return
	}
	delete(h.clients, client)
	close(client.send)
	h.mu.Unlock()
	_ = client.conn.Close()
}

func (h *requestHub) broadcast(summary RequestSummary) {
	h.mu.Lock()
	for client := range h.clients {
		select {
		case client.send <- summary:
		default:
			// Drop overloaded clients to keep the broadcast loop moving.
			go h.remove(client)
		}
	}
	h.mu.Unlock()
}

func (c *wsClient) writePump() {
	for msg := range c.send {
		if err := c.conn.WriteJSON(msg); err != nil {
			break
		}
	}
	c.hub.remove(c)
}

func (c *wsClient) readPump() {
	for {
		if _, _, err := c.conn.NextReader(); err != nil {
			break
		}
	}
	c.hub.remove(c)
}

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func main() {
	cfg := loadConfig()

	db, err := openDB(cfg.DSN)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	defer db.Close()

	if err := ensureSchema(db); err != nil {
		log.Fatalf("db schema: %v", err)
	}

	hub := newRequestHub()

	router := gin.Default()
	router.SetFuncMap(template.FuncMap{
		"prettyJSON": prettyJSON,
		"formatTime": formatTime,
	})
	router.LoadHTMLGlob("templates/*.html")
	router.Static("/static", "./static")

	router.GET("/", func(c *gin.Context) {
		filter := strings.TrimSpace(c.Query("path"))
		if filter != "" {
			filter = normalizePath(filter)
		}
		start := c.Query("start")
		end := c.Query("end")

		requests, err := listRequests(db, 50, filter, start, end)
		if err != nil {
			c.String(http.StatusInternalServerError, "failed to load requests")
			return
		}
		paths, err := listRequestPaths(db)
		if err != nil {
			c.String(http.StatusInternalServerError, "failed to load paths")
			return
		}
		c.HTML(http.StatusOK, "index.html", IndexView{
			Requests:  requests,
			Paths:     paths,
			Active:    filter,
			StartTime: start,
			EndTime:   end,
		})
	})

	router.GET("/api/requests", func(c *gin.Context) {
		filter := strings.TrimSpace(c.Query("path"))
		if filter != "" {
			filter = normalizePath(filter)
		}
		start := c.Query("start")
		end := c.Query("end")

		requests, err := listRequests(db, 50, filter, start, end)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load requests"})
			return
		}
		c.JSON(http.StatusOK, summarizeRequests(requests))
	})

	router.GET("/api/requests/:id", func(c *gin.Context) {
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request id"})
			return
		}
		requestLog, err := getRequest(db, id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				c.JSON(http.StatusNotFound, gin.H{"error": "request not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load request"})
			return
		}
		detail := buildRequestDetail(requestLog)
		if requestLog.RuleID.Valid {
			rule, err := getRule(db, requestLog.RuleID.Int64)
			if err == nil {
				detail.Rule = &RuleSummary{
					ID:         rule.ID,
					Method:     rule.Method,
					Path:       rule.Path,
					StatusCode: rule.StatusCode,
				}
			}
		}
		c.JSON(http.StatusOK, detail)
	})

	router.GET("/requests/:id", func(c *gin.Context) {
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			c.String(http.StatusBadRequest, "invalid request id")
			return
		}

		req, err := getRequest(db, id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				c.String(http.StatusNotFound, "request not found")
				return
			}
			c.String(http.StatusInternalServerError, "failed to load request")
			return
		}

		var rule *Rule
		if req.RuleID.Valid {
			ruleRow, err := getRule(db, req.RuleID.Int64)
			if err == nil {
				rule = &ruleRow
			}
		}

		c.HTML(http.StatusOK, "request.html", RequestView{Request: req, Rule: rule})
	})

	router.GET("/rules", func(c *gin.Context) {
		rules, err := listRules(db)
		if err != nil {
			c.String(http.StatusInternalServerError, "failed to load rules")
			return
		}
		c.HTML(http.StatusOK, "rules.html", RulesView{Rules: rules, DefaultStatus: cfg.DefaultStatus, DefaultBody: cfg.DefaultBody})
	})

	router.GET("/rules/:id/edit", func(c *gin.Context) {
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			c.String(http.StatusBadRequest, "invalid rule id")
			return
		}

		rule, err := getRule(db, id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				c.String(http.StatusNotFound, "rule not found")
				return
			}
			c.String(http.StatusInternalServerError, "failed to load rule")
			return
		}

		c.HTML(http.StatusOK, "rule_edit.html", RuleEditView{Rule: rule, DefaultStatus: cfg.DefaultStatus, DefaultBody: cfg.DefaultBody})
	})

	router.POST("/rules", func(c *gin.Context) {
		input, err := parseRuleForm(c, cfg)
		if err != nil {
			c.String(http.StatusBadRequest, err.Error())
			return
		}
		if err := createRule(db, input); err != nil {
			c.String(http.StatusBadRequest, fmt.Sprintf("failed to create rule: %v", err))
			return
		}
		c.Redirect(http.StatusFound, "/rules")
	})

	router.POST("/rules/:id/update", func(c *gin.Context) {
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			c.String(http.StatusBadRequest, "invalid rule id")
			return
		}
		input, err := parseRuleForm(c, cfg)
		if err != nil {
			c.String(http.StatusBadRequest, err.Error())
			return
		}
		input.ID = id
		if err := updateRule(db, input); err != nil {
			c.String(http.StatusBadRequest, fmt.Sprintf("failed to update rule: %v", err))
			return
		}
		c.Redirect(http.StatusFound, "/rules")
	})

	router.POST("/rules/:id/delete", func(c *gin.Context) {
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			c.String(http.StatusBadRequest, "invalid rule id")
			return
		}
		if err := deleteRule(db, id); err != nil {
			c.String(http.StatusInternalServerError, "failed to delete rule")
			return
		}
		c.Redirect(http.StatusFound, "/rules")
	})

	router.GET("/settings", func(c *gin.Context) {
		paths, err := listRequestPaths(db)
		if err != nil {
			c.String(http.StatusInternalServerError, "failed to load paths")
			return
		}
		c.HTML(http.StatusOK, "settings.html", SettingsView{Paths: paths})
	})

	router.POST("/settings/delete", func(c *gin.Context) {
		rawPath := strings.TrimSpace(c.PostForm("path"))
		if rawPath == "" {
			c.String(http.StatusBadRequest, "path is required")
			return
		}
		pathValue := normalizePath(rawPath)
		if err := deleteRequestsByPath(db, pathValue); err != nil {
			c.String(http.StatusInternalServerError, "failed to delete requests")
			return
		}
		c.Redirect(http.StatusFound, "/settings")
	})

	router.GET("/ws/requests", func(c *gin.Context) {
		conn, err := wsUpgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			log.Printf("websocket upgrade failed: %v", err)
			return
		}
		hub.add(conn)
	})

	router.Any("/hook/*path", func(c *gin.Context) {
		handleHook(c, db, cfg, hub)
	})

	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	log.Printf("listening on %s", cfg.Addr)
	if err := router.Run(cfg.Addr); err != nil {
		log.Fatal(err)
	}
}

func loadConfig() Config {
	data, err := os.ReadFile("config.yml")
	if err != nil {
		log.Fatalf("read config.yml: %v", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("parse config.yml: %v", err)
	}

	if strings.TrimSpace(cfg.Addr) == "" {
		cfg.Addr = ":8080"
	}
	if strings.TrimSpace(cfg.DSN) == "" {
		log.Fatal("db_dsn is required in config.yml")
	}
	if cfg.DefaultStatus == 0 {
		cfg.DefaultStatus = 200
	}
	if strings.TrimSpace(cfg.DefaultBody) == "" {
		cfg.DefaultBody = defaultResponseBody
	}

	return cfg
}

func openDB(dsn string) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}
	return db, nil
}

func ensureSchema(db *sql.DB) error {
	ruleTable := `
		CREATE TABLE IF NOT EXISTS rules (
			id BIGINT AUTO_INCREMENT PRIMARY KEY,
			method VARCHAR(16) NOT NULL,
			path VARCHAR(255) NOT NULL,
			status_code INT NOT NULL,
			response_body TEXT NOT NULL,
			response_headers JSON NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			UNIQUE KEY uniq_method_path (method, path)
		);`

	requestTable := `
		CREATE TABLE IF NOT EXISTS requests (
			id BIGINT AUTO_INCREMENT PRIMARY KEY,
			rule_id BIGINT NULL,
			method VARCHAR(16) NOT NULL,
			path VARCHAR(255) NOT NULL,
			query_json JSON NULL,
			headers_json JSON NULL,
			body_text MEDIUMTEXT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			CONSTRAINT fk_requests_rule_id FOREIGN KEY (rule_id) REFERENCES rules(id) ON DELETE SET NULL
		);`

	if _, err := db.Exec(ruleTable); err != nil {
		return err
	}
	if _, err := db.Exec(requestTable); err != nil {
		return err
	}
	if err := createIndexIfNeeded(db, "requests", "idx_requests_path", "path"); err != nil {
		return err
	}
	return nil
}

func listRequests(db *sql.DB, limit int, pathFilter, startTime, endTime string) ([]RequestLog, error) {
	query := `SELECT id, method, path, rule_id, created_at FROM requests WHERE 1=1`
	var args []interface{}

	if pathFilter != "" {
		query += " AND path = ?"
		args = append(args, pathFilter)
	}
	if startTime != "" {
		query += " AND created_at >= ?"
		args = append(args, startTime)
	}
	if endTime != "" {
		query += " AND created_at <= ?"
		args = append(args, endTime)
	}

	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []RequestLog
	for rows.Next() {
		var logRow RequestLog
		if err := rows.Scan(&logRow.ID, &logRow.Method, &logRow.Path, &logRow.RuleID, &logRow.CreatedAt); err != nil {
			return nil, err
		}
		logs = append(logs, logRow)
	}
	return logs, rows.Err()
}

func summarizeRequests(requests []RequestLog) []RequestSummary {
	summaries := make([]RequestSummary, 0, len(requests))
	for _, request := range requests {
		summaries = append(summaries, buildRequestSummary(request))
	}
	return summaries
}

func buildRequestSummary(request RequestLog) RequestSummary {
	var ruleID *int64
	if request.RuleID.Valid {
		value := request.RuleID.Int64
		ruleID = &value
	}
	createdAt := request.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	return RequestSummary{
		ID:        request.ID,
		Method:    request.Method,
		Path:      request.Path,
		RuleID:    ruleID,
		CreatedAt: formatTime(createdAt),
	}
}

func buildRequestDetail(request RequestLog) RequestDetail {
	var ruleID *int64
	if request.RuleID.Valid {
		value := request.RuleID.Int64
		ruleID = &value
	}
	queryJSON := ""
	if request.QueryJSON.Valid {
		queryJSON = request.QueryJSON.String
	}
	headersJSON := ""
	if request.HeadersJSON.Valid {
		headersJSON = request.HeadersJSON.String
	}
	bodyText := ""
	if request.BodyText.Valid {
		bodyText = request.BodyText.String
	}
	return RequestDetail{
		ID:          request.ID,
		Method:      request.Method,
		Path:        request.Path,
		RuleID:      ruleID,
		QueryJSON:   queryJSON,
		HeadersJSON: headersJSON,
		BodyText:    bodyText,
		CreatedAt:   formatTime(request.CreatedAt),
	}
}

func listRequestPaths(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT path FROM requests ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var paths []string
	seen := make(map[string]bool)
	for rows.Next() {
		var pathValue string
		if err := rows.Scan(&pathValue); err != nil {
			return nil, err
		}
		pathValue = normalizePath(pathValue)
		if !seen[pathValue] {
			paths = append(paths, pathValue)
			seen[pathValue] = true
		}
	}

	return paths, rows.Err()
}

func getRequest(db *sql.DB, id int64) (RequestLog, error) {
	var logRow RequestLog
	row := db.QueryRow(`SELECT id, method, path, rule_id, query_json, headers_json, body_text, created_at FROM requests WHERE id = ?`, id)
	if err := row.Scan(&logRow.ID, &logRow.Method, &logRow.Path, &logRow.RuleID, &logRow.QueryJSON, &logRow.HeadersJSON, &logRow.BodyText, &logRow.CreatedAt); err != nil {
		return RequestLog{}, err
	}
	return logRow, nil
}

func listRules(db *sql.DB) ([]Rule, error) {
	rows, err := db.Query(`SELECT id, method, path, status_code, response_body, response_headers, created_at, updated_at FROM rules ORDER BY method, path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []Rule
	for rows.Next() {
		var rule Rule
		if err := rows.Scan(&rule.ID, &rule.Method, &rule.Path, &rule.StatusCode, &rule.ResponseBody, &rule.ResponseHeaders, &rule.CreatedAt, &rule.UpdatedAt); err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}
	return rules, rows.Err()
}

func getRule(db *sql.DB, id int64) (Rule, error) {
	var rule Rule
	row := db.QueryRow(`SELECT id, method, path, status_code, response_body, response_headers, created_at, updated_at FROM rules WHERE id = ?`, id)
	if err := row.Scan(&rule.ID, &rule.Method, &rule.Path, &rule.StatusCode, &rule.ResponseBody, &rule.ResponseHeaders, &rule.CreatedAt, &rule.UpdatedAt); err != nil {
		return Rule{}, err
	}
	return rule, nil
}

func findRule(db *sql.DB, method, path string) (Rule, error) {
	var rule Rule
	row := db.QueryRow(`SELECT id, method, path, status_code, response_body, response_headers, created_at, updated_at FROM rules WHERE method = ? AND path = ?`, method, path)
	if err := row.Scan(&rule.ID, &rule.Method, &rule.Path, &rule.StatusCode, &rule.ResponseBody, &rule.ResponseHeaders, &rule.CreatedAt, &rule.UpdatedAt); err != nil {
		return Rule{}, err
	}
	return rule, nil
}

func createRule(db *sql.DB, rule Rule) error {
	_, err := db.Exec(`INSERT INTO rules (method, path, status_code, response_body, response_headers) VALUES (?, ?, ?, ?, ?)`,
		rule.Method, rule.Path, rule.StatusCode, rule.ResponseBody, nullableString(rule.ResponseHeaders))
	return err
}

func updateRule(db *sql.DB, rule Rule) error {
	_, err := db.Exec(`UPDATE rules SET method = ?, path = ?, status_code = ?, response_body = ?, response_headers = ? WHERE id = ?`,
		rule.Method, rule.Path, rule.StatusCode, rule.ResponseBody, nullableString(rule.ResponseHeaders), rule.ID)
	return err
}

func deleteRule(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM rules WHERE id = ?`, id)
	return err
}

func insertRequest(db *sql.DB, logRow RequestLog) (int64, error) {
	result, err := db.Exec(`INSERT INTO requests (rule_id, method, path, query_json, headers_json, body_text) VALUES (?, ?, ?, ?, ?, ?)`,
		nullableInt64(logRow.RuleID), logRow.Method, logRow.Path, nullableString(logRow.QueryJSON), nullableString(logRow.HeadersJSON), nullableString(logRow.BodyText))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func deleteRequestsByPath(db *sql.DB, pathValue string) error {
	_, err := db.Exec(`DELETE FROM requests WHERE path = ?`, pathValue)
	return err
}

func createIndexIfNeeded(db *sql.DB, tableName, indexName, column string) error {
	query := fmt.Sprintf("CREATE INDEX %s ON %s (%s)", indexName, tableName, column)
	_, err := db.Exec(query)
	if err == nil {
		return nil
	}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1061 {
		return nil
	}
	return err
}

func handleHook(c *gin.Context, db *sql.DB, cfg Config, hub *requestHub) {
	method := strings.ToUpper(c.Request.Method)
	pathValue := normalizePath(c.Param("path"))

	bodyBytes, _ := c.GetRawData()
	bodyText := string(bodyBytes)

	queryJSON := toJSON(c.Request.URL.Query())
	headersJSON := toJSON(c.Request.Header)

	var matchedRule *Rule
	rule, err := findRule(db, method, pathValue)
	if err == nil {
		matchedRule = &rule
	}

	requestLog := RequestLog{
		Method:      method,
		Path:        pathValue,
		RuleID:      sql.NullInt64{},
		QueryJSON:   sql.NullString{String: queryJSON, Valid: queryJSON != ""},
		HeadersJSON: sql.NullString{String: headersJSON, Valid: headersJSON != ""},
		BodyText:    sql.NullString{String: bodyText, Valid: bodyText != ""},
		CreatedAt:   time.Now(),
	}
	if matchedRule != nil {
		requestLog.RuleID = sql.NullInt64{Int64: matchedRule.ID, Valid: true}
	}

	id, err := insertRequest(db, requestLog)
	if err != nil {
		log.Printf("failed to log request: %v", err)
	} else {
		requestLog.ID = id
		if hub != nil {
			hub.broadcast(buildRequestSummary(requestLog))
		}
	}

	statusCode := cfg.DefaultStatus
	responseBody := cfg.DefaultBody

	if matchedRule != nil {
		if matchedRule.StatusCode != 0 {
			statusCode = matchedRule.StatusCode
		}
		if strings.TrimSpace(matchedRule.ResponseBody) != "" {
			responseBody = matchedRule.ResponseBody
		}
		if matchedRule.ResponseHeaders.Valid && strings.TrimSpace(matchedRule.ResponseHeaders.String) != "" {
			if err := applyHeadersJSON(c, matchedRule.ResponseHeaders.String); err != nil {
				log.Printf("invalid response headers for rule %d: %v", matchedRule.ID, err)
			}
		}
	}

	if matchedRule == nil {
		c.Header("Content-Type", "application/json")
	}

	c.String(statusCode, responseBody)
}

func parseRuleForm(c *gin.Context, cfg Config) (Rule, error) {
	method := strings.ToUpper(strings.TrimSpace(c.PostForm("method")))
	if method == "" {
		return Rule{}, errors.New("method is required")
	}

	pathInput := strings.TrimSpace(c.PostForm("path"))
	if pathInput == "" {
		return Rule{}, errors.New("path is required")
	}
	pathValue := normalizePath(pathInput)

	statusText := strings.TrimSpace(c.PostForm("status_code"))
	statusCode := cfg.DefaultStatus
	if statusText != "" {
		parsed, err := strconv.Atoi(statusText)
		if err != nil {
			return Rule{}, errors.New("status code must be a number")
		}
		statusCode = parsed
	}

	body := c.PostForm("response_body")
	if strings.TrimSpace(body) == "" {
		body = cfg.DefaultBody
	}

	headersInput := strings.TrimSpace(c.PostForm("response_headers"))
	headersValue := sql.NullString{}
	if headersInput != "" {
		if err := validateHeadersJSON(headersInput); err != nil {
			return Rule{}, errors.New("response headers must be valid JSON")
		}
		headersValue = sql.NullString{String: headersInput, Valid: true}
	}

	return Rule{
		Method:          method,
		Path:            pathValue,
		StatusCode:      statusCode,
		ResponseBody:    body,
		ResponseHeaders: headersValue,
	}, nil
}

func validateHeadersJSON(raw string) error {
	var payload map[string]interface{}
	return json.Unmarshal([]byte(raw), &payload)
}

func applyHeadersJSON(c *gin.Context, raw string) error {
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return err
	}
	for key, value := range payload {
		switch typed := value.(type) {
		case string:
			c.Header(key, typed)
		case []interface{}:
			for _, item := range typed {
				c.Writer.Header().Add(key, fmt.Sprint(item))
			}
		default:
			c.Header(key, fmt.Sprint(typed))
		}
	}
	return nil
}

func toJSON(value interface{}) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}

func prettyJSON(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	var payload interface{}
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		return raw
	}
	formatted, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return raw
	}
	return string(formatted)
}

func formatTime(value time.Time) string {
	return value.Local().Format("2006-01-02 15:04:05")
}

func normalizePath(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "/"
	}
	if !strings.HasPrefix(trimmed, "/") {
		trimmed = "/" + trimmed
	}
	return path.Clean(trimmed)
}

func nullableString(value sql.NullString) interface{} {
	if value.Valid {
		return value.String
	}
	return nil
}

func nullableInt64(value sql.NullInt64) interface{} {
	if value.Valid {
		return value.Int64
	}
	return nil
}

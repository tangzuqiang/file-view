package main

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed web/* web/vendor/*
var webFiles embed.FS

type Config struct {
	RootDir     string
	Port        int
	Username    string
	Password    string
	BasePath    string
	PreviewPort int
}

type Server struct {
	cfg      Config
	root     string
	base     string
	db       *sql.DB
	sessions map[string]Session
	previews map[string]time.Time
	mu       sync.Mutex
	index    *template.Template
	login    *template.Template
}

type Session struct {
	UserID  int64
	Expires time.Time
}
type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	IsAdmin   bool   `json:"is_admin"`
	CanUpload bool   `json:"can_upload"`
	CreatedAt string `json:"created_at"`
}
type contextKey string

const userContextKey contextKey = "user"

type FileItem struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	IsDir   bool      `json:"is_dir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
	Ext     string    `json:"ext"`
}

func main() {
	logFile := setupLogFile()
	if logFile != nil {
		defer logFile.Close()
		log.SetOutput(logFile)
	}
	if err := run(); err != nil {
		log.Printf("启动失败: %v", err)
		// Windows 双击启动时保留控制台，让用户能够看到具体错误。
		if info, statErr := os.Stdin.Stat(); statErr == nil && info.Mode()&os.ModeCharDevice != 0 {
			fmt.Print("按回车键退出...")
			_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		}
		os.Exit(1)
	}
}

// GUI 子系统编译后没有控制台，日志写到 EXE 同目录。
func setupLogFile() *os.File {
	dir := "."
	if executable, err := os.Executable(); err == nil {
		dir = filepath.Dir(executable)
	}
	f, err := os.OpenFile(filepath.Join(dir, "file-view.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil
	}
	return f
}

func run() error {
	cfg, exeDir, err := parseArgs(os.Args[1:])
	if err != nil {
		return err
	}
	if !filepath.IsAbs(cfg.RootDir) {
		cfg.RootDir = filepath.Join(exeDir, cfg.RootDir)
	}
	root, err := filepath.Abs(cfg.RootDir)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(root, 0755); err != nil {
		return fmt.Errorf("创建根目录失败: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	indexBytes, _ := webFiles.ReadFile("web/index.html")
	loginBytes, _ := webFiles.ReadFile("web/login.html")
	dbPath := filepath.Join(exeDir, "file-view.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("打开用户数据库失败: %w", err)
	}
	defer db.Close()
	if err = initDatabase(db, cfg); err != nil {
		return err
	}
	s := &Server{cfg: cfg, root: root, base: cleanBase(cfg.BasePath), db: db, sessions: map[string]Session{}, previews: map[string]time.Time{}}
	// 使用非默认分隔符，避免与 Vue 的 {{ ... }} 插值语法冲突。
	s.index = template.Must(template.New("index").Delims("[[", "]]").Parse(string(indexBytes)))
	s.login = template.Must(template.New("login").Delims("[[", "]]").Parse(string(loginBytes)))
	mux := http.NewServeMux()
	mux.Handle(s.base+"/assets/", http.StripPrefix(s.base+"/assets/", http.FileServer(http.FS(mustSub(webFiles, "web/vendor")))))
	mux.HandleFunc(s.base+"/login", s.loginHandler)
	mux.HandleFunc(s.base+"/logout", s.logoutHandler)
	mux.HandleFunc(s.base+"/api/list", s.auth(s.listHandler))
	mux.HandleFunc(s.base+"/api/upload", s.requireUpload(s.uploadHandler))
	mux.HandleFunc(s.base+"/api/admin/users", s.requireAdmin(s.usersHandler))
	mux.HandleFunc(s.base+"/api/raw", s.auth(s.rawHandler))
	mux.HandleFunc(s.base+"/api/download", s.auth(s.downloadHandler))
	mux.HandleFunc(s.base+"/api/zip", s.auth(s.zipHandler))
	mux.HandleFunc(s.base+"/", s.auth(s.indexHandler))
	previewMux := http.NewServeMux()
	previewMux.HandleFunc(s.base+"/view/", s.viewHandler)
	mainAddr := fmt.Sprintf(":%d", cfg.Port)
	previewAddr := fmt.Sprintf(":%d", cfg.PreviewPort)
	mainListener, err := net.Listen("tcp", mainAddr)
	if err != nil {
		return err
	}
	defer mainListener.Close()
	previewListener, err := net.Listen("tcp", previewAddr)
	if err != nil {
		return fmt.Errorf("预览端口 %d 启动失败: %w", cfg.PreviewPort, err)
	}
	defer previewListener.Close()
	log.Printf("文件预览服务已启动: http://localhost:%d%s/ （根目录: %s，HTML 预览端口: %d）", cfg.Port, s.base, root, cfg.PreviewPort)
	errors := make(chan error, 2)
	go func() { errors <- http.Serve(mainListener, securityHeaders(mux)) }()
	go func() { errors <- http.Serve(previewListener, previewSecurityHeaders(previewMux)) }()
	return <-errors
}

func parseArgs(args []string) (Config, string, error) {
	executable, err := os.Executable()
	if err != nil {
		return Config{}, "", err
	}
	exeDir := filepath.Dir(executable)
	c := Config{Username: "admin"}
	flags := flag.NewFlagSet("file-view", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&c.Password, "admin", "", "管理员密码；首次启动可省略并自动生成，传入时会重置管理员密码")
	flags.IntVar(&c.Port, "port", 8080, "Web 服务端口")
	flags.IntVar(&c.PreviewPort, "preview-port", 0, "HTML 预览端口，默认主端口加 1")
	flags.StringVar(&c.RootDir, "root", "D:/share", "共享文件根目录")
	flags.StringVar(&c.BasePath, "base-path", "", "可选访问路径前缀")
	if err = flags.Parse(args); err != nil {
		return c, exeDir, fmt.Errorf("启动参数错误: %w", err)
	}
	if c.RootDir == "" {
		return c, exeDir, errors.New("root 不能为空")
	}
	if c.Port < 1 || c.Port > 65535 {
		return c, exeDir, errors.New("port 必须在 1-65535 之间")
	}
	if c.PreviewPort == 0 {
		if c.Port == 65535 {
			return c, exeDir, errors.New("port 为 65535 时必须设置 preview-port")
		}
		c.PreviewPort = c.Port + 1
	}
	if c.PreviewPort < 1 || c.PreviewPort > 65535 || c.PreviewPort == c.Port {
		return c, exeDir, errors.New("preview-port 必须是与 port 不同的有效端口")
	}
	return c, exeDir, nil
}

func initDatabase(db *sql.DB, cfg Config) error {
	if _, err := db.Exec(`PRAGMA journal_mode=DELETE; PRAGMA busy_timeout=5000;
		CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT NOT NULL UNIQUE COLLATE NOCASE,
			password_salt TEXT NOT NULL,
			password_hash TEXT NOT NULL,
			is_admin INTEGER NOT NULL DEFAULT 0,
			can_upload INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`); err != nil {
		return fmt.Errorf("初始化用户数据库失败: %w", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE is_admin=1`).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		password := cfg.Password
		if password == "" {
			password = randomPassword(14)
			log.Printf("首次启动管理员：admin，密码：%s", password)
		}
		salt, hash, err := hashPassword(password)
		if err != nil {
			return err
		}
		if _, err = db.Exec(`INSERT INTO users(username,password_salt,password_hash,is_admin,can_upload) VALUES(?,?,?,?,?)`, cfg.Username, salt, hash, 1, 1); err != nil {
			return fmt.Errorf("创建初始管理员失败: %w", err)
		}
		log.Printf("已创建初始管理员账号: %s", cfg.Username)
	} else if cfg.Password != "" {
		salt, hash, err := hashPassword(cfg.Password)
		if err != nil {
			return err
		}
		var id int64
		if err = db.QueryRow(`SELECT id FROM users WHERE is_admin=1 ORDER BY id LIMIT 1`).Scan(&id); err != nil {
			return err
		}
		if _, err = db.Exec(`UPDATE users SET password_salt=?,password_hash=? WHERE id=?`, salt, hash, id); err != nil {
			return err
		}
		log.Printf("已通过 -admin 参数重置管理员密码")
	}
	return nil
}

func randomPassword(length int) string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
	b := make([]byte, length)
	random := make([]byte, length)
	_, _ = rand.Read(random)
	for i := range b {
		b[i] = alphabet[int(random[i])%len(alphabet)]
	}
	return string(b)
}

func hashPassword(password string) (string, string, error) {
	if len(password) < 6 {
		return "", "", errors.New("密码至少需要 6 个字符")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", "", err
	}
	hash := pbkdf2SHA256([]byte(password), salt, 210000, 32)
	return base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash), nil
}

func verifyPassword(password, saltText, hashText string) bool {
	salt, err1 := base64.RawStdEncoding.DecodeString(saltText)
	want, err2 := base64.RawStdEncoding.DecodeString(hashText)
	if err1 != nil || err2 != nil {
		return false
	}
	got := pbkdf2SHA256([]byte(password), salt, 210000, len(want))
	return hmac.Equal(got, want)
}

func pbkdf2SHA256(password, salt []byte, iterations, keyLen int) []byte {
	hLen := sha256.Size
	out := make([]byte, 0, keyLen)
	for block := uint32(1); len(out) < keyLen; block++ {
		mac := hmac.New(sha256.New, password)
		mac.Write(salt)
		var counter [4]byte
		binary.BigEndian.PutUint32(counter[:], block)
		mac.Write(counter[:])
		u := mac.Sum(nil)
		t := append([]byte(nil), u...)
		for i := 1; i < iterations; i++ {
			mac = hmac.New(sha256.New, password)
			mac.Write(u)
			u = mac.Sum(nil)
			for j := 0; j < hLen; j++ {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

func cleanBase(v string) string {
	v = "/" + strings.Trim(v, "/")
	if v == "/" {
		return ""
	}
	return v
}

func mustSub(fsys fs.FS, dir string) fs.FS {
	s, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return s
}

func (s *Server) indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != s.base+"/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	previewToken, err := s.newPreviewToken()
	if err != nil {
		http.Error(w, "无法创建预览令牌", http.StatusInternalServerError)
		return
	}
	user := currentUser(r)
	_ = s.index.Execute(w, map[string]any{"Base": s.base, "PreviewToken": previewToken, "PreviewPort": s.cfg.PreviewPort, "Username": user.Username, "IsAdmin": user.IsAdmin, "CanUpload": user.CanUpload})
}

func (s *Server) newPreviewToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	s.previews[token] = time.Now().Add(30 * time.Minute)
	now := time.Now()
	for k, expiry := range s.previews {
		if now.After(expiry) {
			delete(s.previews, k)
		}
	}
	s.mu.Unlock()
	return token, nil
}

func (s *Server) loginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if _, ok := s.sessionUser(r); ok {
			http.Redirect(w, r, s.base+"/", http.StatusSeeOther)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = s.login.Execute(w, map[string]any{"Base": s.base, "Error": r.URL.Query().Get("error") != ""})
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, s.base+"/login?error=1", http.StatusSeeOther)
		return
	}
	var id int64
	var salt, hash string
	err := s.db.QueryRow(`SELECT id,password_salt,password_hash FROM users WHERE username=?`, strings.TrimSpace(r.Form.Get("username"))).Scan(&id, &salt, &hash)
	if err != nil || !verifyPassword(r.Form.Get("password"), salt, hash) {
		http.Redirect(w, r, s.base+"/login?error=1", http.StatusSeeOther)
		return
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		http.Error(w, "无法创建会话", 500)
		return
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	s.mu.Lock()
	s.sessions[token] = Session{UserID: id, Expires: time.Now().Add(24 * time.Hour)}
	s.cleanupSessionsLocked()
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "fileview_session", Value: token, Path: s.base + "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 86400})
	http.Redirect(w, r, s.base+"/", http.StatusSeeOther)
}

func (s *Server) logoutHandler(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("fileview_session"); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "fileview_session", Path: s.base + "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, s.base+"/login", http.StatusSeeOther)
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := s.sessionUser(r)
		if !ok {
			http.Redirect(w, r, s.base+"/login", http.StatusSeeOther)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userContextKey, user)))
	}
}

func (s *Server) sessionUser(r *http.Request) (User, bool) {
	c, err := r.Cookie("fileview_session")
	if err != nil {
		return User{}, false
	}
	s.mu.Lock()
	session, ok := s.sessions[c.Value]
	if !ok || time.Now().After(session.Expires) {
		delete(s.sessions, c.Value)
		s.mu.Unlock()
		return User{}, false
	}
	s.mu.Unlock()
	var u User
	var admin, upload int
	err = s.db.QueryRow(`SELECT id,username,is_admin,can_upload,created_at FROM users WHERE id=?`, session.UserID).Scan(&u.ID, &u.Username, &admin, &upload, &u.CreatedAt)
	if err != nil {
		return User{}, false
	}
	u.IsAdmin = admin != 0
	u.CanUpload = upload != 0
	return u, true
}
func (s *Server) cleanupSessionsLocked() {
	now := time.Now()
	for k, v := range s.sessions {
		if now.After(v.Expires) {
			delete(s.sessions, k)
		}
	}
}

func currentUser(r *http.Request) User { u, _ := r.Context().Value(userContextKey).(User); return u }
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.auth(func(w http.ResponseWriter, r *http.Request) {
		if !currentUser(r).IsAdmin {
			jsonError(w, "需要管理员权限", http.StatusForbidden)
			return
		}
		next(w, r)
	})
}
func (s *Server) requireUpload(next http.HandlerFunc) http.HandlerFunc {
	return s.auth(func(w http.ResponseWriter, r *http.Request) {
		if !currentUser(r).CanUpload {
			jsonError(w, "当前账号没有上传权限", http.StatusForbidden)
			return
		}
		next(w, r)
	})
}

type userRequest struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	IsAdmin   bool   `json:"is_admin"`
	CanUpload bool   `json:"can_upload"`
}

func (s *Server) usersHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rows, err := s.db.Query(`SELECT id,username,is_admin,can_upload,created_at FROM users ORDER BY is_admin DESC, username`)
		if err != nil {
			jsonError(w, "读取用户失败", 500)
			return
		}
		defer rows.Close()
		users := []User{}
		for rows.Next() {
			var u User
			var a, p int
			if rows.Scan(&u.ID, &u.Username, &a, &p, &u.CreatedAt) == nil {
				u.IsAdmin = a != 0
				u.CanUpload = p != 0
				users = append(users, u)
			}
		}
		writeJSON(w, map[string]any{"users": users, "current_user_id": currentUser(r).ID})
	case http.MethodPost:
		var req userRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			jsonError(w, "请求格式错误", 400)
			return
		}
		req.Username = strings.TrimSpace(req.Username)
		if req.Username == "" {
			jsonError(w, "用户名不能为空", 400)
			return
		}
		salt, hash, err := hashPassword(req.Password)
		if err != nil {
			jsonError(w, err.Error(), 400)
			return
		}
		_, err = s.db.Exec(`INSERT INTO users(username,password_salt,password_hash,is_admin,can_upload) VALUES(?,?,?,?,?)`, req.Username, salt, hash, boolInt(req.IsAdmin), boolInt(req.CanUpload))
		if err != nil {
			jsonError(w, "用户名已存在或保存失败", 409)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	case http.MethodPut:
		var req userRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.ID <= 0 {
			jsonError(w, "请求格式错误", 400)
			return
		}
		req.Username = strings.TrimSpace(req.Username)
		if req.Username == "" {
			jsonError(w, "用户名不能为空", 400)
			return
		}
		var wasAdmin int
		if s.db.QueryRow(`SELECT is_admin FROM users WHERE id=?`, req.ID).Scan(&wasAdmin) != nil {
			jsonError(w, "用户不存在", 404)
			return
		}
		if wasAdmin != 0 && !req.IsAdmin {
			var admins int
			_ = s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE is_admin=1`).Scan(&admins)
			if admins <= 1 {
				jsonError(w, "至少需要保留一个管理员", 400)
				return
			}
		}
		var err error
		if req.Password != "" {
			var salt, hash string
			salt, hash, err = hashPassword(req.Password)
			if err == nil {
				_, err = s.db.Exec(`UPDATE users SET username=?,password_salt=?,password_hash=?,is_admin=?,can_upload=? WHERE id=?`, req.Username, salt, hash, boolInt(req.IsAdmin), boolInt(req.CanUpload), req.ID)
			}
		} else {
			_, err = s.db.Exec(`UPDATE users SET username=?,is_admin=?,can_upload=? WHERE id=?`, req.Username, boolInt(req.IsAdmin), boolInt(req.CanUpload), req.ID)
		}
		if err != nil {
			jsonError(w, "更新失败，用户名可能已存在", 409)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	case http.MethodDelete:
		id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
		if err != nil {
			jsonError(w, "用户 ID 无效", 400)
			return
		}
		if id == currentUser(r).ID {
			jsonError(w, "不能删除当前登录账号", 400)
			return
		}
		var admin int
		if s.db.QueryRow(`SELECT is_admin FROM users WHERE id=?`, id).Scan(&admin) != nil {
			jsonError(w, "用户不存在", 404)
			return
		}
		if admin != 0 {
			var count int
			_ = s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE is_admin=1`).Scan(&count)
			if count <= 1 {
				jsonError(w, "至少需要保留一个管理员", 400)
				return
			}
		}
		_, err = s.db.Exec(`DELETE FROM users WHERE id=?`, id)
		if err != nil {
			jsonError(w, "删除失败", 500)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	default:
		methodNotAllowed(w)
	}
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// resolve 将浏览器中的 UTF-8、正斜杠相对路径安全映射到 Windows 原生路径。
// filepath.Rel 检查和 EvalSymlinks 检查共同阻止 .. 及目录联接逃逸根目录。
func (s *Server) resolve(rel string, allowMissing bool) (string, error) {
	rel = strings.TrimPrefix(strings.ReplaceAll(rel, "\\", "/"), "/")
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." {
		clean = ""
	}
	if filepath.IsAbs(clean) {
		return "", errors.New("不允许绝对路径")
	}
	p := filepath.Join(s.root, clean)
	back, err := filepath.Rel(s.root, p)
	if err != nil || back == ".." || strings.HasPrefix(back, ".."+string(os.PathSeparator)) {
		return "", errors.New("路径越界")
	}
	check := p
	if allowMissing {
		check = filepath.Dir(p)
	}
	resolved, err := filepath.EvalSymlinks(check)
	if err != nil {
		return "", err
	}
	back, err = filepath.Rel(s.root, resolved)
	if err != nil || back == ".." || strings.HasPrefix(back, ".."+string(os.PathSeparator)) {
		return "", errors.New("路径越界")
	}
	return p, nil
}

func slashPath(p string) string { return filepath.ToSlash(p) }

func (s *Server) listHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	rel := r.URL.Query().Get("path")
	dir, err := s.resolve(rel, false)
	if err != nil {
		jsonError(w, "无效路径: "+err.Error(), 400)
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		jsonError(w, "无法读取目录", 404)
		return
	}
	items := make([]FileItem, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		itemRel := slashPath(filepath.Join(filepath.FromSlash(rel), e.Name()))
		items = append(items, FileItem{Name: e.Name(), Path: itemRel, IsDir: e.IsDir(), Size: info.Size(), ModTime: info.ModTime(), Ext: strings.ToLower(filepath.Ext(e.Name()))})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].IsDir != items[j].IsDir {
			return items[i].IsDir
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
	writeJSON(w, map[string]any{"path": slashPath(filepath.Clean(filepath.FromSlash(rel))), "items": items})
}

func (s *Server) uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	dir, err := s.resolve(r.URL.Query().Get("path"), false)
	if err != nil {
		jsonError(w, err.Error(), 400)
		return
	}
	reader, err := r.MultipartReader()
	if err != nil {
		jsonError(w, "上传格式错误", 400)
		return
	}
	count := 0
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			jsonError(w, "读取上传失败", 400)
			return
		}
		if part.FileName() == "" {
			part.Close()
			continue
		}
		name := filepath.Base(strings.ReplaceAll(part.FileName(), "\\", "/"))
		if name == "." || name == "" {
			part.Close()
			continue
		}
		target := filepath.Join(dir, name)
		if existing, statErr := os.Lstat(target); statErr == nil && existing.Mode()&os.ModeSymlink != 0 {
			part.Close()
			jsonError(w, "拒绝覆盖符号链接: "+name, 400)
			return
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			part.Close()
			jsonError(w, "无法保存 "+name, 500)
			return
		}
		_, cpErr := io.Copy(f, part)
		closeErr := f.Close()
		part.Close()
		if cpErr != nil || closeErr != nil {
			_ = os.Remove(target)
			jsonError(w, "保存失败: "+name, 500)
			return
		}
		count++
	}
	writeJSON(w, map[string]any{"ok": true, "count": count})
}

func (s *Server) rawHandler(w http.ResponseWriter, r *http.Request)      { s.serveFile(w, r, true) }
func (s *Server) downloadHandler(w http.ResponseWriter, r *http.Request) { s.serveFile(w, r, false) }

// viewHandler 保留文件的 URL 目录层级，使 HTML 内的相对图片、CSS 和 JS
// 能按 HTML 文件所在目录正确解析。
func (s *Server) viewHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w)
		return
	}
	prefix := s.base + "/view/"
	remainder := strings.TrimPrefix(r.URL.Path, prefix)
	parts := strings.SplitN(remainder, "/", 2)
	if remainder == r.URL.Path || len(parts) != 2 || !s.validPreviewToken(parts[0]) {
		http.Error(w, "预览链接无效或已过期，请从文件列表重新打开", http.StatusUnauthorized)
		return
	}
	rel := parts[1]
	if rel == "" {
		http.NotFound(w, r)
		return
	}
	s.serveResolvedFile(w, r, rel, true)
}

func (s *Server) validPreviewToken(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	expiry, ok := s.previews[token]
	if !ok || time.Now().After(expiry) {
		delete(s.previews, token)
		return false
	}
	return true
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, inline bool) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	s.serveResolvedFile(w, r, r.URL.Query().Get("path"), inline)
}

func (s *Server) serveResolvedFile(w http.ResponseWriter, r *http.Request, rel string, inline bool) {
	p, err := s.resolve(rel, false)
	if err != nil {
		http.Error(w, "无效路径", 400)
		return
	}
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	name := info.Name()
	disposition := "attachment"
	if inline {
		disposition = "inline"
	}
	w.Header().Set("Content-Disposition", disposition+`; filename="file"; filename*=UTF-8''`+url.PathEscape(name))
	ext := strings.ToLower(filepath.Ext(name))
	if ext == ".html" || ext == ".htm" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// HTML 需要直接渲染，但不应借助同源登录态操作文件 API。
		w.Header().Set("Content-Security-Policy", "sandbox allow-scripts allow-forms allow-popups allow-same-origin")
	} else if t := mime.TypeByExtension(ext); t != "" {
		w.Header().Set("Content-Type", t)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	f, err := os.Open(p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	// ServeContent 不会像 ServeFile 那样把 index.html 强制重定向到目录。
	http.ServeContent(w, r, name, info.ModTime(), f)
}

func (s *Server) zipHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	p, err := s.resolve(r.URL.Query().Get("path"), false)
	if err != nil {
		http.Error(w, "无效路径", 400)
		return
	}
	info, err := os.Stat(p)
	if err != nil || !info.IsDir() {
		http.NotFound(w, r)
		return
	}
	name := info.Name()
	if p == s.root {
		name = filepath.Base(s.root)
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="folder.zip"; filename*=UTF-8''`+url.PathEscape(name+".zip"))
	zw := zip.NewWriter(w)
	defer zw.Close()
	err = filepath.WalkDir(p, func(current string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil // 不跟随或打包可能指向共享根目录之外的链接。
		}
		rel, err := filepath.Rel(p, current)
		if err != nil || rel == "." {
			return err
		}
		zipName := filepath.ToSlash(filepath.Join(name, rel))
		if d.IsDir() {
			_, err = zw.Create(zipName + "/")
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		h, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		h.Name = zipName
		h.Method = zip.Deflate
		dst, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		src, err := os.Open(current)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(dst, src)
		closeErr := src.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		log.Printf("ZIP 生成失败: %v", err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
func methodNotAllowed(w http.ResponseWriter) {
	http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
}
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// HTML 预览运行在独立端口，必须允许主页面跨端口嵌入；文件访问仍由短期令牌保护。
func previewSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

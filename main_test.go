package main

import (
	"bytes"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveChinesePathAndTraversal(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "中文目录")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "报告 2026.txt")
	if err := os.WriteFile(file, []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	s := &Server{root: root}
	got, err := s.resolve("中文目录/报告 2026.txt", false)
	if err != nil {
		t.Fatalf("中文路径解析失败: %v", err)
	}
	if got != file {
		t.Fatalf("得到 %q，期望 %q", got, file)
	}
	if _, err = s.resolve("../secret.txt", false); err == nil {
		t.Fatal("路径穿越未被拒绝")
	}
}

func TestRenameAndDeleteHandlers(t *testing.T) {
	root := t.TempDir()
	oldDir := filepath.Join(root, "旧目录")
	if err := os.Mkdir(oldDir, 0755); err != nil {
		t.Fatal(err)
	}
	s := &Server{root: root}
	body := []byte(`{"path":"旧目录","name":"新目录"}`)
	r := httptest.NewRequest("POST", "/api/rename", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.renameHandler(w, r)
	if w.Code != 200 {
		t.Fatalf("重命名失败 %d: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "新目录")); err != nil {
		t.Fatalf("新目录不存在: %v", err)
	}
	oldFile := filepath.Join(root, "旧文件.txt")
	if err := os.WriteFile(oldFile, []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	body = []byte(`{"path":"旧文件.txt","name":"新文件.txt"}`)
	r = httptest.NewRequest("POST", "/api/rename", bytes.NewReader(body))
	w = httptest.NewRecorder()
	s.renameHandler(w, r)
	if w.Code != 200 {
		t.Fatalf("文件重命名失败 %d: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "新文件.txt")); err != nil {
		t.Fatalf("新文件不存在: %v", err)
	}
	r = httptest.NewRequest("DELETE", "/api/delete?path="+url.QueryEscape("新目录"), nil)
	w = httptest.NewRecorder()
	s.deleteHandler(w, r)
	if w.Code != 200 {
		t.Fatalf("删除失败 %d: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "新目录")); !os.IsNotExist(err) {
		t.Fatal("目录未被删除")
	}
	r = httptest.NewRequest("DELETE", "/api/delete?path=", nil)
	w = httptest.NewRecorder()
	s.deleteHandler(w, r)
	if w.Code != 400 {
		t.Fatalf("删除根目录应返回 400，实际 %d", w.Code)
	}
}

func TestViewRouteKeepsDirectoryForRelativeAssets(t *testing.T) {
	root := t.TempDir()
	assetDir := filepath.Join(root, "中文页面", "assets")
	if err := os.MkdirAll(assetDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assetDir, "图片.png"), []byte("png"), 0644); err != nil {
		t.Fatal(err)
	}
	s := &Server{root: root, previews: map[string]time.Time{"test-token": time.Now().Add(time.Minute)}}
	r := httptest.NewRequest("GET", "/view/test-token/%E4%B8%AD%E6%96%87%E9%A1%B5%E9%9D%A2/assets/%E5%9B%BE%E7%89%87.png", nil)
	w := httptest.NewRecorder()
	s.viewHandler(w, r)
	if w.Code != 200 {
		t.Fatalf("预览资源返回状态 %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "png" {
		t.Fatalf("预览资源内容错误: %q", w.Body.String())
	}
}

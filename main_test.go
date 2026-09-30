package main

import (
	"net/http/httptest"
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

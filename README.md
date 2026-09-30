# File View

面向 Windows 的单文件自托管文件浏览与预览服务。后端仅使用 Go 标准库，Vue、Tailwind Browser 和 Mammoth 均通过 `go:embed` 编译进 EXE，运行时不依赖网络。

## 使用

1. 编译无控制台窗口版本：`go build -trimpath -ldflags="-s -w -H=windowsgui" -o file-view.exe .`
2. 启动：`file-view.exe -admin=管理密码 -port=8080 -root=D:/share`
3. 打开 <http://localhost:8080>。

启动参数：

- `-admin=密码`：首次创建或重置管理员密码，至少 6 位。
- `-port=8080`：主服务端口，默认 `8080`。
- `-preview-port=8081`：HTML 隔离预览端口，默认使用主端口加 1。
- `-root=D:/share`：共享根目录，默认 `D:/share`。
- `-base-path=/files`：可选访问路径前缀。

会话保存在内存中，有效期 24 小时，服务重启后需重新登录。HTML 文件会在同源沙箱页面中直接渲染；`.docx` 由浏览器端 Mammoth 转换；`.doc` 会提示下载。

程序在后台运行，不显示 CMD 窗口。启动和错误信息记录在 EXE 同目录的 `file-view.log`；退出程序可在任务管理器中结束 `file-view.exe`。

首次启动会创建 `file-view.db` SQLite 用户数据库。如果没有传 `-admin`，程序会自动生成管理员密码并写入 `file-view.log`。之后可在网页的“用户管理”中维护账号。部署或备份时保留 `file-view.exe` 和 `file-view.db` 即可。

# 影库 yingku

自建类 Emby 媒体服务：Go 单二进制 + SQLite，直接扫描已整理的 STRM 媒体库
（NFO + NanShare 媒体信息 sidecar），提供网页浏览与媒体库管理；
播放经 NanShare 的 Emby 助手 302 直链 115 CDN，本服务只做 Emby 协议层。

## 运行

环境变量：

- `YINGKU_ADDR` 监听地址（默认 `:8097`）
- `YINGKU_DATA` SQLite 路径（默认 `./data/yingku.db`）
- `YINGKU_LIBRARY` 可选：首次启动时按「电影/电视剧」二级目录自动播种媒体库
- `YINGKU_FS_ROOT` 文件夹选择器可浏览范围（默认 `/`）

Docker：`docker build -t yingku .` 后以 host 网络或端口映射运行，
把媒体库目录只读挂载进容器、`/data` 挂持久卷。

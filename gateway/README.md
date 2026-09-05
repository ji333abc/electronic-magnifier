# 玩客云统一视频与镜头控制

本目录包含一个面向 ARMv7 玩客云的轻量 Go 中控服务。它把 go2rtc 的低延迟 WebRTC 画面、IPC 快照和 ESP32-C3 镜头电机控制合并到同一个需要登录的响应式监控网页中。默认配置为对焦、变焦两路电机，控制和状态接口可扩展到 8 路，并已预留限位输入、位置反馈和自动对焦能力标识。

## 组件

- `cmd/lens-gateway`：可交叉编译的单文件中控服务。
- `internal/gateway/web`：响应式控制台页面。
- `config.example.json`：不含密钥的设备、码流和电机配置。
- `deploy/go2rtc.yaml.example`：只监听本机管理端口的 go2rtc 配置。
- `deploy/*.service`：玩客云 systemd 服务。
- `deploy/install.sh`：安装 ARMv7 中控、校验并安装 go2rtc v1.9.14。
- `deploy/install-recorder.sh`：安装 MediaMTX 录像服务、36 小时循环录像和网页回放。

## 1. 准备 IPC

1. 将 IPC 地址设为 `192.168.1.122`，修改默认管理员密码。
2. 主码流和子码流都设为 H.264，关闭 B 帧；主码流建议 1080p、15–25fps、2–4Mbps、GOP 约 1 秒。
3. 确认以下地址能通过 VLC 播放：
   - `rtsp://用户名:密码@192.168.1.122:554/mpeg4`
   - `rtsp://用户名:密码@192.168.1.122:554/mpeg4cif`
4. 确认 `http://192.168.1.122/webcapture.jpg?command=snap&channel=0` 能返回 JPEG 快照；设备接口不同时修改 `ipcSnapshotPath`。

不要把 IPC 的 80、554 或 8091 端口映射到公网。

## 2. 配置并刷写 ESP32-C3

在 `ESP32C3` 目录执行 `idf.py menuconfig`。进入 `Lens motor controller`，设置 Wi-Fi、静态地址和一个随机 API 密钥。该密钥必须与玩客云 `ESP_API_KEY` 完全一致。联调完成后关闭旧版无鉴权接口，再重新执行 `idf.py build` 和刷写。

ESP32 的 Wi-Fi 密码与 API 密钥保存在已忽略的 `sdkconfig`，不再写进源码。

## 3. 通过 GitHub Actions 构建 ARMv7 中控

本项目以 GitHub Actions 作为唯一构建验证环境。修改 `gateway/**` 或构建工作流后推送到 GitHub，`Build gateway ARMv7` 会自动执行：

1. Go 单元测试。
2. `go vet` 静态检查。
3. Linux ARMv7 交叉编译。
4. 生成原始二进制、`.tar.gz` 压缩包和 SHA-256 校验文件。
5. 主分支构建成功后，以 `build-<运行序号>` 创建 GitHub 预发布版并上传上述文件。

在仓库的 **Releases** 页面下载最新的 `lens-gateway-armv7.tar.gz` 和 `lens-gateway-armv7.sha256`。不需要在开发电脑上安装 Go 或执行本地交叉编译。

## 4. 安装到玩客云

把整个 `gateway` 目录复制到玩客云，在其中执行：

```sh
sudo sh deploy/install.sh
```

安装器优先使用 `gateway/go2rtc_linux_arm` 离线文件；没有离线文件时会强制 HTTP/1.1、断点重试，并在 curl 失败后改用 wget。无论来源如何，只有 SHA-256 与官方 v1.9.14 发布文件一致时才会安装。网络不稳定时，可在电脑上下载 `go2rtc_linux_arm` 放到 `gateway` 目录后重新运行安装器。

安装器不会在密钥为空时启动服务。先生成登录密码哈希和随机密钥：

```sh
/usr/local/bin/lens-gateway -hash-password '换成你的网页密码'
openssl rand -hex 32
```

编辑 `/etc/lens-gateway/secrets.env`：

- `GATEWAY_ADMIN_PASSWORD_HASH`：上一步输出的 bcrypt 哈希。
- `GATEWAY_SESSION_SECRET`：一个随机 32 字节以上字符串。
- `ESP_API_KEY`：与 ESP32 menuconfig 相同的密钥。
- `IPC_USER`、`IPC_PASSWORD`：IPC 新账号密码；包含 `@`、`:` 等字符时需进行 URL 百分号编码。
- `LAN_IP`：玩客云局域网地址，默认 `192.168.1.120`。
- `TAILSCALE_IP`：执行 `tailscale ip -4` 得到的地址。

如实际设备地址不同，同时修改 `/etc/lens-gateway/config.json` 和 `/etc/lens-gateway/go2rtc.yaml`。随后启动：

```sh
sudo systemctl enable --now go2rtc lens-gateway
sudo systemctl status go2rtc lens-gateway
```

局域网访问 `http://192.168.1.120`。

## 5. Tailscale 与防火墙

让远程浏览器和玩客云加入同一个 tailnet，然后启用 HTTPS 反向代理：

```sh
sudo tailscale serve --https=443 http://127.0.0.1:80
```

WebRTC 媒体使用 TCP/UDP 8555。防火墙应只允许 `192.168.1.0/24` 和 `tailscale0` 访问 80、8555，不允许路由器做公网端口转发。使用 UFW 时可参考：

```sh
sudo ufw allow from 192.168.1.0/24 to any port 80 proto tcp
sudo ufw allow from 192.168.1.0/24 to any port 8555 proto tcp
sudo ufw allow from 192.168.1.0/24 to any port 8555 proto udp
sudo ufw allow in on tailscale0 to any port 8555 proto tcp
sudo ufw allow in on tailscale0 to any port 8555 proto udp
```

不要在尚未确认现有 SSH 防火墙规则前直接启用 UFW，以免锁住玩客云。

## 6. 验证

录像回放通过中控将 MediaMTX 动态生成的 MP4 暂存到 `/var/tmp`，再提供 HTTP 字节范围读取，支持播放器原生进度条向前、向后拖动。首次打开片段需要等待准备完成；同时最多暂存两个片段，每段上限 512 MiB，空闲两分钟后自动删除。该目录需有最多约 1 GiB 可用空间；临时文件只用于回放，不影响录像硬盘上的原始文件和 36 小时保留策略。

更新中控二进制并重启后，在网页选一个已完成的录像片段，依次拖到后半段和前半段，确认画面及播放时间随之变化。浏览器网络面板中的范围请求应返回 `206`、`Accept-Ranges: bytes` 和对应的 `Content-Range`。

```sh
curl http://127.0.0.1/api/health
journalctl -u go2rtc -u lens-gateway -f
```

网页登录后应看到 IPC、电机、视频三个状态点。按住点动按钮时，中控每 250ms 给 ESP32 续租；浏览器或网络断开后，Linux 在约 700ms 急停，ESP32 在约 800ms 释放线圈。不同用户可同时操作不同电机，同一电机只有一个短时控制者。

## 无限位自动精调

对焦电机卡片提供“一键自动精调”。该功能定位为人工粗调后的最后细调，不执行归零或全焦程扫描：玩客云通过 `ipcSnapshotPath` 读取 IPC 快照，对中央区域计算清晰度，并在当前位置最多 `±64` 半步内先粗后细搜索。每次移动后会等待镜片和新画面稳定，三张评分图也会跨越多个帧周期。最终阶段以 1 个半步为单位扫描，回到最佳点后会复拍确认；如发现回差导致清晰度下降，会继续向同一进给方向校正最多 3 个半步。精调期间独占对焦电机；再次点击按钮、点击停止、租约超时或服务退出都会取消搜索并释放线圈。

由于系统没有真实限位，达到搜索边界仍未找到峰值时会回到起始相对位置并提示先手动粗调。相对步数不能替代机械保护，传动机构仍应具备打滑或弹性缓冲能力。

## 开发验证

```sh
go test ./...
go vet ./...
```

本地运行需要设置 `GATEWAY_ADMIN_PASSWORD_HASH`、`GATEWAY_SESSION_SECRET`、`ESP_API_KEY`，再执行 `go run ./cmd/lens-gateway -config config.example.json`。

# 部署

## Docker

```bash
# 构建（注入版本号可选）
docker build -t janus:latest --build-arg VERSION=$(git describe --tags --always) .

# 运行：配置全走环境变量；上游 OpenCode 用 OPENCODE_URL 固定（容器内无法 /proc 自动发现）
docker run -d --name janus --restart unless-stopped \
  -p 2810:2810 \
  -v janus-data:/data \
  -e BRIDGE_API_KEY=sk-your-key \
  -e BRIDGE_DIRECTORY=/workspace \
  -e OPENCODE_URL=http://host.docker.internal:15327 \
  -e OPENCODE_PASSWORD=your-opencode-password \
  janus:latest
```

- 镜像基于 `distroless/static`（非 root、无 shell），二进制为纯静态。
- 持久化库写在 `/data`（`XDG_DATA_HOME`），建议挂卷。
- 也可挂配置文件：`-e JANUS_CONFIG=/etc/janus/janus.env -v $PWD/janus.env:/etc/janus/janus.env:ro`。
- 容器内访问宿主机的 OpenCode：Linux 上用 `--network host` 或宿主 IP；Docker Desktop 用 `host.docker.internal`。

## systemd

```bash
sudo install -d -o root -g root -m 0755 /etc/janus
sudo cp janus.env.example /etc/janus/janus.env
sudo chown root:root /etc/janus/janus.env && sudo chmod 600 /etc/janus/janus.env
sudo install -m 0755 janus /usr/local/bin/janus
sudo cp deploy/janus.service /etc/systemd/system/janus.service
sudo systemctl daemon-reload
sudo systemctl enable --now janus
```

关键点：

- 默认 **以 root 运行**。janus 需要读 OpenCode 凭据库（`/root/.local/share/opencode`，
  `/root` 是 700）并可能读写项目目录；用独立用户会在这些位置大量权限失败。
  若坚持非 root，需自行给该用户授予 OpenCode 数据目录与项目目录的读写权限。
- 数据目录 `/var/lib/janus` 由 unit 的 `StateDirectory=janus` 自动创建，
  `BRIDGE_DB` 固定指向其中。**不要设 `XDG_DATA_HOME`**，否则会把 OpenCode
  凭据库的默认路径也一起带偏。
- **`BRIDGE_DIRECTORY` 可选**：客户端请求会带 `X-OpenCode-Directory`（agent 工具也会传项目目录），
  不配时用进程 cwd。远程/网关部署可以完全不配。
- **`/v1/usage`（余额 / Go 限额）** 需要读 `/root` 下的 OpenCode 凭据库，因此要求以 root 运行；
  不用它可忽略。

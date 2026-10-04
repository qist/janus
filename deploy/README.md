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
sudo useradd --system --home /var/lib/janus --create-home janus
sudo install -d -o janus -g janus /etc/janus /var/lib/janus
sudo cp janus.env.example /etc/janus/janus.env
sudo chown root:janus /etc/janus/janus.env && sudo chmod 640 /etc/janus/janus.env
sudo install -m 0755 janus /usr/local/bin/janus
sudo cp deploy/janus.service /etc/systemd/system/janus.service
sudo systemctl daemon-reload
sudo systemctl enable --now janus
```

unit 默认使用 `JANUS_CONFIG=/etc/janus/janus.env`、数据目录 `/var/lib/janus`。

# syntax=docker/dockerfile:1

# ---------- 构建阶段：纯静态（CGO 关闭） ----------
FROM golang:1.27-alpine AS build
WORKDIR /src
# 先只用依赖文件，利用层缓存
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags "-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.Date=${DATE}" \
      -o /out/janus .

# ---------- 运行阶段：极小、非 root、无 shell ----------
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/janus /usr/local/bin/janus

# 持久化库（SQLite）落到挂载卷；配置走环境变量（见 janus.env.example）。
ENV XDG_DATA_HOME=/data \
    BRIDGE_ADDR=0.0.0.0:2810
VOLUME ["/data"]
EXPOSE 2810

# 也可以在运行时用 -e JANUS_CONFIG=/etc/janus/janus.env 挂载配置文件。
ENTRYPOINT ["/usr/local/bin/janus"]

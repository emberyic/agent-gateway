# 构建阶段
FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
ENV GOPROXY=https://goproxy.cn,direct
RUN go mod download
COPY . .
# 只构建网关二进制（根目录的 test_backend.go 是本地测试后端，不进镜像）
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/gateway ./cmd/gateway

# 运行阶段
FROM alpine:latest
RUN apk --no-cache add ca-certificates wget
WORKDIR /app
COPY --from=builder /out/gateway /app/gateway
# 配置文件一起进镜像，容器里可用 GATEWAY_CONFIG 换（compose 用的是 gateway.docker.yml）
COPY configs /app/configs
ENV GATEWAY_CONFIG=/app/configs/gateway.yml
EXPOSE 8080

# 存活检查走 /healthz：只报网关自身、不依赖后端，所以后端挂了也不会把网关标成不健康
# 要看后端状态用 /readyz（上游不健康时它返回 503）
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -q -O- http://127.0.0.1:8080/healthz >/dev/null || exit 1

ENTRYPOINT ["/app/gateway"]

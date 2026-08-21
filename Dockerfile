# syntax=docker/dockerfile:1

# 前端构建阶段：构建 Web 管理界面到 static/out
FROM node:24-alpine AS web-builder
WORKDIR /web
RUN corepack enable && corepack prepare pnpm@latest --activate
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ .
RUN pnpm build

# 后端编译阶段：编译含前端资源的 octopus 二进制
FROM golang:1.26 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web-builder /static/out ./static/out
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -tags=jsoniter -o /out/octopus .

# 运行阶段：轻量 alpine
FROM alpine:3.20
ENV TZ=Asia/Shanghai
RUN apk add --no-cache ca-certificates tzdata && rm -rf /var/cache/apk/*
WORKDIR /app
COPY --from=builder /out/octopus ./octopus
RUN chmod +x ./octopus
CMD ["./octopus", "start"]

# ---- 构建阶段 ----
FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/wms ./cmd/wms

# ---- 运行阶段 ----
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
ENV TZ=Asia/Shanghai
WORKDIR /app
COPY --from=builder /out/wms ./wms
COPY configs ./configs
COPY migrations ./migrations
EXPOSE 8080
ENTRYPOINT ["./wms"]

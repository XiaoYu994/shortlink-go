# 一个镜像同时放 redirector、console 和 migrate。
# 部署清单用命令选进程，默认是 redirector。
# migrate 从当前目录找 migrations/，所以工作目录固定在 /app。

FROM golang:1.27.3-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/redirector ./cmd/redirector \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/console ./cmd/console \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate

FROM alpine:3.22

RUN apk add --no-cache ca-certificates \
    && adduser -D -H -u 65532 nonroot
WORKDIR /app
COPY --from=build /out/redirector /out/console /out/migrate /usr/local/bin/
COPY migrations /app/migrations
USER nonroot
EXPOSE 8080 8081
CMD ["redirector"]

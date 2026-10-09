FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=secret,id=proxy_ca,target=/etc/ssl/certs/ca-certificates.crt \
    go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/lifeplan ./cmd/lifeplan && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/plan ./cmd/plan

FROM alpine:3.20
RUN --mount=type=secret,id=proxy_ca,target=/etc/ssl/cert.pem \
    apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 app && \
    mkdir -p /data/files && chown app /data/files
COPY --from=builder /out/ /usr/local/bin/
COPY --chmod=755 deploy/files-backup.sh deploy/files-backup-loop.sh /usr/local/bin/
USER app
# Выполняем бинарник именно в итоговом Alpine: init домена загружает Europe/Moscow.
# Без DATABASE_URL ожидаем обычную ошибку конфигурации, а не panic при старте.
RUN status=0; output="$(lifeplan 2>&1)" || status=$?; \
    test -r /usr/share/zoneinfo/Europe/Moscow && test "$status" -eq 1 && \
    printf '%s\n' "$output" | grep -q 'DATABASE_URL не задан'
ENTRYPOINT ["lifeplan"]
CMD ["serve"]

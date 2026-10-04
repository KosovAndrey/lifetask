FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/lifeplan ./cmd/lifeplan && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/plan ./cmd/plan

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 app && \
    mkdir -p /data/files && chown app /data/files
COPY --from=builder /out/ /usr/local/bin/
USER app
ENTRYPOINT ["lifeplan"]
CMD ["serve"]

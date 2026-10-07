# syntax=docker/dockerfile:1
ARG GO=1.27-alpine

# --- зависимости отдельно от кода: слой живёт в кэше между сборками
FROM golang:${GO} AS deps
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# --- тесты: сломанные тесты валят сборку
FROM deps AS test
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod go test ./...

# --- сборка: статический бинарник (CGO выкл → scratch)
FROM deps AS build
ARG GIT_REV=unknown
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.rev=${GIT_REV}" -o /out/vdl . && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/healthcheck ./cmd/healthcheck

# --- рантайм: scratch — два бинарника, UI и сертификаты; /data — состояние
FROM scratch
ARG GIT_REV=unknown
LABEL org.opencontainers.image.title="vdl" \
      org.opencontainers.image.description="Универсальный видео-загрузчик: cobalt-ядро + нативный twitter-fallback + куки-менеджер в Telegram" \
      org.opencontainers.image.source="https://github.com/0x3654/vdl" \
      org.opencontainers.image.revision="${GIT_REV}"
COPY --from=build /out/vdl /vdl
COPY --from=build /out/healthcheck /healthcheck
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=65534:65534 /tmp /tmp
USER 65534:65534
VOLUME /data
EXPOSE 8360
ENV PORT=8360 DATA_DIR=/data
HEALTHCHECK --interval=60s --timeout=5s --start-period=10s --retries=3 CMD ["/healthcheck"]
ENTRYPOINT ["/vdl"]

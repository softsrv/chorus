# ── Stage 1: Builder ─────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS builder

WORKDIR /build

# Download dependencies first (layer cache)
COPY go.mod go.sum ./
RUN go mod download

# Download Tailwind CSS standalone CLI (no Node/npm required)
RUN apk add --no-cache curl libstdc++ && \
    ARCH=$(uname -m) && \
    if [ "$ARCH" = "aarch64" ]; then \
        TW_ARCH="arm64-musl"; \
        TW_HASH="9c106e815ce7ea99a65f91c13be2c51f00388dd3c0127c7a13a38f76cd1e26aa"; \
    else \
        TW_ARCH="x64-musl"; \
        TW_HASH="d861210e7c772e3b8a8b302d51bd4c8e2ab2e6f5dcb7545b16da6e9676c99079"; \
    fi && \
    curl -fsSL "https://github.com/tailwindlabs/tailwindcss/releases/download/v4.3.0/tailwindcss-linux-${TW_ARCH}" \
         -o /usr/local/bin/tailwindcss && \
    echo "${TW_HASH}  /usr/local/bin/tailwindcss" | sha256sum -c && \
    chmod +x /usr/local/bin/tailwindcss

# Build Tailwind CSS
COPY web ./web
RUN /usr/local/bin/tailwindcss -i ./web/static/css/app.css \
                               -o ./web/static/css/dist/app.css \
                               --minify

# Compile Go binary. Web assets (including the CSS built above) are embedded
# via go:embed, so the runtime image needs only the binary.
# .dockerignore excludes web/static/css/dist, so this COPY can't clobber it.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
      -ldflags="-s -w" \
      -o /build/bin/app \
      ./cmd/app

# ── Stage 2: Runtime ─────────────────────────────────────────────────────────
FROM gcr.io/distroless/static:nonroot

WORKDIR /app

# distroless "nonroot" runs as uid 65532.
USER 65532:65532

COPY --from=builder /build/bin/app ./app

EXPOSE 8080

ENTRYPOINT ["./app"]

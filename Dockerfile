# ForgeSync controller image: forgesyncd with the admin UI built in, the
# forgesync CLI, and git (needed for replication).
#
#   docker build -t forgesync --build-arg VERSION=$(git describe --always) .

FROM node:24-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
# Vite writes straight into the Go package that embeds the UI.
RUN mkdir -p ../internal/webui/dist && npm run build

FROM golang:1.27.1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY --from=web /src/internal/webui/dist/ internal/webui/dist/
ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X scenegit.org/forgesync/internal/buildinfo.Version=${VERSION} -X scenegit.org/forgesync/internal/buildinfo.Commit=${COMMIT}" \
      -o /out/ ./cmd/...

FROM alpine:3.23
RUN apk add --no-cache git ca-certificates \
 && adduser -D -H -u 10001 forgesync \
 && mkdir -p /var/lib/forgesync /etc/forgesync \
 && chown forgesync /var/lib/forgesync
COPY --from=build /out/forgesyncd /out/forgesync /usr/local/bin/
# By number, so it means the same thing wherever the image runs: a host
# reading the filesystem has no /etc/passwd of ours to look the name up in.
USER 10001
EXPOSE 8090
# The same readiness the compose file and systemd watch: answering means
# the process is up and the database is reachable.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8090/readyz || exit 1
ENTRYPOINT ["forgesyncd", "-config", "/etc/forgesync/forgesync.yaml"]

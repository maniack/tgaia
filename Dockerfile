# syntax=docker/dockerfile:1
ARG GO_VERSION=1.27.3
ARG ALPINE_VERSION=3.22

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/tgaia ./cmd/tgaia

FROM alpine:${ALPINE_VERSION} AS runtime
RUN apk add --no-cache ca-certificates \
    && addgroup -g 10001 app \
    && adduser -D -H -u 10001 -G app app
WORKDIR /app
COPY --from=build /out/tgaia /usr/local/bin/tgaia
USER 10001:10001
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/tgaia"]
CMD ["--config", "/etc/tgaia/config.yaml", "serve"]

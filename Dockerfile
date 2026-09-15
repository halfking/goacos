# goacos — multi-arch image (linux/amd64 + linux/arm64).
# Go cross-compiles, so the build stage always runs on the builder's native
# platform (no QEMU) and only the final scratch-ish stage is per-arch.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/goacos .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -g 10001 goacos && adduser -D -u 10001 -G goacos goacos
COPY --from=build /out/goacos /usr/local/bin/goacos
USER goacos
EXPOSE 8848
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD goacos healthcheck 127.0.0.1:8848 || exit 1
ENTRYPOINT ["goacos"]
CMD ["serve"]
